package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dseif0x/agents-operator/internal/github"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
)

type fakeOrch struct{ notified []string }

func (f *fakeOrch) Notify(id string) { f.notified = append(f.notified, id) }
func (f *fakeOrch) PodLogs(context.Context, string, int64) ([]byte, error) {
	return []byte("logs"), nil
}

func newService(t *testing.T) (*Service, *fakeOrch, *store.User) {
	t.Helper()
	st := store.NewMemory()
	u := &store.User{Username: "alice", PasswordHash: "x"}
	_ = st.Users().Create(context.Background(), u)
	orch := &fakeOrch{}
	svc := &Service{
		Store: st, Orch: orch, Broker: NewBroker(),
		Creds:    &Credentials{Store: st, CS: fake.NewClientset(), Namespace: "agents-operator"},
		Defaults: Defaults{PVCSize: "20Gi", StorageClass: "nfs-fast", Autonomous: true},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return svc, orch, u
}

func TestCreateValidation(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		req  CreateRequest
	}{
		{"empty name", CreateRequest{Name: "", Agent: "claude"}},
		{"bad agent", CreateRequest{Name: "ok", Agent: "gemini"}},
		{"bad repo", CreateRequest{Name: "ok", Agent: "claude", RepoURL: "ftp://x/y"}},
		{"bad size", CreateRequest{Name: "ok", Agent: "claude", PVCSize: "lots"}},
		{"reserved env", CreateRequest{Name: "ok", Agent: "claude", Env: map[string]string{"RUNNER_TOKEN": "x"}}},
		{"bad env key", CreateRequest{Name: "ok", Agent: "claude", Env: map[string]string{"bad key": "x"}}},
		{"bad branch", CreateRequest{Name: "ok", Agent: "claude", RepoURL: "https://x/y.git", Branch: "-x"}},
		{"bad tag", CreateRequest{Name: "ok", Agent: "claude", ImageTag: "a/b"}},
		{"bad path", CreateRequest{Name: "ok", Agent: "claude", Repos: []store.Repo{{URL: "https://x/y.git", Path: "../etc"}}}},
		{"reserved path", CreateRequest{Name: "ok", Agent: "claude", Repos: []store.Repo{{URL: "https://x/y.git", Path: "home"}}}},
		{"duplicate path", CreateRequest{Name: "ok", Agent: "claude", RepoURL: "https://x/y.git", Repos: []store.Repo{{URL: "https://z/y.git"}}}},
	}
	for _, c := range cases {
		if _, err := svc.Create(ctx, u, c.req); err == nil {
			t.Errorf("%s: expected validation error", c.name)
		} else if _, ok := err.(*ValidationError); !ok {
			t.Errorf("%s: err type %T", c.name, err)
		}
	}
}

func TestLifecycle(t *testing.T) {
	svc, orch, u := newService(t)
	ctx := context.Background()
	events, cancel := svc.Broker.Subscribe(u.ID)
	defer cancel()

	sess, err := svc.Create(ctx, u, CreateRequest{Name: "one", Agent: "claude", RepoURL: "git@github.com:x/y.git"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.State != store.StateCreating || sess.PVCSize != "20Gi" || sess.StorageClass != "nfs-fast" || !sess.Autonomous {
		t.Fatalf("defaults not applied: %+v", sess)
	}
	if len(sess.Repos) != 1 || sess.Repos[0].Path != "y" || sess.Repos[0].URL != "git@github.com:x/y.git" {
		t.Fatalf("shorthand repo not normalised: %+v", sess.Repos)
	}
	multi, err := svc.Create(ctx, u, CreateRequest{Name: "multi", Agent: "shell", RepoURL: "https://github.com/a/app.git", Branch: "dev",
		Repos: []store.Repo{{URL: "https://github.com/a/lib/", Path: "shared-lib"}, {URL: "ssh://git@host/x/tools.git"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(multi.Repos) != 3 || multi.Repos[0].Path != "app" || multi.Repos[0].Branch != "dev" || multi.Repos[1].Path != "shared-lib" || multi.Repos[2].Path != "tools" {
		t.Fatalf("multi repos = %+v", multi.Repos)
	}
	for in, want := range map[string]string{"git@github.com:x/y.git": "y", "https://h/a/b": "b", "https://h/a/b.git/": "b", "weird": "weird", "": "repo"} {
		if got := RepoPathFromURL(in); got != want {
			t.Errorf("RepoPathFromURL(%q)=%q want %q", in, got, want)
		}
	}
	if len(orch.notified) < 1 || orch.notified[0] != sess.ID {
		t.Fatalf("orchestrator not notified: %v", orch.notified)
	}
	select {
	case ev := <-events:
		if ev.Type != "session" || ev.Session.State != store.StateCreating {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no SSE event")
	}

	// Ownership: another user cannot see it.
	other := &store.User{ID: "other", Username: "bob"}
	if _, err := svc.Get(ctx, other.ID, sess.ID); err != store.ErrNotFound {
		t.Fatalf("cross-user get err = %v", err)
	}
	if _, err := svc.Stop(ctx, other, sess.ID); err != store.ErrNotFound {
		t.Fatalf("cross-user stop err = %v", err)
	}

	// Transitions.
	if _, err := svc.Start(ctx, u, sess.ID); err != ErrInvalidTransition {
		t.Fatalf("start while creating err = %v", err)
	}
	if _, err := svc.Stop(ctx, u, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Stop(ctx, u, sess.ID); err != ErrInvalidTransition {
		t.Fatalf("stop while stopping err = %v", err)
	}
	if _, err := svc.Store.Sessions().SetState(ctx, sess.ID, store.StateStopped, ""); err != nil {
		t.Fatal(err)
	}
	started, err := svc.Start(ctx, u, sess.ID)
	if err != nil || started.State != store.StateCreating || started.Generation != 2 {
		t.Fatalf("start = %+v, %v", started, err)
	}
	if _, err := svc.Delete(ctx, u, sess.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, u.ID, sess.ID); got.State != store.StateDeleting {
		t.Fatalf("state = %s", got.State)
	}
	// Delete is idempotent.
	if _, err := svc.Delete(ctx, u, sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := svc.Events(ctx, u.ID, sess.ID)
	if err != nil || len(evs) < 4 {
		t.Fatalf("events = %d, %v", len(evs), err)
	}
	logs, err := svc.Logs(ctx, u.ID, sess.ID)
	if err != nil || string(logs) != "logs" {
		t.Fatalf("logs = %q %v", logs, err)
	}
	svc.SessionDeleted(ctx, sess.ID, u.ID)
	// Drain remaining events and expect the deletion at the end.
	var last Event
	for done := false; !done; {
		select {
		case ev := <-events:
			last = ev
		default:
			done = true
		}
	}
	if last.Type != "deleted" || last.ID != sess.ID {
		t.Fatalf("last event = %+v", last)
	}
}

func TestCredentials(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	if err := svc.Creds.Set(ctx, u.ID, store.CredAnthropicAPIKey, []byte("  sk-ant \n")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Creds.Set(ctx, u.ID, store.CredGitUserName, []byte("Alice")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Creds.Set(ctx, u.ID, "nope", []byte("x")); err == nil {
		t.Fatal("unknown kind accepted")
	}
	// A setup token copied off a phone screen arrives wrapped; whitespace
	// goes, the token stays whole. Something that is not a token is refused.
	if err := svc.Creds.Set(ctx, u.ID, store.CredClaudeOAuthToken, []byte("  sk-ant-oat01-abc\n  def \n ghi\n")); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Creds.Get(ctx, u.ID, store.CredClaudeOAuthToken); string(v) != "sk-ant-oat01-abcdefghi" {
		t.Fatalf("oauth token stored as %q", v)
	}
	if err := svc.Creds.Set(ctx, u.ID, store.CredClaudeOAuthToken, []byte("Store this token securely")); err == nil {
		t.Fatal("non-token accepted as an OAuth token")
	} else if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("err type %T", err)
	}
	if v, _ := svc.Creds.Get(ctx, u.ID, store.CredAnthropicAPIKey); string(v) != "sk-ant" {
		t.Fatalf("api key stored as %q", v)
	}
	list, err := svc.Creds.List(ctx, u.ID)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, i := range list {
		switch i.Kind {
		case store.CredAnthropicAPIKey:
			if !i.Secret || i.Value != "" {
				t.Fatalf("secret value leaked: %+v", i)
			}
		case store.CredGitUserName:
			if i.Secret || i.Value != "Alice" {
				t.Fatalf("plain value missing: %+v", i)
			}
		}
	}
	if err := svc.Creds.Delete(ctx, u.ID, store.CredAnthropicAPIKey); err != nil {
		t.Fatal(err)
	}
	list, _ = svc.Creds.List(ctx, u.ID)
	if len(list) != 2 {
		t.Fatalf("after delete = %d", len(list))
	}
}

func TestGitHubRepos(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	// No token: not configured, no error.
	repos, configured, err := svc.GitHubRepos(ctx, u.ID, false)
	if err != nil || configured || len(repos) != 0 {
		t.Fatalf("unconfigured = %v %v %v", repos, configured, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_x" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `[
			{"full_name":"me/zeta","clone_url":"https://github.com/me/zeta.git","default_branch":"main"},
			{"full_name":"me/Alpha","clone_url":"https://github.com/me/Alpha.git","default_branch":"main"},
			{"full_name":"org/used","clone_url":"https://github.com/org/used.git","default_branch":"dev","private":true}]`)
	}))
	defer srv.Close()
	svc.GitHub = &github.Client{BaseURL: srv.URL}
	if err := svc.Creds.Set(ctx, u.ID, store.CredGitHubToken, []byte("ghp_x")); err != nil {
		t.Fatal(err)
	}
	// Two sessions with org/used (one via git@ spelling), one deleted afterwards: usage persists.
	s1, err := svc.Create(ctx, u, CreateRequest{Name: "a", Agent: "shell", RepoURL: "git@github.com:org/used.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, u, CreateRequest{Name: "b", Agent: "shell", RepoURL: "https://github.com/org/used.git"}); err != nil {
		t.Fatal(err)
	}
	_ = svc.Store.Sessions().Delete(ctx, s1.ID)

	repos, configured, err = svc.GitHubRepos(ctx, u.ID, false)
	if err != nil || !configured {
		t.Fatalf("configured = %v err = %v", configured, err)
	}
	got := []string{}
	for _, r := range repos {
		got = append(got, fmt.Sprintf("%s:%d", r.FullName, r.Uses))
	}
	want := "org/used:2,me/Alpha:0,me/zeta:0"
	if strings.Join(got, ",") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
	// Bad token surfaces as a validation error, still "configured".
	_ = svc.Creds.Set(ctx, u.ID, store.CredGitHubToken, []byte("bad"))
	svc.GitHub = &github.Client{BaseURL: srv.URL}
	_, configured, err = svc.GitHubRepos(ctx, u.ID, true)
	if err == nil || !configured {
		t.Fatalf("bad token: configured=%v err=%v", configured, err)
	}
}

func TestViewAndBroker(t *testing.T) {
	svc, _, u := newService(t)
	sess, _ := svc.Create(context.Background(), u, CreateRequest{Name: "v", Agent: "shell"})
	v := svc.View(sess)
	if v.ID != sess.ID || v.PodName != "agents-operator-"+sess.ID || v.Env == nil || v.NeedsAttention {
		t.Fatalf("view = %+v", v)
	}
	b := NewBroker()
	ch1, c1 := b.Subscribe("a")
	defer c1()
	ch2, c2 := b.Subscribe("b")
	defer c2()
	b.Publish("a", Event{Type: "deleted", ID: "x"})
	select {
	case <-ch2:
		t.Fatal("event delivered to wrong owner")
	default:
	}
	select {
	case ev := <-ch1:
		if ev.ID != "x" {
			t.Fatal(ev)
		}
	default:
		t.Fatal("event not delivered")
	}
}

type staticResolver struct{ ep term.Endpoint }

func (r staticResolver) Endpoint(context.Context, string) (term.Endpoint, error) { return r.ep, nil }

// fakeRunner is enough of a runner for the poller: /status reports the
// credential file's change time, /ws answers export_login with a bundle
// whose content counts the exports.
type fakeRunner struct {
	mu      sync.Mutex
	loginAt *time.Time
	exports int
}

func (f *fakeRunner) setLoginAt(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginAt = &t
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exports
}

func (f *fakeRunner) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(runner.Status{Agent: "claude", Running: true, LoginUpdatedAt: f.loginAt})
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var msg runner.Control
		_ = json.Unmarshal(data, &msg)
		if msg.T != runner.MsgExportLogin {
			return
		}
		f.mu.Lock()
		f.exports++
		n := f.exports
		f.mu.Unlock()
		bundle, _ := runner.EncodeLoginBundle(runner.LoginBundle{Files: map[string][]byte{".claude/.credentials.json": []byte(fmt.Sprintf(`{"v":%d}`, n))}})
		reply, _ := json.Marshal(runner.Control{T: runner.MsgLogin, Kind: msg.Kind, Data: base64.StdEncoding.EncodeToString(bundle)})
		_ = c.Write(r.Context(), websocket.MessageText, reply)
		_, _, _ = c.Read(r.Context()) // until the hub closes
	})
	return mux
}

func TestSyncLogin(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	fr := &fakeRunner{}
	srv := httptest.NewServer(fr.handler())
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	svc.Term = &term.Proxy{Resolver: staticResolver{term.Endpoint{IP: host, Port: port, Token: "t"}}, Log: svc.Log}
	saved := func() string {
		v, err := svc.Creds.Get(ctx, u.ID, store.CredClaudeLogin)
		if err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return ""
		}
		return string(runner.DecodeLoginBundle(runner.LoginClaude, v).Files[".claude/.credentials.json"])
	}

	sess, err := svc.Create(ctx, u, CreateRequest{Name: "c", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.Sessions().SetState(ctx, sess.ID, store.StateRunning, ""); err != nil {
		t.Fatal(err)
	}

	// The CLI logged in, but the user never saved: nothing is stored.
	fr.setLoginAt(time.Now())
	svc.PollOnce(ctx)
	if fr.count() != 0 || saved() != "" {
		t.Fatalf("exported %d, saved %q for a user without a saved login", fr.count(), saved())
	}

	// The user saves. The account copy is newer than the file, so polling
	// does not export again.
	if err := svc.SaveLogin(ctx, u, sess.ID, store.CredClaudeLogin); err != nil {
		t.Fatal(err)
	}
	svc.PollOnce(ctx)
	if fr.count() != 1 || saved() != `{"v":1}` {
		t.Fatalf("after save: exports=%d saved=%q", fr.count(), saved())
	}

	// The CLI refreshes its tokens: the next poll re-exports, exactly once.
	fr.setLoginAt(time.Now().Add(time.Minute))
	svc.PollOnce(ctx)
	svc.PollOnce(ctx)
	if fr.count() != 2 || saved() != `{"v":2}` {
		t.Fatalf("after refresh: exports=%d saved=%q", fr.count(), saved())
	}
	events, _ := svc.Events(ctx, u.ID, sess.ID)
	found := false
	for _, e := range events {
		if e.Kind == "login" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no login event recorded: %+v", events)
	}

	// A shell session's runner is never asked for a login.
	sh, _ := svc.Create(ctx, u, CreateRequest{Name: "s", Agent: "shell"})
	_, _ = svc.Store.Sessions().SetState(ctx, sh.ID, store.StateRunning, "")
	fr.setLoginAt(time.Now().Add(2 * time.Minute))
	svc.PollOnce(ctx)
	if fr.count() != 3 { // the claude session exported the newer file once more; the shell one did not
		t.Fatalf("exports = %d", fr.count())
	}
}

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/egress"
	"github.com/dseif0x/agents-operator/internal/github"
	"github.com/dseif0x/agents-operator/internal/names"
	"github.com/dseif0x/agents-operator/internal/reconcile"
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
		{"bad runtime class", CreateRequest{Name: "ok", Agent: "claude", RuntimeClass: "gVisor!"}},
		{"bad selector key", CreateRequest{Name: "ok", Agent: "claude", NodeSelector: map[string]string{"bad key": "x"}}},
		{"bad selector value", CreateRequest{Name: "ok", Agent: "claude", NodeSelector: map[string]string{"k": "a b"}}},
		{"bad toleration operator", CreateRequest{Name: "ok", Agent: "claude", Tolerations: []config.Toleration{{Key: "k", Operator: "Like"}}}},
		{"exists with value", CreateRequest{Name: "ok", Agent: "claude", Tolerations: []config.Toleration{{Key: "k", Operator: "Exists", Value: "v"}}}},
		{"equal without key", CreateRequest{Name: "ok", Agent: "claude", Tolerations: []config.Toleration{{Operator: "Equal", Value: "v"}}}},
		{"bad effect", CreateRequest{Name: "ok", Agent: "claude", Tolerations: []config.Toleration{{Key: "k", Operator: "Exists", Effect: "Never"}}}},
		{"extended in requests", CreateRequest{Name: "ok", Agent: "claude", Resources: config.Resources{Requests: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "1"}}}}},
		{"bad extended name", CreateRequest{Name: "ok", Agent: "claude", Resources: config.Resources{Limits: config.ResourceList{Extended: map[string]string{"gpu": "1"}}}}},
		{"fractional gpu", CreateRequest{Name: "ok", Agent: "claude", Resources: config.Resources{Limits: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "500m"}}}}},
		{"zero gpu", CreateRequest{Name: "ok", Agent: "claude", Resources: config.Resources{Limits: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "0"}}}}},
		{"seconds without NoExecute", CreateRequest{Name: "ok", Agent: "claude", Tolerations: []config.Toleration{{Key: "k", Operator: "Exists", Effect: "NoSchedule", TolerationSeconds: ptr.To[int64](5)}}}},
	}
	for _, c := range cases {
		if _, err := svc.Create(ctx, u, c.req); err == nil {
			t.Errorf("%s: expected validation error", c.name)
		} else if _, ok := err.(*ValidationError); !ok {
			t.Errorf("%s: err type %T", c.name, err)
		}
	}
	// Valid scheduling settings are kept as given.
	sess, err := svc.Create(ctx, u, CreateRequest{Name: "sched", Agent: "claude", RuntimeClass: "gvisor",
		Resources:    config.Resources{Limits: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "1", "hugepages-2Mi": "64Mi"}}},
		NodeSelector: map[string]string{"kubernetes.io/arch": "arm64", "example.com/gpu": ""},
		Tolerations:  []config.Toleration{{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"}, {Operator: "Exists"}, {Key: "k", Value: "v", Effect: "NoExecute", TolerationSeconds: ptr.To[int64](30)}}})
	if err != nil {
		t.Fatal(err)
	}
	if v := svc.View(sess); v.RuntimeClass != "gvisor" || v.NodeSelector["kubernetes.io/arch"] != "arm64" || len(v.Tolerations) != 3 || v.Resources.Limits.Extended["nvidia.com/gpu"] != "1" {
		t.Fatalf("view = %+v", v)
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
	if sess.State != store.StateCreating || sess.PVCSize != "20Gi" || sess.StorageClass != "nfs-fast" || !sess.Autonomous || sess.NoGitCredentials {
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
	gh, err := svc.Creds.CreateAccount(ctx, u.ID, AccountInput{Kind: store.AcctGitHTTPS, Host: "github.com", Value: "ghp_x"})
	if err != nil {
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
	if _, err := svc.Creds.UpdateAccount(ctx, u.ID, gh.ID, AccountInput{Host: "github.com", Value: "bad"}); err != nil {
		t.Fatal(err)
	}
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
		v := accountValue(t, svc.Creds, u.ID, store.AcctClaudeLogin)
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
	if err := svc.SaveLogin(ctx, u, sess.ID, store.AcctClaudeLogin); err != nil {
		t.Fatal(err)
	}
	// The new login account is the session's from now on.
	if got, _ := svc.Store.Sessions().Get(ctx, sess.ID); got.AIAccount == "" {
		t.Fatal("session not switched to the saved login")
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

	// A hub that is not the leader still polls, so its pages show live
	// status, but leaves the side effects to the leader.
	svc.Leading = func() bool { return false }
	fr.setLoginAt(time.Now().Add(90 * time.Second))
	svc.PollOnce(ctx)
	if fr.count() != 2 {
		t.Fatalf("a follower exported a login: exports=%d", fr.count())
	}
	if _, ok := svc.live.Load(sess.ID); !ok {
		t.Fatal("a follower did not record live status")
	}
	svc.Leading = nil

	// A shell session's runner is never asked for a login.
	sh, _ := svc.Create(ctx, u, CreateRequest{Name: "s", Agent: "shell"})
	_, _ = svc.Store.Sessions().SetState(ctx, sh.ID, store.StateRunning, "")
	fr.setLoginAt(time.Now().Add(2 * time.Minute))
	svc.PollOnce(ctx)
	if fr.count() != 3 { // the claude session exported the newer file once more; the shell one did not
		t.Fatalf("exports = %d", fr.count())
	}
}

func TestUpdate(t *testing.T) {
	svc, orch, u := newService(t)
	ctx := context.Background()
	sess, err := svc.Create(ctx, u, CreateRequest{Name: "orig", Agent: "claude", RepoURL: "https://x/app.git", PVCSize: "5Gi", Resources: config.Resources{Limits: config.ResourceList{CPU: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	edit := CreateRequest{Name: "edited", Agent: "codex", PVCSize: "99Gi", StorageClass: "other", ImageTag: "1.2.3", RuntimeClass: "gvisor",
		Repos: []store.Repo{{URL: "https://x/app.git"}, {URL: "https://x/lib.git"}}, Env: map[string]string{"A": "b"}, Autonomous: ptr.To(false),
		GitCredentials: ptr.To(false), Resources: config.Resources{Limits: config.ResourceList{CPU: "2", Memory: "1Gi"}}}
	// Only stopped or failed sessions can be edited.
	if _, err := svc.Update(ctx, u, sess.ID, edit); err != ErrInvalidTransition {
		t.Fatalf("update while creating = %v", err)
	}
	if _, err := svc.Store.Sessions().SetState(ctx, sess.ID, store.StateStopped, ""); err != nil {
		t.Fatal(err)
	}
	notified := len(orch.notified)
	up, err := svc.Update(ctx, u, sess.ID, edit)
	if err != nil {
		t.Fatal(err)
	}
	// Agent, PVC size and storage class are kept; the rest is replaced.
	if up.Agent != "claude" || up.PVCSize != "5Gi" || up.StorageClass != "nfs-fast" {
		t.Fatalf("immutable fields changed: %+v", up)
	}
	if up.Name != "edited" || up.ImageTag != "1.2.3" || up.RuntimeClass != "gvisor" || len(up.Repos) != 2 || up.Repos[1].Path != "lib" || up.Env["A"] != "b" || up.Autonomous || up.Resources.Limits.Memory != "1Gi" || !up.NoGitCredentials {
		t.Fatalf("update = %+v", up)
	}
	if v := svc.View(up); v.GitCredentials {
		t.Fatal("view reports git credentials for a session without them")
	}
	if up.State != store.StateStopped || len(orch.notified) != notified {
		t.Fatalf("update changed state or notified the reconciler: %+v", up)
	}
	// Validation applies as on create; the service account needs the chart to offer one.
	if _, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "", Agent: "claude"}); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "sa", K8sAccess: store.K8sAccessReadOnly}); err == nil {
		t.Fatal("read-only access accepted without a service account configured")
	}
	svc.Defaults.ServiceAccount = "runner-view"
	if up, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "sa", K8sAccess: store.K8sAccessReadOnly, K8sNamespaces: []string{"ignored"}}); err != nil || up.K8sAccess != store.K8sAccessReadOnly || len(up.K8sNamespaces) != 0 {
		t.Fatalf("read-only access = %+v, %v", up, err)
	}
	// Namespace mode needs the hub to offer it, at least one namespace, and never the hub's own.
	if _, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "sa", K8sAccess: store.K8sAccessNamespace, K8sNamespaces: []string{"dev"}}); err == nil {
		t.Fatal("namespace access accepted without namespaceWrite")
	}
	svc.Defaults.K8sNamespaceWrite = true
	svc.Defaults.Namespace = "agents"
	for _, bad := range [][]string{nil, {" "}, {"agents"}, {"kube-system"}, {"Not_Valid"}} {
		if _, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "sa", K8sAccess: store.K8sAccessNamespace, K8sNamespaces: bad}); err == nil {
			t.Fatalf("namespaces %q accepted", bad)
		}
	}
	if _, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "sa", K8sAccess: "cluster-admin"}); err == nil {
		t.Fatal("unknown mode accepted")
	}
	up, err = svc.Update(ctx, u, sess.ID, CreateRequest{Name: "sa", K8sAccess: store.K8sAccessNamespace, K8sNamespaces: []string{" dev ", "staging", "dev"}})
	if err != nil || up.K8sAccess != store.K8sAccessNamespace || strings.Join(up.K8sNamespaces, ",") != "dev,staging" {
		t.Fatalf("namespace access = %+v, %v", up, err)
	}
	// Off clears the list.
	if up, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "sa", K8sNamespaces: []string{"dev"}}); err != nil || up.K8sAccess != store.K8sAccessOff || len(up.K8sNamespaces) != 0 {
		t.Fatalf("off = %+v, %v", up, err)
	}
	// Another user cannot edit it.
	other := &store.User{Username: "bob", PasswordHash: "x"}
	_ = svc.Store.Users().Create(ctx, other)
	if _, err := svc.Update(ctx, other, sess.ID, edit); err == nil {
		t.Fatal("other user's edit accepted")
	}
}

func TestEgressValidation(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	// A hub that opens nothing refuses any destination.
	if _, err := svc.Create(ctx, u, CreateRequest{Name: "off", Agent: "shell", Egress: []string{"192.168.1.10"}}); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("create on a hub without egress = %v", err)
	}
	allowed, _ := egress.ParsePrefixes([]string{"192.168.0.0/16"})
	denied, _ := egress.ParsePrefixes([]string{"192.168.99.0/24"})
	svc.Defaults.Egress = egress.Policy{Allowed: allowed, Denied: denied}

	sess, err := svc.Create(ctx, u, CreateRequest{Name: "lan", Agent: "shell", Egress: []string{" 192.168.1.10:8080", "192.168.2.7/24", "192.168.1.10:8080"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sess.Egress, []string{"192.168.1.10:8080", "192.168.2.0/24"}) {
		t.Fatalf("stored egress = %q", sess.Egress)
	}
	if v := svc.View(sess); !slices.Equal(v.Egress, sess.Egress) {
		t.Fatalf("view egress = %q", v.Egress)
	}
	for _, bad := range []string{"8.8.8.8", "192.168.99.5", "nas.local"} {
		if _, err := svc.Create(ctx, u, CreateRequest{Name: "bad", Agent: "shell", Egress: []string{bad}}); err == nil {
			t.Errorf("%q accepted", bad)
		} else if _, ok := err.(*ValidationError); !ok {
			t.Errorf("%q: %T is not a validation error", bad, err)
		}
	}
	// Editing a stopped session replaces the list.
	if _, err := svc.Store.Sessions().SetState(ctx, sess.ID, store.StateStopped, ""); err != nil {
		t.Fatal(err)
	}
	up, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "lan", Egress: []string{"192.168.5.5"}})
	if err != nil || !slices.Equal(up.Egress, []string{"192.168.5.5"}) {
		t.Fatalf("update = %q, %v", up.Egress, err)
	}
	if up, err = svc.Update(ctx, u, sess.ID, CreateRequest{Name: "lan"}); err != nil || len(up.Egress) != 0 {
		t.Fatalf("clearing = %q, %v", up.Egress, err)
	}
	if v := svc.View(up); v.Egress == nil {
		t.Fatal("view renders egress as null")
	}
}

func TestSandboxAccessValidation(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	req := CreateRequest{Name: "Cluster A", Agent: "claude", K8sAccess: store.K8sAccessSandbox, K8sNamespaces: []string{"ignored"}}
	if _, err := svc.Create(ctx, u, req); err == nil {
		t.Fatal("sandbox accepted on a hub without the mode")
	}
	svc.Defaults.K8sSandbox = true
	svc.Defaults.SandboxDomain = "sandbox.example.com"
	svc.Defaults.Sandbox = reconcile.Config{Sandbox: config.Sandbox{Enabled: true, Domain: "sandbox.example.com"}}.Defaults()
	sess, err := svc.Create(ctx, u, req)
	if err != nil || sess.K8sAccess != store.K8sAccessSandbox || len(sess.K8sNamespaces) != 0 {
		t.Fatalf("sandbox session = %+v, %v", sess, err)
	}
	v := svc.View(sess)
	if v.SandboxNamespace != "sandbox-"+sess.ID || v.SandboxHost != "cluster-a.sandbox.example.com" {
		t.Fatalf("view = %q %q", v.SandboxNamespace, v.SandboxHost)
	}
	// Another sandbox (of anyone) cannot take the same host label.
	other := &store.User{Username: "bob", PasswordHash: "x"}
	_ = svc.Store.Users().Create(ctx, other)
	if _, err := svc.Create(ctx, other, CreateRequest{Name: "cluster-a", Agent: "claude", K8sAccess: store.K8sAccessSandbox}); err == nil {
		t.Fatal("duplicate sandbox host accepted")
	}
	// The same name is fine outside sandbox mode, and a session keeps its own label on edit.
	if _, err := svc.Create(ctx, other, CreateRequest{Name: "cluster-a", Agent: "claude"}); err != nil {
		t.Fatalf("non-sandbox session with the same name: %v", err)
	}
	if _, err := svc.Store.Sessions().SetState(ctx, sess.ID, store.StateStopped, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "Cluster A", K8sAccess: store.K8sAccessSandbox}); err != nil {
		t.Fatalf("edit keeping the name: %v", err)
	}
	// The sandbox's env names are reserved.
	if _, err := svc.Create(ctx, u, CreateRequest{Name: "x", Agent: "claude", Env: map[string]string{"SANDBOX_NAMESPACE": "kube-system"}}); err == nil {
		t.Fatal("reserved sandbox env accepted")
	}
}

func TestPersonaAndAgentValidation(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	def := "---\nname: red-team\ndescription: Reviewer\n---\nYou review code."
	sess, err := svc.Create(ctx, u, CreateRequest{Name: "oc", Agent: "opencode", Persona: "  Be terse.\r\nNo refusals.  ", AgentDefinition: def + "\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Persona != "Be terse.\nNo refusals." || sess.AgentDefinition != def {
		t.Fatalf("stored = %q %q", sess.Persona, sess.AgentDefinition)
	}
	if v := svc.View(sess); v.Persona != sess.Persona || v.AgentDefinition != def || v.Handle != sess.Handle {
		t.Fatalf("view = %+v", v)
	}
	// Every agent but the shell takes a persona.
	for _, agent := range []string{"claude", "codex"} {
		if _, err := svc.Create(ctx, u, CreateRequest{Name: agent, Agent: agent, Persona: "You review."}); err != nil {
			t.Errorf("%s persona: %v", agent, err)
		}
	}
	for name, req := range map[string]CreateRequest{
		"persona on a shell":          {Name: "c", Agent: "shell", Persona: "x"},
		"definition on another agent": {Name: "c", Agent: "codex", AgentDefinition: def},
		"too long":                    {Name: "c", Agent: "opencode", Persona: strings.Repeat("x", MaxPersona+1)},
		"nul":                         {Name: "c", Agent: "opencode", Persona: "a\x00b"},
		"not utf-8":                   {Name: "c", Agent: "opencode", Persona: "a\xffb"},
		"bad definition":              {Name: "c", Agent: "opencode", AgentDefinition: "You are an agent."},
	} {
		if _, err := svc.Create(ctx, u, req); err == nil {
			t.Errorf("%s accepted", name)
		} else if _, ok := err.(*ValidationError); !ok {
			t.Errorf("%s: %T %v", name, err, err)
		}
	}
	// Edits on a stopped session apply; the handle stays.
	if _, err := svc.Store.Sessions().SetState(ctx, sess.ID, store.StateStopped, ""); err != nil {
		t.Fatal(err)
	}
	up, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "renamed", Persona: "   "})
	if err != nil || up.Persona != "" || up.AgentDefinition != "" || up.Handle != sess.Handle || up.Name != "renamed" {
		t.Fatalf("cleared = %+v, %v", up, err)
	}
	// Reserved env vars cannot smuggle either in.
	for _, k := range []string{"AGENT_PERSONA", "AGENT_HANDLE", "AGENT_DEFINITION", "AGENT_DEFINITION_NAME"} {
		if _, err := svc.Create(ctx, u, CreateRequest{Name: "e", Agent: "opencode", Env: map[string]string{k: "x"}}); err == nil {
			t.Errorf("%s accepted as session env", k)
		}
	}
}

func TestHandles(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		s, err := svc.Create(ctx, u, CreateRequest{Name: fmt.Sprintf("s%d", i), Agent: "shell"})
		if err != nil {
			t.Fatal(err)
		}
		if !names.Pattern.MatchString(s.Handle) || seen[s.Handle] {
			t.Fatalf("handle %q", s.Handle)
		}
		seen[s.Handle] = true
	}
	// Sessions from before handles get one at start; existing ones keep theirs.
	old := &store.Session{OwnerID: u.ID, Name: "old", Agent: "shell", State: store.StateStopped}
	if err := svc.Store.Sessions().Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := svc.BackfillHandles(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Store.Sessions().Get(ctx, old.ID)
	if !names.Pattern.MatchString(got.Handle) || seen[got.Handle] {
		t.Fatalf("backfilled handle %q", got.Handle)
	}
	if err := svc.BackfillHandles(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := svc.Store.Sessions().Get(ctx, old.ID); again.Handle != got.Handle {
		t.Fatal("backfill changed an existing handle")
	}
}

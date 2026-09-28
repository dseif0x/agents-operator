package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dseif0x/agents-operator/internal/github"
	"github.com/dseif0x/agents-operator/internal/store"
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
	list, err := svc.Creds.List(ctx, u.ID)
	if err != nil || len(list) != 2 {
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
	if len(list) != 1 {
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

package store

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/config"
)

// stores returns the implementations under test: always Memory, and
// Postgres when AGENTS_OPERATOR_TEST_DATABASE_URL is set (CI provides one).
func stores(t *testing.T) map[string]Store {
	t.Helper()
	out := map[string]Store{"memory": NewMemory()}
	if url := os.Getenv("AGENTS_OPERATOR_TEST_DATABASE_URL"); url != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pg, err := Open(ctx, url, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		// Start from a clean slate; tests share one database.
		_, _ = pg.pool.Exec(ctx, "TRUNCATE session_events, sessions, user_credentials, repo_usage, users")
		t.Cleanup(pg.Close)
		out["postgres"] = pg
	} else {
		t.Log("AGENTS_OPERATOR_TEST_DATABASE_URL not set; skipping postgres")
	}
	return out
}

func TestStores(t *testing.T) {
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) { exercise(t, st) })
	}
}

func exercise(t *testing.T, st Store) {
	ctx := context.Background()
	if err := st.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	// users
	u := &User{Username: "alice", PasswordHash: "h1"}
	if err := st.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().Create(ctx, &User{Username: "alice", PasswordHash: "h2"}); err != ErrConflict {
		t.Fatalf("duplicate username err = %v", err)
	}
	got, err := st.Users().GetByUsername(ctx, "alice")
	if err != nil || got.ID != u.ID || got.PasswordHash != "h1" {
		t.Fatalf("GetByUsername = %+v, %v", got, err)
	}
	if _, err := st.Users().GetByID(ctx, NewID()); err != ErrNotFound {
		t.Fatalf("missing user err = %v", err)
	}
	up, err := st.Users().UpsertPassword(ctx, "alice", "h3")
	if err != nil || up.ID != u.ID || up.PasswordHash != "h3" {
		t.Fatalf("UpsertPassword existing = %+v, %v", up, err)
	}
	adm, err := st.Users().UpsertPassword(ctx, "admin", "h4")
	if err != nil || adm.Username != "admin" {
		t.Fatalf("UpsertPassword new = %+v, %v", adm, err)
	}

	// sessions
	s := &Session{
		OwnerID: u.ID, Name: "one", Agent: "claude", Repos: []Repo{{URL: "https://example.com/r.git", Branch: "main", Path: "r"}},
		PVCSize: "20Gi", StorageClass: "nfs-fast", RuntimeClass: "gvisor", ServiceAccount: true, State: StateCreating, Autonomous: true,
		Resources:    config.Resources{Requests: config.ResourceList{CPU: "250m"}, Limits: config.ResourceList{Memory: "1Gi"}},
		NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
		Tolerations:  []config.Toleration{{Key: "gpu", Operator: "Exists"}},
		Env:          map[string]string{"FOO": "bar"},
	}
	if err := st.Sessions().Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	g, err := st.Sessions().Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "one" || g.Generation != 1 || len(g.Repos) != 1 || g.Repos[0].Path != "r" || g.Repos[0].Branch != "main" || g.Resources.Limits.Memory != "1Gi" || g.NodeSelector["kubernetes.io/arch"] != "amd64" ||
		len(g.Tolerations) != 1 || g.Env["FOO"] != "bar" || !g.Autonomous || g.LastOutputAt != nil || g.RuntimeClass != "gvisor" || !g.ServiceAccount {
		t.Fatalf("round trip lost data: %+v", g)
	}
	if err := st.Sessions().Create(ctx, &Session{OwnerID: adm.ID, Name: "two", Agent: "codex", PVCSize: "1Gi", State: StateStopped}); err != nil {
		t.Fatal(err)
	}
	list, err := st.Sessions().List(ctx, u.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d, %v", len(list), err)
	}
	all, err := st.Sessions().ListAll(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAll = %d, %v", len(all), err)
	}
	// Update replaces the editable settings and nothing else.
	edited := *g
	edited.Name, edited.ImageTag, edited.ServiceAccount, edited.Autonomous = "renamed", "1.2.3", false, false
	edited.Repos = append(edited.Repos, Repo{URL: "https://example.com/two.git", Path: "two"})
	edited.Env = map[string]string{"A": "b"}
	edited.State = StateRunning // ignored
	ed, err := st.Sessions().Update(ctx, &edited)
	if err != nil || ed.Name != "renamed" || ed.ImageTag != "1.2.3" || ed.ServiceAccount || ed.Autonomous || len(ed.Repos) != 2 || ed.Env["A"] != "b" || ed.State != StateCreating || ed.Generation != 1 {
		t.Fatalf("Update = %+v, %v", ed, err)
	}
	if _, err := st.Sessions().Update(ctx, &Session{ID: NewID()}); err != ErrNotFound {
		t.Fatalf("Update missing = %v", err)
	}
	if _, err := st.Sessions().SetState(ctx, s.ID, StateRunning, "pod ready"); err != nil {
		t.Fatal(err)
	}
	b, err := st.Sessions().Bump(ctx, s.ID, StateCreating)
	if err != nil || b.Generation != 2 || b.State != StateCreating || b.StateReason != "" {
		t.Fatalf("Bump = %+v, %v", b, err)
	}
	now := time.Now().Truncate(time.Microsecond)
	if err := st.Sessions().TouchOutput(ctx, s.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions().TouchOutput(ctx, s.ID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions().TouchAttached(ctx, s.ID, now); err != nil {
		t.Fatal(err)
	}
	g, _ = st.Sessions().Get(ctx, s.ID)
	if g.LastOutputAt == nil || !g.LastOutputAt.Equal(now) || g.LastAttachedAt == nil {
		t.Fatalf("touch: %+v", g)
	}
	counts, err := st.Sessions().CountByState(ctx)
	if err != nil || counts[StateCreating] != 1 || counts[StateStopped] != 1 {
		t.Fatalf("CountByState = %v, %v", counts, err)
	}
	d, err := st.Sessions().SetState(ctx, s.ID, StateDeleting, "")
	if err != nil || d.DeletedAt == nil {
		t.Fatalf("deleting should set deleted_at: %+v %v", d, err)
	}

	// events
	for i := 0; i < 5; i++ {
		if err := st.Events().Add(ctx, s.ID, "state", "msg"); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := st.Events().List(ctx, s.ID, 3)
	if err != nil || len(evs) != 3 || evs[0].ID < evs[1].ID {
		t.Fatalf("events = %v, %v", evs, err)
	}
	if err := st.Events().Prune(ctx, s.ID, 2); err != nil {
		t.Fatal(err)
	}
	evs, _ = st.Events().List(ctx, s.ID, 0)
	if len(evs) != 2 {
		t.Fatalf("after prune = %d", len(evs))
	}
	if err := st.Sessions().Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Sessions().Get(ctx, s.ID); err != ErrNotFound {
		t.Fatalf("after delete err = %v", err)
	}

	// credentials
	if err := st.Credentials().Upsert(ctx, &Credential{UserID: u.ID, Kind: CredAnthropicAPIKey, SecretRef: "anthropic_api_key"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Credentials().Upsert(ctx, &Credential{UserID: u.ID, Kind: CredAnthropicAPIKey, SecretRef: "anthropic_api_key"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Credentials().Upsert(ctx, &Credential{UserID: u.ID, Kind: CredGitSSHKey, SecretRef: "git_ssh_key"}); err != nil {
		t.Fatal(err)
	}
	cs, err := st.Credentials().List(ctx, u.ID)
	if err != nil || len(cs) != 2 || cs[0].Kind != CredAnthropicAPIKey {
		t.Fatalf("credentials = %v, %v", cs, err)
	}
	if err := st.Credentials().Delete(ctx, u.ID, CredGitSSHKey); err != nil {
		t.Fatal(err)
	}
	cs, _ = st.Credentials().List(ctx, u.ID)
	if len(cs) != 1 {
		t.Fatalf("after delete = %d", len(cs))
	}

	// repo usage
	for _, k := range []string{"github.com/a/b", "github.com/a/b", "github.com/c/d"} {
		if err := st.RepoUsage().Increment(ctx, u.ID, k); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := st.RepoUsage().List(ctx, u.ID)
	if err != nil || usage["github.com/a/b"] != 2 || usage["github.com/c/d"] != 1 || len(usage) != 2 {
		t.Fatalf("usage = %v, %v", usage, err)
	}
	if other, _ := st.RepoUsage().List(ctx, adm.ID); len(other) != 0 {
		t.Fatalf("usage leaked across users: %v", other)
	}
}

func TestRepoKey(t *testing.T) {
	want := "github.com/owner/repo"
	for _, in := range []string{
		"https://github.com/Owner/Repo.git", "git@github.com:owner/repo.git", "ssh://git@github.com/owner/repo",
		"https://github.com/owner/repo/", "HTTPS://GITHUB.COM/owner/repo.git", "https://x-access-token:abc@github.com/owner/repo.git",
	} {
		if got := RepoKey(in); got != want {
			t.Errorf("RepoKey(%q) = %q", in, got)
		}
	}
}

func TestNewID(t *testing.T) {
	id := NewID()
	if len(id) != 36 || id[14] != '4' {
		t.Fatalf("bad uuid %q", id)
	}
	if NewID() == id {
		t.Fatal("ids not random")
	}
}

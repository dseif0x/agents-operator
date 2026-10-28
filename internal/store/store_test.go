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
		_, _ = pg.pool.Exec(ctx, "TRUNCATE push_subscriptions, app_settings, chat_reads, chat_deliveries, chat_attachments, chat_messages, chat_members, chats, session_events, sessions, user_credentials, accounts, user_profiles, repo_usage, users")
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
		PVCSize: "20Gi", StorageClass: "nfs-fast", RuntimeClass: "gvisor", K8sAccess: K8sAccessNamespace, K8sNamespaces: []string{"dev", "staging"}, State: StateCreating, Autonomous: true,
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
		len(g.Tolerations) != 1 || g.Env["FOO"] != "bar" || !g.Autonomous || g.LastOutputAt != nil || g.RuntimeClass != "gvisor" || g.K8sAccess != K8sAccessNamespace || len(g.K8sNamespaces) != 2 || g.K8sNamespaces[1] != "staging" {
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
	edited.Name, edited.ImageTag, edited.K8sAccess, edited.K8sNamespaces, edited.Autonomous = "renamed", "1.2.3", K8sAccessReadOnly, nil, false
	edited.NoGitCredentials = true
	edited.Egress = []string{"192.168.1.10:8080"}
	edited.Persona, edited.AgentDefinition = "Be terse.", "---\nname: a\n---\nb"
	edited.Repos = append(edited.Repos, Repo{URL: "https://example.com/two.git", Path: "two"})
	edited.Env = map[string]string{"A": "b"}
	edited.State = StateRunning // ignored
	ed, err := st.Sessions().Update(ctx, &edited)
	if err != nil || ed.Name != "renamed" || ed.ImageTag != "1.2.3" || ed.K8sAccess != K8sAccessReadOnly || len(ed.K8sNamespaces) != 0 || ed.K8sNamespaces == nil || ed.Autonomous || !ed.NoGitCredentials || len(ed.Egress) != 1 || ed.Persona != "Be terse." || ed.AgentDefinition == "" || ed.Egress[0] != "192.168.1.10:8080" || len(ed.Repos) != 2 || ed.Env["A"] != "b" || ed.State != StateCreating || ed.Generation != 1 {
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
	if ids, err := st.Credentials().UserIDs(ctx); err != nil || len(ids) != 1 || ids[0] != u.ID {
		t.Fatalf("credential users = %v, %v", ids, err)
	}

	// accounts
	a1 := &Account{UserID: u.ID, Kind: AcctOpenAICompatible, Name: "LiteLLM", BaseURL: "http://l/v1", Models: []string{"a", "b"}}
	if err := st.Accounts().Create(ctx, a1); err != nil || a1.ID == "" {
		t.Fatalf("create account: %v", err)
	}
	if err := st.Accounts().Create(ctx, &Account{UserID: u.ID, Kind: AcctAnthropicAPIKey, Name: "litellm"}); err != ErrConflict {
		t.Fatalf("duplicate AI account name err = %v", err)
	}
	g1 := &Account{UserID: u.ID, Kind: AcctGitHTTPS, Name: "github.com", Host: "github.com"}
	if err := st.Accounts().Create(ctx, g1); err != nil {
		t.Fatal(err)
	}
	if err := st.Accounts().Create(ctx, &Account{UserID: u.ID, Kind: AcctGitHTTPS, Name: "other", Host: "GitHub.com"}); err != ErrConflict {
		t.Fatalf("second HTTPS account for a host err = %v", err)
	}
	// Same host, other kind; same name as an AI account: both fine.
	if err := st.Accounts().Create(ctx, &Account{UserID: u.ID, Kind: AcctGitSSH, Name: "LiteLLM", Host: "github.com"}); err != nil {
		t.Fatal(err)
	}
	// Another user may use the same name.
	if err := st.Accounts().Create(ctx, &Account{UserID: adm.ID, Kind: AcctAnthropicAPIKey, Name: "LiteLLM"}); err != nil {
		t.Fatal(err)
	}
	accts, err := st.Accounts().List(ctx, u.ID)
	if err != nil || len(accts) != 3 || accts[0].ID != a1.ID || len(accts[0].Models) != 2 || accts[1].Host != "github.com" {
		t.Fatalf("accounts = %+v, %v", accts, err)
	}
	before := a1.UpdatedAt
	a1.Name, a1.Models = "Gateway", nil
	if err := st.Accounts().Update(ctx, a1); err != nil || a1.Name != "Gateway" || len(a1.Models) != 0 || !a1.UpdatedAt.After(before) {
		t.Fatalf("update account = %+v, %v", a1, err)
	}
	if got, err := st.Accounts().Get(ctx, u.ID, a1.ID); err != nil || got.Name != "Gateway" || got.BaseURL != "http://l/v1" {
		t.Fatalf("get account = %+v, %v", got, err)
	}
	if _, err := st.Accounts().Get(ctx, adm.ID, a1.ID); err != ErrNotFound {
		t.Fatalf("account of another user err = %v", err)
	}
	if _, err := st.Accounts().Get(ctx, u.ID, "not-a-uuid"); err != ErrNotFound {
		t.Fatalf("bad id err = %v", err)
	}
	if err := st.Accounts().Delete(ctx, u.ID, g1.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Accounts().Delete(ctx, u.ID, g1.ID); err != ErrNotFound {
		t.Fatalf("second delete err = %v", err)
	}

	// profiles
	p, err := st.Profiles().Get(ctx, u.ID)
	if err != nil || p.GitName != "" || p.Defaults == nil {
		t.Fatalf("empty profile = %+v, %v", p, err)
	}
	if err := st.Profiles().Put(ctx, &Profile{UserID: u.ID, GitName: "Alice", GitEmail: "a@x", Defaults: map[string]string{"claude": a1.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Profiles().Put(ctx, &Profile{UserID: u.ID, GitName: "Alice B", GitEmail: "a@x", Defaults: map[string]string{"claude": a1.ID}}); err != nil {
		t.Fatal(err)
	}
	if p, err := st.Profiles().Get(ctx, u.ID); err != nil || p.GitName != "Alice B" || p.Defaults["claude"] != a1.ID {
		t.Fatalf("profile = %+v, %v", p, err)
	}

	// a session's AI account
	sa := &Session{OwnerID: u.ID, Name: "acct", Agent: "claude", State: StateCreating, AIAccount: a1.ID}
	if err := st.Sessions().Create(ctx, sa); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Sessions().Get(ctx, sa.ID); got.AIAccount != a1.ID {
		t.Fatalf("ai account = %q", got.AIAccount)
	}
	sa.AIAccount = ""
	if got, err := st.Sessions().Update(ctx, sa); err != nil || got.AIAccount != "" {
		t.Fatalf("ai account after update = %+v, %v", got, err)
	}

	// handles
	h1 := &Session{OwnerID: u.ID, Name: "h1", Agent: "claude", State: StateCreating, Handle: "eager_turing"}
	if err := st.Sessions().Create(ctx, h1); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions().Create(ctx, &Session{OwnerID: u.ID, Name: "h2", Agent: "claude", State: StateCreating, Handle: "eager_turing"}); err != ErrConflict {
		t.Fatalf("duplicate handle err = %v", err)
	}
	if err := st.Sessions().Create(ctx, &Session{OwnerID: adm.ID, Name: "h3", Agent: "claude", State: StateCreating, Handle: "eager_turing"}); err != nil {
		t.Fatalf("same handle for another owner: %v", err)
	}
	if err := st.Sessions().SetHandle(ctx, sa.ID, "brave_curie"); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions().SetHandle(ctx, sa.ID, "calm_bohr"); err != ErrNotFound {
		t.Fatalf("second SetHandle err = %v", err)
	}
	h1.Persona = "Be blunt."
	if got, err := st.Sessions().Update(ctx, h1); err != nil || got.Handle != "eager_turing" || got.Persona != "Be blunt." {
		t.Fatalf("update kept handle = %+v, %v", got, err)
	}

	// chats
	chat := &Chat{OwnerID: u.ID, Name: "launch", Description: "ship it"}
	if err := st.Chats().Create(ctx, chat); err != nil || chat.ID == "" {
		t.Fatalf("create chat: %v", err)
	}
	for _, sid := range []string{sa.ID, h1.ID} {
		if err := st.Chats().AddMember(ctx, chat.ID, sid); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Chats().AddMember(ctx, chat.ID, sa.ID); err != ErrConflict {
		t.Fatalf("duplicate member err = %v", err)
	}
	if ids, _ := st.Chats().ChatsOf(ctx, h1.ID); len(ids) != 1 || ids[0] != chat.ID {
		t.Fatalf("chats of = %v", ids)
	}
	m1 := &ChatMessage{ChatID: chat.ID, Text: "hello", Mentions: []string{h1.ID}}
	if err := st.Chats().Post(ctx, m1, []NewAttachment{{Name: "a.png", ContentType: "image/png", Data: []byte("png")}}, []string{sa.ID, h1.ID}); err != nil || m1.ID == 0 || len(m1.Attachments) != 1 {
		t.Fatalf("post = %+v, %v", m1, err)
	}
	m2 := &ChatMessage{ChatID: chat.ID, AuthorSession: h1.ID, AuthorName: "h1", Text: "hi back", ReplyTo: m1.ID, Everyone: true}
	if err := st.Chats().Post(ctx, m2, nil, []string{sa.ID}); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Chats().Messages(ctx, chat.ID, 0, 0, 10)
	if err != nil || len(msgs) != 2 || msgs[0].ID != m1.ID || len(msgs[0].Attachments) != 1 || msgs[1].AuthorSession != h1.ID || msgs[1].ReplyTo != m1.ID || !msgs[1].Everyone || msgs[0].Mentions[0] != h1.ID {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
	if older, _ := st.Chats().Messages(ctx, chat.ID, m2.ID, 0, 10); len(older) != 1 || older[0].ID != m1.ID {
		t.Fatalf("before = %+v", older)
	}
	if newer, _ := st.Chats().Messages(ctx, chat.ID, 0, m1.ID, 10); len(newer) != 1 || newer[0].ID != m2.ID {
		t.Fatalf("after = %+v", newer)
	}
	a, inChat, data, err := st.Chats().Attachment(ctx, m1.Attachments[0].ID)
	if err != nil || inChat != chat.ID || string(data) != "png" || a.ContentType != "image/png" {
		t.Fatalf("attachment = %+v %s %v", a, inChat, err)
	}
	if n, _ := st.Chats().AttachmentBytes(ctx, u.ID); n != 3 {
		t.Fatalf("attachment bytes = %d", n)
	}
	if q, _ := st.Chats().Queued(ctx, sa.ID); len(q) != 2 {
		t.Fatalf("queued = %d", len(q))
	}
	if err := st.Chats().MarkDelivered(ctx, sa.ID, []int64{m1.ID}); err != nil {
		t.Fatal(err)
	}
	if q, _ := st.Chats().Queued(ctx, sa.ID); len(q) != 1 || q[0].ID != m2.ID {
		t.Fatalf("queued after delivery = %+v", q)
	}
	if ds, _ := st.Chats().Deliveries(ctx, []int64{m1.ID}); len(ds) != 2 {
		t.Fatalf("deliveries = %+v", ds)
	}
	if n, _ := st.Chats().Unread(ctx, chat.ID, OwnerReader); n != 1 { // the agent's message
		t.Fatalf("owner unread = %d", n)
	}
	if n, _ := st.Chats().Unread(ctx, chat.ID, h1.ID); n != 1 { // the owner's message
		t.Fatalf("h1 unread = %d", n)
	}
	_ = st.Chats().SetRead(ctx, chat.ID, OwnerReader, m2.ID)
	_ = st.Chats().SetRead(ctx, chat.ID, OwnerReader, m1.ID) // never back
	if n, _ := st.Chats().Unread(ctx, chat.ID, OwnerReader); n != 0 {
		t.Fatalf("owner unread after reading = %d", n)
	}
	if err := st.Chats().SetMemberMuted(ctx, chat.ID, h1.ID, true); err != nil {
		t.Fatal(err)
	}
	if ms, _ := st.Chats().Members(ctx, chat.ID); len(ms) != 2 || !ms[1].Muted {
		t.Fatalf("members = %+v", ms)
	}
	if err := st.Chats().RemoveMember(ctx, chat.ID, sa.ID); err != nil {
		t.Fatal(err)
	}
	if q, _ := st.Chats().Queued(ctx, sa.ID); len(q) != 0 {
		t.Fatal("a removed member kept its queue")
	}
	chat.Name, chat.Muted = "renamed", true
	if err := st.Chats().Update(ctx, chat); err != nil || chat.Name != "renamed" || !chat.Muted {
		t.Fatalf("update chat = %+v, %v", chat, err)
	}
	if list, _ := st.Chats().List(ctx, u.ID); len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	// A deleted session leaves the chat but its messages stay.
	if err := st.Sessions().Delete(ctx, h1.ID); err != nil {
		t.Fatal(err)
	}
	if ms, _ := st.Chats().Members(ctx, chat.ID); len(ms) != 0 {
		t.Fatalf("members after delete = %+v", ms)
	}
	if msgs, _ := st.Chats().Messages(ctx, chat.ID, 0, 0, 10); len(msgs) != 2 || msgs[1].AuthorName != "h1" {
		t.Fatalf("messages after delete = %+v", msgs)
	}
	if err := st.Chats().Delete(ctx, chat.ID); err != nil {
		t.Fatal(err)
	}

	// push subscriptions and settings
	sub := &PushSubscription{Endpoint: "https://push.example/a", UserID: u.ID, P256dh: "p", Auth: "a", UserAgent: "ua"}
	if err := st.Push().Save(ctx, sub); err != nil {
		t.Fatal(err)
	}
	sub.Auth = "b"
	if err := st.Push().Save(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if subs, err := st.Push().List(ctx, u.ID); err != nil || len(subs) != 1 || subs[0].Auth != "b" {
		t.Fatalf("push subs = %+v, %v", subs, err)
	}
	if err := st.Push().Delete(ctx, adm.ID, sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	if subs, _ := st.Push().List(ctx, u.ID); len(subs) != 1 {
		t.Fatal("another user deleted a subscription")
	}
	if err := st.Push().Delete(ctx, "", sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	if subs, _ := st.Push().List(ctx, u.ID); len(subs) != 0 {
		t.Fatal("subscription not deleted")
	}
	if v, err := st.Settings().Get(ctx, "k"); err != nil || v != "" {
		t.Fatalf("unset setting = %q, %v", v, err)
	}
	if v, _ := st.Settings().SetIfAbsent(ctx, "k", "one"); v != "one" {
		t.Fatalf("first set = %q", v)
	}
	if v, _ := st.Settings().SetIfAbsent(ctx, "k", "two"); v != "one" {
		t.Fatalf("second set = %q", v)
	}
	if _, err := st.Chats().Get(ctx, chat.ID); err != ErrNotFound {
		t.Fatalf("get deleted chat err = %v", err)
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

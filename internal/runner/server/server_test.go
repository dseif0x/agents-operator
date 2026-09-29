package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func TestRing(t *testing.T) {
	r := NewRing(8)
	_, _ = r.Write([]byte("abc"))
	if got := string(r.Bytes()); got != "abc" {
		t.Fatalf("got %q", got)
	}
	_, _ = r.Write([]byte("defgh"))
	if got := string(r.Bytes()); got != "abcdefgh" {
		t.Fatalf("got %q", got)
	}
	_, _ = r.Write([]byte("ij"))
	if got := string(r.Bytes()); got != "cdefghij" {
		t.Fatalf("got %q", got)
	}
	_, _ = r.Write([]byte("0123456789"))
	if got := string(r.Bytes()); got != "23456789" {
		t.Fatalf("got %q", got)
	}
	r.Reset()
	if r.Len() != 0 {
		t.Fatal("reset failed")
	}
}

func TestNeedsAttention(t *testing.T) {
	now := time.Now()
	old := now.Add(-5 * time.Second)
	cases := []struct {
		tail string
		last time.Time
		want bool
	}{
		{"Do you want to proceed? (y/n)", old, true},
		{"$", old, true},
		{"❯", old, true},
		{"working...", old, false},
		{"$", now, false},
		{"", old, false},
	}
	for _, c := range cases {
		if got := NeedsAttention(c.tail, c.last, now); got != c.want {
			t.Errorf("NeedsAttention(%q)=%v want %v", c.tail, got, c.want)
		}
	}
	// Charset designations, keypad modes, cursor save/restore, DCS strings
	// and a trailing half-written escape are all invisible.
	raw := "\x1b(B\x1b)0\x1b=\x1b>\x1b7\x1b[?25h\x1bPq;1\x1b\\\x1b#8❯ done\x1b8\x1b[K\x1b"
	if got := LastLine([]byte(raw)); got != "❯ done" {
		t.Errorf("LastLine=%q", got)
	}
	if got := LastLine([]byte("\x1b(B\r\n(B\r\n")); got != "(B" { // real text stays
		t.Errorf("LastLine=%q", got)
	}
	if got := LastLine([]byte("\x1b[32mhello\x1b[0m\r\nworld \x1b[K\r\n\r\n")); got != "world" {
		t.Errorf("LastLine=%q", got)
	}
}

type testEnv struct {
	proc *Process
	srv  *httptest.Server
	tok  string
	home string
}

func newTestEnv(t *testing.T, script string) *testEnv {
	t.Helper()
	log := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
	proc := NewProcess(func() Command {
		return Command{Path: "bash", Args: []string{"-c", script}, Env: []string{"TERM=xterm", "PATH=/usr/bin:/bin"}}
	}, log)
	if err := proc.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(proc.Close)
	home := t.TempDir()
	s := &Server{Proc: proc, Token: "secret", Agent: "shell", Home: home, Log: log}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{proc: proc, srv: srv, tok: "secret", home: home}
}

func (e *testEnv) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + e.tok}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func readControl(t *testing.T, c *websocket.Conn, want string) runner.Control {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		typ, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m runner.Control
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("bad control: %v", err)
		}
		if m.T == want {
			return m
		}
	}
	t.Fatalf("no %q control message", want)
	return runner.Control{}
}

// readUntil reads binary frames until the accumulated output contains needle.
func readUntil(t *testing.T, c *websocket.Conn, needle string) string {
	t.Helper()
	var buf bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		typ, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v (so far %q)", err, buf.String())
		}
		if typ == websocket.MessageBinary {
			buf.Write(data)
			if strings.Contains(buf.String(), needle) {
				return buf.String()
			}
		}
	}
	t.Fatalf("did not see %q, got %q", needle, buf.String())
	return ""
}

func TestAuthRequired(t *testing.T) {
	e := newTestEnv(t, "sleep 5")
	resp, err := http.Get(e.srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without token = %d", resp.StatusCode)
	}
	resp, err = http.Get(e.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
}

func TestScrollbackReplay(t *testing.T) {
	e := newTestEnv(t, "echo MARKER_ONE; sleep 30")
	c1 := e.dial(t)
	hello := readControl(t, c1, runner.MsgHello)
	if hello.Cols != runner.DefaultCols || hello.Rows != runner.DefaultRows {
		t.Fatalf("hello size %dx%d", hello.Cols, hello.Rows)
	}
	readUntil(t, c1, "MARKER_ONE")
	_ = c1.Close(websocket.StatusNormalClosure, "")

	// A second client must get the replayed output before anything live.
	c2 := e.dial(t)
	hello2 := readControl(t, c2, runner.MsgHello)
	if !hello2.Scrollback {
		t.Fatal("expected scrollback=true on reattach")
	}
	out := readUntil(t, c2, "MARKER_ONE")
	if !strings.Contains(out, "MARKER_ONE") {
		t.Fatalf("replay missing: %q", out)
	}

	// Plain HTTP scrollback endpoint returns the same bytes.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/scrollback", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(body.String(), "MARKER_ONE") {
		t.Fatalf("scrollback endpoint: %q", body.String())
	}
}

func TestInputAndResize(t *testing.T) {
	e := newTestEnv(t, `stty -echo; while read -r line; do echo "GOT:$line SIZE:$(stty size)"; done`)
	c := e.dial(t)
	readControl(t, c, runner.MsgHello)
	ctx := context.Background()

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	// Give the resize a moment to land before sending input.
	time.Sleep(200 * time.Millisecond)
	if err := c.Write(ctx, websocket.MessageBinary, []byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, c, "GOT:ping")
	readUntil2 := out
	if !strings.Contains(readUntil2, "SIZE:30 100") {
		readUntil2 = readUntil(t, c, "SIZE:30 100")
	}
	if !strings.Contains(readUntil2, "SIZE:30 100") {
		t.Fatalf("resize not applied: %q", readUntil2)
	}
	cols, rows := e.proc.Size()
	if cols != 100 || rows != 30 {
		t.Fatalf("size = %dx%d", cols, rows)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","cols":0,"rows":0}`)); err != nil {
		t.Fatal(err)
	}
	m := readControl(t, c, runner.MsgError)
	if m.Message == "" {
		t.Fatal("expected error for invalid size")
	}
}

func TestExitAndRestart(t *testing.T) {
	e := newTestEnv(t, "echo RUN_$RANDOM; exit 3")
	c := e.dial(t)
	readControl(t, c, runner.MsgHello)
	m := readControl(t, c, runner.MsgExit)
	if m.Code == nil || *m.Code != 3 {
		t.Fatalf("exit code = %v", m.Code)
	}

	// Status reflects the exit.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var st runner.Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	_ = resp.Body.Close()
	if st.Running || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("status = %+v", st)
	}

	// A late client is told about the exit immediately.
	c2 := e.dial(t)
	readControl(t, c2, runner.MsgHello)
	m2 := readControl(t, c2, runner.MsgExit)
	if m2.Code == nil || *m2.Code != 3 {
		t.Fatalf("late exit code = %v", m2.Code)
	}

	// Restart relaunches the command; both clients see a new exit.
	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"t":"restart"}`)); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c2, "restarting agent")
	m3 := readControl(t, c2, runner.MsgExit)
	if m3.Code == nil || *m3.Code != 3 {
		t.Fatalf("exit after restart = %v", m3.Code)
	}
	if e.proc.Status("shell", 0).Restarts != 1 {
		t.Fatalf("restarts = %d", e.proc.Status("shell", 0).Restarts)
	}
}

func TestExportLogin(t *testing.T) {
	e := newTestEnv(t, "sleep 30")
	c := e.dial(t)
	readControl(t, c, runner.MsgHello)
	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"t":"export_login","kind":"claude_login"}`)); err != nil {
		t.Fatal(err)
	}
	m := readControl(t, c, runner.MsgError)
	if !strings.Contains(m.Message, "not found") {
		t.Fatalf("unexpected: %+v", m)
	}

	// With the credential file and the companion state file present, both
	// travel in one bundle; a missing companion is fine.
	home := e.home
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"tok":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"hasCompletedOnboarding":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"t":"export_login","kind":"claude_login"}`)); err != nil {
		t.Fatal(err)
	}
	m = readControl(t, c, runner.MsgLogin)
	raw, err := base64.StdEncoding.DecodeString(m.Data)
	if err != nil {
		t.Fatal(err)
	}
	b := runner.DecodeLoginBundle(runner.LoginClaude, raw)
	if string(b.Files[".claude/.credentials.json"]) != `{"tok":1}` || string(b.Files[".claude.json"]) != `{"hasCompletedOnboarding":true}` {
		t.Fatalf("bundle = %+v", b.Files)
	}
	// Legacy raw saves decode as a one-file bundle.
	legacy := runner.DecodeLoginBundle(runner.LoginClaude, []byte(`{"tok":2}`))
	if string(legacy.Files[".claude/.credentials.json"]) != `{"tok":2}` || len(legacy.Files) != 1 {
		t.Fatalf("legacy = %+v", legacy.Files)
	}
}

func TestStatusLoginUpdatedAt(t *testing.T) {
	e := newTestEnv(t, "sleep 30")
	baseline := time.Now()
	s := &Server{Proc: e.proc, Token: "secret", Agent: "claude", Home: e.home, LoginBaseline: baseline, Log: slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	status := func() runner.Status {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/status", nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st runner.Status
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// No credential file: nothing reported.
	if st := status(); st.LoginUpdatedAt != nil {
		t.Fatalf("login_updated_at without a file: %v", st.LoginUpdatedAt)
	}
	// A file seeded before the baseline (as the runner does at boot): nothing.
	cred := filepath.Join(e.home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(cred), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cred, []byte(`{"tok":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := baseline.Add(-time.Hour)
	if err := os.Chtimes(cred, old, old); err != nil {
		t.Fatal(err)
	}
	if st := status(); st.LoginUpdatedAt != nil {
		t.Fatalf("seeded file reported: %v", st.LoginUpdatedAt)
	}
	// The CLI rewrites it later: reported with the file's time.
	later := baseline.Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(cred, later, later); err != nil {
		t.Fatal(err)
	}
	st := status()
	if st.LoginUpdatedAt == nil || !st.LoginUpdatedAt.Equal(later) {
		t.Fatalf("login_updated_at = %v, want %v", st.LoginUpdatedAt, later)
	}
	// Agents without a login kind never report one.
	s.Agent = "shell"
	if st := status(); st.LoginUpdatedAt != nil {
		t.Fatalf("shell reported a login: %v", st.LoginUpdatedAt)
	}
}

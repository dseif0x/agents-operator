package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	if got := LastLine([]byte("\x1b[32mhello\x1b[0m\r\nworld \x1b[K\r\n\r\n")); got != "world" {
		t.Errorf("LastLine=%q", got)
	}
}

type testEnv struct {
	proc *Process
	srv  *httptest.Server
	tok  string
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
	s := &Server{Proc: proc, Token: "secret", Agent: "shell", Home: t.TempDir(), Log: log}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{proc: proc, srv: srv, tok: "secret"}
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
}

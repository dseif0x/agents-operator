package term

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/runner/server"
)

type staticResolver struct{ ep Endpoint }

func (s staticResolver) Endpoint(context.Context, string) (Endpoint, error) { return s.ep, nil }

// startRunner runs a real runner server around a bash script.
func startRunner(t *testing.T, script string) Endpoint {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc := server.NewProcess(func() server.Command {
		return server.Command{Path: "bash", Args: []string{"-c", script}, Env: []string{"TERM=xterm", "PATH=/usr/bin:/bin"}}
	}, log)
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Close)
	srv := httptest.NewServer((&server.Server{Proc: proc, Token: "tok", Agent: "shell", Home: t.TempDir(), Log: log}).Handler())
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	return Endpoint{IP: host, Port: p, Token: "tok"}
}

func TestProxyPipesAndFiltersControl(t *testing.T) {
	ep := startRunner(t, `echo READY; stty -echo; while read -r l; do echo "GOT:$l"; done`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := &Proxy{Resolver: staticResolver{ep}, OriginPatterns: []string{"hub.example.com"}, Log: log}
	attached := make(chan struct{}, 4)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeWS(w, r, "s1", func() { attached <- struct{}{} })
	}))
	t.Cleanup(hub.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(hub.URL, "http")

	// Wrong origin is rejected before the upgrade.
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example.com"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad origin: err=%v resp=%v", err, resp)
	}

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://hub.example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	select {
	case <-attached:
	case <-time.After(2 * time.Second):
		t.Fatal("onAttach not called")
	}

	// hello control frame and replayed output come through untouched.
	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("first frame: %v %v", typ, err)
	}
	var hello runner.Control
	_ = json.Unmarshal(data, &hello)
	if hello.T != runner.MsgHello {
		t.Fatalf("hello = %+v", hello)
	}
	readUntil(t, ctx, c, "READY")

	// export_login from a browser is dropped; resize passes; input passes.
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"export_login","kind":"claude_login"}`))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","cols":80,"rows":24}`))
	_ = c.Write(ctx, websocket.MessageBinary, []byte("hello\n"))
	out := readUntil(t, ctx, c, "GOT:hello")
	if strings.Contains(out, "login") {
		t.Fatalf("export_login leaked: %q", out)
	}

	// Status and scrollback helpers.
	st, err := proxy.Status(ctx, "s1")
	if err != nil || !st.Running || st.Cols != 80 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	sb, err := proxy.Scrollback(ctx, "s1")
	if err != nil || !bytes.Contains(sb, []byte("READY")) {
		t.Fatalf("scrollback = %q, %v", sb, err)
	}
	// Control round trip: a bad login kind yields an error reply.
	if _, err := proxy.Control(ctx, "s1", runner.Control{T: runner.MsgExportLogin, Kind: "nope"}, runner.MsgLogin); err == nil {
		t.Fatal("expected error")
	}
}

func TestProxyRunnerUnreachable(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := &Proxy{Resolver: staticResolver{Endpoint{IP: "127.0.0.1", Port: 1, Token: "x"}}, Log: log}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxy.ServeWS(w, r, "s1", nil) }))
	t.Cleanup(hub.Close)
	resp, err := http.Get(hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func readUntil(t *testing.T, ctx context.Context, c *websocket.Conn, needle string) string {
	t.Helper()
	var buf bytes.Buffer
	for {
		typ, data, err := c.Read(ctx)
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
}

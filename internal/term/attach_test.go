package term

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// A browser that attaches through the proxy takes the session over; the
// one before it gets the detached message and the CloseDetached code
// passed through (so it does not reconnect on its own), and the hub's own
// Control connections detach nobody.
func TestProxySingleAttach(t *testing.T) {
	ep := startRunner(t, `stty -echo; while read -r l; do echo "GOT:$l"; done`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := &Proxy{Resolver: staticResolver{ep}, OriginPatterns: []string{"hub.example.com"}, Log: log}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeWS(w, r, "s1", nil)
	}))
	t.Cleanup(hub.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(hub.URL, "http")
	open := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://hub.example.com"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.CloseNow() })
		return c
	}

	a := open()
	_ = a.Write(ctx, websocket.MessageBinary, []byte("one\n"))
	readUntil(t, ctx, a, "GOT:one")

	b := open()
	sawDetached := false
	for {
		typ, data, err := a.Read(ctx)
		if err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(runner.CloseDetached) {
				t.Fatalf("browser a close = %v (%v), want %d", got, err, runner.CloseDetached)
			}
			break
		}
		var m runner.Control
		if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.T == runner.MsgDetached {
			sawDetached = true
		}
	}
	if !sawDetached {
		t.Fatal("browser a got no detached message")
	}

	// A hub control round trip (no attach parameter) leaves b attached.
	if _, err := proxy.Control(ctx, "s1", runner.Control{T: runner.MsgExportLogin, Kind: "nope"}, runner.MsgLogin); err == nil {
		t.Fatal("expected error reply")
	}
	_ = b.Write(ctx, websocket.MessageBinary, []byte("two\n"))
	readUntil(t, ctx, b, "GOT:two")
}

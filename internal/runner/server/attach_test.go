package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func (e *testEnv) dialAttach(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/ws?"+runner.AttachParam+"=1", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + e.tok}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// expectDetached reads until the detached message and then the close with
// runner.CloseDetached.
func expectDetached(t *testing.T, c *websocket.Conn) {
	t.Helper()
	readControl(t, c, runner.MsgDetached)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, err := c.Read(ctx)
		cancel()
		if err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(runner.CloseDetached) {
				t.Fatalf("close status = %v (err %v), want %d", got, err, runner.CloseDetached)
			}
			return
		}
	}
}

func TestSingleAttach(t *testing.T) {
	e := newTestEnv(t, `stty -echo; while read -r line; do echo "GOT:$line"; done`)
	ctx := context.Background()

	a := e.dialAttach(t)
	readControl(t, a, runner.MsgHello)

	// The hub's own control connections (no attach parameter) detach nobody.
	ctl := e.dial(t)
	readControl(t, ctl, runner.MsgHello)
	_ = ctl.Close(websocket.StatusNormalClosure, "")
	if err := a.Write(ctx, websocket.MessageBinary, []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, a, "GOT:one")

	// A second attachment takes over: the first is told and closed.
	b := e.dialAttach(t)
	readControl(t, b, runner.MsgHello)
	expectDetached(t, a)
	if err := b.Write(ctx, websocket.MessageBinary, []byte("two\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, b, "GOT:two")

	// And the next one takes over from b in turn.
	c := e.dialAttach(t)
	readControl(t, c, runner.MsgHello)
	expectDetached(t, b)
	if err := c.Write(ctx, websocket.MessageBinary, []byte("three\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "GOT:three")
}

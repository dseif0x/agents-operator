package server

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func TestModeTracker(t *testing.T) {
	m := newModeTracker()
	// OpenCode's startup, a sequence split across chunks, and one with
	// several modes.
	m.feed([]byte("\x1b[?1049h\x1b[?25l\x1b[?2026hframe\x1b[?2026l\x1b[?10"))
	m.feed([]byte("00h\x1b[?1002h\x1b[?1006;2004h\x1b"))
	m.feed([]byte("[?1003h"))
	if !m.alt {
		t.Fatal("alternate screen not seen")
	}
	if got := string(m.restore()); got != "\x1b[?25l\x1b[?1003h\x1b[?1006h\x1b[?2004h" {
		t.Fatalf("restore = %q", got)
	}
	// Leaving: everything back to its power-on state.
	m.feed([]byte("\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?25h\x1b[?1049l"))
	if m.alt || len(m.restore()) != 0 {
		t.Fatalf("after exit: alt=%v restore=%q", m.alt, m.restore())
	}
	// Not a private mode sequence: nothing carried over.
	m.feed([]byte("text\x1b[1"))
	if m.carry != nil {
		t.Fatalf("carry = %q", m.carry)
	}
}

func TestStartsOnAltScreen(t *testing.T) {
	for _, c := range []struct {
		out  string
		alt  bool
		want bool
	}{
		{"frame", true, true},                         // switch dropped from the buffer
		{"frame", false, false},                       // a plain shell
		{"$ \x1b[?1049hframe", true, false},           // the switch is replayed
		{"frame\x1b[?1049l$ \x1b[?1049h", true, true}, // started on it, left, came back
	} {
		if got := startsOnAltScreen([]byte(c.out), c.alt); got != c.want {
			t.Errorf("%q alt=%v: %v", c.out, c.alt, got)
		}
	}
}

// A full-screen TUI that set its modes long before the client attached:
// the client is put back on the alternate screen with them, and the TUI is
// made to repaint.
func TestAttachToFullScreenApp(t *testing.T) {
	old := redrawNudge
	// Long against the script's polling below: bash runs a trap only between
	// commands, and two signals within one sleep would run it once.
	redrawNudge = 400 * time.Millisecond
	t.Cleanup(func() { redrawNudge = old })
	e := newTestEnv(t, `printf '\033[?1049h\033[?25l\033[?1000h\033[?1006h'
head -c 3000000 /dev/zero | tr '\0' x
trap 'echo "REDRAW $(stty size)"' WINCH
echo READY
while :; do sleep 0.01; done`)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(string(e.proc.Scrollback()), "READY") {
		if time.Now().After(deadline) {
			t.Fatal("no READY")
		}
		time.Sleep(20 * time.Millisecond)
	}

	c := e.dial(t)
	c.SetReadLimit(runner.ScrollbackSize + 64*1024)
	hello := readControl(t, c, runner.MsgHello)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, replay, err := c.Read(ctx)
	if err != nil || !strings.HasPrefix(string(replay), "\x1b[?1049hxxx") {
		t.Fatalf("replay starts %q (%v)", replay[:min(len(replay), 16)], err)
	}
	_, modes, err := c.Read(ctx)
	if err != nil || string(modes) != "\x1b[?25l\x1b[?1000h\x1b[?1006h" {
		t.Fatalf("modes = %q (%v)", modes, err)
	}

	// Same size as the PTY: nudged a row short and back.
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","cols":`+strconv.Itoa(hello.Cols)+`,"rows":`+strconv.Itoa(hello.Rows)+`}`)); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, c, "REDRAW "+strconv.Itoa(hello.Rows)+" "+strconv.Itoa(hello.Cols))
	if !strings.Contains(out, "REDRAW "+strconv.Itoa(hello.Rows-1)+" ") {
		t.Fatalf("no nudge: %q", out)
	}
	if cols, rows := e.proc.Size(); cols != hello.Cols || rows != hello.Rows {
		t.Fatalf("size after redraw %dx%d", cols, rows)
	}
}

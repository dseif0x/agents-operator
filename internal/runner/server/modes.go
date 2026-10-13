package server

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Full-screen TUIs (OpenCode, for one) switch the terminal into the modes
// they need once, at startup: the alternate screen, mouse reporting and its
// encoding, bracketed paste, a hidden cursor. A client that attaches later
// gets only the last ScrollbackSize bytes of output, from which those
// switches have long been dropped, and a fresh xterm.js starts with every
// mode off: the replay lands on the normal screen and clicks are never
// reported. modeTracker follows the private modes (CSI ? Pm h / l) in the
// output so an attaching client can be put into the terminal's current
// state.

// trackedModes are the modes restored on attach, with their power-on state.
// Synchronized output (2026) is left out on purpose: it is only ever on
// for the moment a frame is drawn.
var trackedModes = map[int]bool{
	1:    false, // application cursor keys
	7:    true,  // autowrap
	9:    false, // X10 mouse
	25:   true,  // cursor visible
	1000: false, // mouse: press and release
	1002: false, // mouse: drag
	1003: false, // mouse: any motion
	1004: false, // focus events
	1005: false, // mouse: UTF-8 encoding
	1006: false, // mouse: SGR encoding
	1015: false, // mouse: urxvt encoding
	1016: false, // mouse: SGR pixel encoding
	2004: false, // bracketed paste
}

// altScreenModes switch to and from the alternate screen.
var altScreenModes = map[int]bool{47: true, 1047: true, 1049: true}

var privateModeRE = regexp.MustCompile(`\x1b\[\?([0-9;]*)([hl])`)

// partialModeRE matches the start of a private mode sequence cut off at the
// end of a chunk.
var partialModeRE = regexp.MustCompile(`^\x1b(\[(\?[0-9;]*)?)?$`)

// scanModes calls fn for every private mode set or reset in b, in order.
func scanModes(b []byte, fn func(mode int, on bool)) {
	for _, m := range privateModeRE.FindAllSubmatch(b, -1) {
		on := m[2][0] == 'h'
		for p := range strings.SplitSeq(string(m[1]), ";") {
			if n, err := strconv.Atoi(p); err == nil {
				fn(n, on)
			}
		}
	}
}

type modeTracker struct {
	modes map[int]bool
	alt   bool
	carry []byte
}

func newModeTracker() *modeTracker {
	return &modeTracker{modes: map[int]bool{}}
}

// feed follows the modes set in a chunk of output. A sequence split across
// chunks is completed with the next one.
func (t *modeTracker) feed(chunk []byte) {
	data := chunk
	if len(t.carry) > 0 {
		data = append(t.carry, chunk...)
		t.carry = nil
	}
	scanModes(data, func(mode int, on bool) {
		switch {
		case altScreenModes[mode]:
			t.alt = on
		case mode == 1000 || mode == 1002 || mode == 1003 || mode == 9:
			// One mouse tracking mode at a time, as in xterm: setting one
			// replaces the others, resetting any turns tracking off.
			for _, m := range []int{9, 1000, 1002, 1003} {
				t.modes[m] = false
			}
			t.modes[mode] = on
		default:
			if _, ok := trackedModes[mode]; ok {
				t.modes[mode] = on
			}
		}
	})
	if i := bytes.LastIndexByte(data, 0x1b); i >= 0 && len(data)-i < 32 && partialModeRE.Match(data[i:]) {
		t.carry = slices.Clone(data[i:])
	}
}

// restore returns the sequence that puts a terminal fresh from a reset into
// the tracked modes' current state (alternate screen aside).
func (t *modeTracker) restore() []byte {
	var on, off []int
	for m, def := range trackedModes {
		cur, ok := t.modes[m]
		if !ok || cur == def {
			continue
		}
		if cur {
			on = append(on, m)
		} else {
			off = append(off, m)
		}
	}
	slices.Sort(on)
	slices.Sort(off)
	var b bytes.Buffer
	for _, m := range off {
		b.WriteString("\x1b[?" + strconv.Itoa(m) + "l")
	}
	for _, m := range on {
		b.WriteString("\x1b[?" + strconv.Itoa(m) + "h")
	}
	return b.Bytes()
}

// startsOnAltScreen reports whether replaying out lands on the alternate
// screen from its first byte: the screen switch that came before it was
// dropped from the buffer. That is the case when the first switch in out
// leaves the alternate screen, or when there is none and the alternate
// screen is on now.
func startsOnAltScreen(out []byte, altNow bool) bool {
	first := -1
	scanModes(out, func(mode int, on bool) {
		if first < 0 && altScreenModes[mode] {
			first = 0
			if !on {
				first = 1
			}
		}
	})
	if first < 0 {
		return altNow
	}
	return first == 1
}

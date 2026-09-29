package server

import (
	"regexp"
	"strings"
	"time"
)

// ansiRE matches, in order: CSI sequences; OSC strings; DCS, APC, PM and
// SOS strings (ESC P, ESC _, ESC ^, ESC X … ESC \); charset designations
// and other nF sequences with intermediate bytes (ESC ( B, ESC # 8); every
// other two-byte escape (ESC =, ESC >, ESC 7, ESC c …); and a dangling ESC
// at the end of a buffer. TUIs emit ESC ( B constantly, and without the nF
// branch "(B" leaked into the session list.
var ansiRE = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[P_^X][^\x1b]*\x1b\\|\x1b[ -/]+[0-~]|\x1b[0-~]|\x1b`)

// StripANSI removes terminal escape sequences from s.
func StripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

// promptRE matches the tail of a screen that is very likely waiting for a
// human: a shell or agent prompt character, a yes/no question, or a
// question mark at the end of the last visible line.
var promptRE = regexp.MustCompile(`(?i)(\?|\(y/n\)|\[y/n\]|\[yes/no\]|>|❯|›|\$|#|%|:)\s*$`)

// LastLine returns the last non-empty visible line of raw terminal output.
func LastLine(raw []byte) string {
	s := StripANSI(string(raw))
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" {
			return l
		}
	}
	return ""
}

// QuietFor is how long the PTY must have been silent before a prompt-looking
// tail counts as "needs attention".
const QuietFor = 2 * time.Second

// NeedsAttention reports whether the tail of the output looks like a prompt
// and the agent has been silent for at least QuietFor.
func NeedsAttention(tail string, lastOutput time.Time, now time.Time) bool {
	if tail == "" || lastOutput.IsZero() {
		return false
	}
	if now.Sub(lastOutput) < QuietFor {
		return false
	}
	return promptRE.MatchString(tail)
}

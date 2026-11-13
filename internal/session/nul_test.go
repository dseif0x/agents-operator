package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner/convo"
)

func TestStripNUL(t *testing.T) {
	in, _ := json.Marshal(map[string]any{
		"events": []any{map[string]any{"type": "item", "item": map[string]any{"output": "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR", "n": 12345678901234567}}},
	})
	if !strings.Contains(string(in), `\u0000`) {
		t.Fatalf("test input has no NUL escape: %s", in)
	}
	out := stripNUL(in)
	if strings.Contains(string(out), `\u0000`) {
		t.Fatalf("NUL survived: %s", out)
	}
	if !strings.Contains(string(out), `IHDR`) || !strings.Contains(string(out), `12345678901234567`) {
		t.Fatalf("content lost: %s", out)
	}
	var a struct {
		Events []convo.Event `json:"events"`
	}
	if err := json.Unmarshal(out, &a); err != nil || len(a.Events) != 1 {
		t.Fatalf("does not decode as events: %v %s", err, out)
	}
}

func TestStripNULUntouched(t *testing.T) {
	for _, s := range []string{`{"a":"plain"}`, `{"a":"literal \\u0000 text"}`, `not json \u0000`} {
		out := stripNUL(json.RawMessage(s))
		var want, got any
		if json.Unmarshal([]byte(s), &want) != nil {
			if string(out) != s {
				t.Errorf("invalid JSON changed: %q -> %q", s, out)
			}
			continue
		}
		_ = json.Unmarshal(out, &got)
		wb, _ := json.Marshal(want)
		gb, _ := json.Marshal(got)
		if string(wb) != string(gb) {
			t.Errorf("%s changed to %s", s, out)
		}
	}
}

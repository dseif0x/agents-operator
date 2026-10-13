package session

import (
	"testing"

	"github.com/dseif0x/agents-operator/internal/store"
)

func TestNormaliseOpenClaudeBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		" https://unjailedai.com/v1/ ": "https://unjailedai.com/v1",
		"http://10.1.2.3:8000/v1":      "http://10.1.2.3:8000/v1",
	} {
		got, err := normalise(store.CredUnjailedBaseURL, []byte(in))
		if err != nil || string(got) != want {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"unjailedai.com/v1", "ftp://x/v1", "https://"} {
		if _, err := normalise(store.CredUnjailedBaseURL, []byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// The key is a token: pasted line breaks and spaces go.
	if got, _ := normalise(store.CredUnjailedAPIKey, []byte(" sk-uj\n123 ")); string(got) != "sk-uj123" {
		t.Errorf("key = %q", got)
	}
}

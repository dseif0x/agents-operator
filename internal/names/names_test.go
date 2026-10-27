package names

import "testing"

func TestNew(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		h := New(func(s string) bool { return seen[s] })
		if !Pattern.MatchString(h) || seen[h] {
			t.Fatalf("bad or duplicate handle %q", h)
		}
		seen[h] = true
	}
	// Everything taken without a number: a numbered handle comes back.
	h := New(func(s string) bool {
		return !Pattern.MatchString(s) || len(s) < 3 || s[len(s)-1] < '0' || s[len(s)-1] > '9'
	})
	if !Pattern.MatchString(h) {
		t.Fatalf("numbered handle %q", h)
	}
}

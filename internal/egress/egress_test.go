package egress

import (
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]struct {
		str, cidr string
		port      uint16
	}{
		"192.168.1.10":          {"192.168.1.10", "192.168.1.10/32", 0},
		" 192.168.1.10:8080 ":   {"192.168.1.10:8080", "192.168.1.10/32", 8080},
		"192.168.1.77/24":       {"192.168.1.0/24", "192.168.1.0/24", 0},
		"192.168.1.0/24:443":    {"192.168.1.0/24:443", "192.168.1.0/24", 443},
		"192.168.1.10/32":       {"192.168.1.10", "192.168.1.10/32", 0},
		"fd00::5":               {"fd00::5", "fd00::5/128", 0},
		"[fd00::5]:22":          {"[fd00::5]:22", "fd00::5/128", 22},
		"[fd00::/64]:22":        {"[fd00::/64]:22", "fd00::/64", 22},
		"[fd00::5]":             {"fd00::5", "fd00::5/128", 0},
		"fd00:0:0:0:0:0:0:5/64": {"fd00::/64", "fd00::/64", 0},
		"::ffff:192.168.1.1":    {"192.168.1.1", "192.168.1.1/32", 0},
	} {
		r, err := Parse(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if r.String() != want.str || r.CIDR() != want.cidr || r.Port != want.port {
			t.Errorf("%q = %s / %s / %d, want %+v", in, r, r.CIDR(), r.Port, want)
		}
		// The canonical form parses back to the same rule.
		if again, err := Parse(r.String()); err != nil || again != r {
			t.Errorf("%q does not round-trip: %v %v", r, again, err)
		}
	}
	for _, bad := range []string{"", "nas.local", "192.168.1.300", "192.168.1.10:0", "192.168.1.10:70000", "192.168.1.10:http",
		"192.168.1.0/33", "[fd00::5", "[fd00::5]x", "fe80::1%eth0", "1.2.3.4:5:6"} {
		if r, err := Parse(bad); err == nil {
			t.Errorf("%q accepted as %s", bad, r)
		}
	}
}

func TestCheck(t *testing.T) {
	allowed, err := ParsePrefixes([]string{"192.168.0.0/16", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	denied, _ := ParsePrefixes([]string{"10.42.0.0/16", "10.43.0.0/16"})
	p := Policy{Allowed: allowed, Denied: denied}

	got, err := p.Check([]string{"192.168.1.10:8080", "192.168.1.10:8080", "10.1.2.3", "192.168.2.0/24"})
	if err != nil || !slices.Equal(got, []string{"192.168.1.10:8080", "10.1.2.3", "192.168.2.0/24"}) {
		t.Fatalf("Check = %v, %v", got, err)
	}
	if got, err := p.Check(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for in, msg := range map[string]string{
		"8.8.8.8":        "outside",
		"192.168.0.0/15": "outside", // wider than the allowed range
		"10.42.0.7":      "never reach",
		"10.0.0.0/8":     "never reach", // contains a denied range
		"10.43.0.1:443":  "never reach",
		"nas":            "not an address",
	} {
		if _, err := p.Check([]string{in}); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: err = %v, want %q", in, err, msg)
		}
	}
	many := make([]string, MaxRules+1)
	for i := range many {
		many[i] = "192.168.1.1"
	}
	if _, err := p.Check(many); err == nil {
		t.Fatal("too many destinations accepted")
	}
	// A hub with nothing allowed refuses everything but an empty list.
	if _, err := (Policy{}).Check([]string{"192.168.1.1"}); err == nil {
		t.Fatal("disabled policy accepted a destination")
	}
	if _, err := (Policy{}).Check(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePrefixes([]string{"10.0.0.0/8", "x"}); err == nil {
		t.Fatal("bad CIDR accepted")
	}
}

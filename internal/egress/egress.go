// Package egress parses and checks the extra destinations a session may
// reach on the local network: an address or CIDR, optionally with a port
// (`192.168.1.10`, `192.168.1.0/24`, `192.168.1.10:8080`, `[fd00::5]:22`).
// The hub opens them with a per-session NetworkPolicy on top of the
// chart's default-deny one.
package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// MaxRules caps the destinations per session.
const MaxRules = 20

// Rule is one destination: a prefix and, when Port is not 0, a single port
// (TCP and UDP). Without a port every port is open.
type Rule struct {
	Prefix netip.Prefix
	Port   uint16
}

// String is the canonical form: a single address without /32 (or /128),
// IPv6 in brackets when a port follows.
func (r Rule) String() string {
	host := r.Prefix.String()
	if r.Prefix.IsSingleIP() {
		host = r.Prefix.Addr().String()
	}
	if r.Port == 0 {
		return host
	}
	if r.Prefix.Addr().Is6() {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(int(r.Port))
}

// CIDR is the prefix in CIDR notation, as a NetworkPolicy ipBlock wants it.
func (r Rule) CIDR() string { return r.Prefix.String() }

// Parse reads one destination.
func Parse(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, errors.New("empty destination")
	}
	host, port := s, ""
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.Index(s, "]")
		if end < 0 {
			return Rule{}, fmt.Errorf("%q: missing ]", s)
		}
		host, port = s[1:end], strings.TrimPrefix(s[end+1:], ":")
		if rest := s[end+1:]; rest != "" && !strings.HasPrefix(rest, ":") {
			return Rule{}, fmt.Errorf("%q: unexpected text after ]", s)
		}
	case strings.Count(s, ":") == 1:
		host, port, _ = strings.Cut(s, ":")
	}
	var r Rule
	if strings.Contains(host, "/") {
		p, err := netip.ParsePrefix(host)
		if err != nil {
			return Rule{}, fmt.Errorf("%q: not an address or CIDR", s)
		}
		r.Prefix = p.Masked()
	} else {
		a, err := netip.ParseAddr(host)
		if err != nil || a.Zone() != "" {
			return Rule{}, fmt.Errorf("%q: not an address or CIDR", s)
		}
		a = a.Unmap() // ::ffff:192.168.1.1 is 192.168.1.1
		r.Prefix = netip.PrefixFrom(a, a.BitLen())
	}
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return Rule{}, fmt.Errorf("%q: port must be 1-65535", s)
		}
		r.Port = uint16(n)
	}
	return r, nil
}

// Policy is what a hub lets sessions open: destinations must lie inside one
// of Allowed and must not touch any of Denied (the cluster's own ranges).
// An empty Allowed turns the feature off.
type Policy struct {
	Allowed []netip.Prefix
	Denied  []netip.Prefix
}

// ParsePrefixes reads a list of CIDRs, for the hub configuration.
func ParsePrefixes(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR", s)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// Enabled reports whether sessions may open anything at all.
func (p Policy) Enabled() bool { return len(p.Allowed) > 0 }

// Check validates and normalises a session's destinations: each one parsed,
// inside an allowed range, clear of every denied one, duplicates dropped.
// The result is in canonical form, in the order given.
func (p Policy) Check(entries []string) ([]string, error) {
	if len(entries) == 0 {
		return []string{}, nil
	}
	if !p.Enabled() {
		return nil, errors.New("this hub does not let sessions open local network addresses")
	}
	if len(entries) > MaxRules {
		return nil, fmt.Errorf("at most %d destinations", MaxRules)
	}
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		r, err := Parse(e)
		if err != nil {
			return nil, err
		}
		if !p.allows(r.Prefix) {
			return nil, fmt.Errorf("%s is outside the ranges sessions may open (%s)", r, joinPrefixes(p.Allowed))
		}
		for _, d := range p.Denied {
			if d.Overlaps(r.Prefix) {
				return nil, fmt.Errorf("%s overlaps %s, which sessions may never reach", r, d)
			}
		}
		if s := r.String(); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// allows reports whether prefix lies entirely inside one allowed range.
func (p Policy) allows(prefix netip.Prefix) bool {
	for _, a := range p.Allowed {
		if a.Bits() <= prefix.Bits() && a.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func joinPrefixes(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, ", ")
}

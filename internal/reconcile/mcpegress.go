package reconcile

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/agents-operator/internal/store"
)

// MCP servers on the local network. The chart's NetworkPolicy keeps
// session pods off the cluster and the LAN, so a remote MCP server there
// (Home Assistant, a server in the cluster) is unreachable until something
// opens it. With Config.MCPAutoAllow the hub opens exactly the servers a
// session picked, in that session's own NetworkPolicy (egress.go):
//
//   - an in-cluster Service (`name.namespace`, `name.namespace.svc`,
//     `name.namespace.svc.cluster.local`): its pods, by the Service's
//     selector, on the target port behind the URL's port. NetworkPolicies
//     see traffic after the Service has forwarded it, so the Service IP
//     itself would never match.
//   - an address, or a host name that resolves to one: that address on the
//     URL's port.
//
// A name that is not a Service is resolved as a host. Hosts behind an
// ingress controller (a LoadBalancer IP forwarded to its pods) are not
// covered: their traffic reaches the controller's pods, not the address.

// lookupHost resolves a host name; tests replace it.
var lookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// mcpEgress is the result for one server: the rules, or why it got none.
type mcpEgress struct {
	Name  string
	Rules []networkingv1.NetworkPolicyEgressRule
	Err   error
}

// mcpEgressRules resolves the MCP servers a session picked. A server that
// cannot be resolved gets no rules and an error the caller reports; the
// session still starts, since the agent works without that server.
func (r *Reconciler) mcpEgressRules(ctx context.Context, sess *store.Session) ([]mcpEgress, error) {
	if !r.cfg.MCPAutoAllow || len(sess.MCPServers) == 0 {
		return nil, nil
	}
	uc, err := r.userCredentials(ctx, sess.OwnerID)
	if err != nil {
		return nil, err
	}
	var out []mcpEgress
	for _, id := range sess.MCPServers {
		for _, a := range uc.usable() {
			if a.ID != id || a.Kind != store.AcctMCPServer || a.BaseURL == "" {
				continue
			}
			rules, err := r.mcpRulesFor(ctx, a.BaseURL)
			out = append(out, mcpEgress{Name: a.Name, Rules: rules, Err: err})
		}
	}
	return out, nil
}

// mcpRulesFor turns one MCP server URL into egress rules.
func (r *Reconciler) mcpRulesFor(ctx context.Context, raw string) ([]networkingv1.NetworkPolicyEgressRule, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("%q is not a URL", raw)
	}
	port, err := urlPort(u)
	if err != nil {
		return nil, err
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if a, err := netip.ParseAddr(host); err == nil {
		return []networkingv1.NetworkPolicyEgressRule{addrRule(a, port)}, nil
	}
	if name, ns, ok := r.serviceName(host); ok {
		rule, found, err := r.serviceRule(ctx, name, ns, port)
		if err != nil {
			return nil, err
		}
		if found {
			return []networkingv1.NetworkPolicyEgressRule{rule}, nil
		}
		if strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") {
			return nil, fmt.Errorf("service %s/%s not found", ns, name)
		}
	}
	addrs, err := lookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	rules := make([]networkingv1.NetworkPolicyEgressRule, 0, len(addrs))
	seen := map[netip.Addr]bool{}
	for _, a := range addrs {
		a = a.Unmap()
		if seen[a] {
			continue
		}
		seen[a] = true
		rules = append(rules, addrRule(a, port))
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	return rules, nil
}

// urlPort is the URL's port, or its scheme's default.
func urlPort(u *url.URL) (int32, error) {
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("bad port %q", p)
		}
		return int32(n), nil
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return 80, nil
	case "https":
		return 443, nil
	}
	return 0, fmt.Errorf("no port in %q", u.String())
}

// serviceName reads a host as a Service: `name.namespace` with optional
// `.svc` or `.svc.cluster.local`, or a bare `name` in the hub's namespace.
func (r *Reconciler) serviceName(host string) (name, ns string, ok bool) {
	h := strings.TrimSuffix(strings.TrimSuffix(host, ".cluster.local"), ".svc")
	parts := strings.Split(h, ".")
	switch len(parts) {
	case 1:
		return parts[0], r.cfg.Namespace, true
	case 2:
		return parts[0], parts[1], true
	}
	return "", "", false
}

// serviceRule allows the pods behind a Service's port. found is false when
// there is no such Service, so the caller can try the name as a host.
func (r *Reconciler) serviceRule(ctx context.Context, name, ns string, port int32) (networkingv1.NetworkPolicyEgressRule, bool, error) {
	svc, err := r.cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return networkingv1.NetworkPolicyEgressRule{}, false, nil
	case apierrors.IsForbidden(err):
		return networkingv1.NetworkPolicyEgressRule{}, false, fmt.Errorf("the hub may not read services (chart: runner.networkPolicy.mcpAutoAllow grants it): %w", err)
	case err != nil:
		return networkingv1.NetworkPolicyEgressRule{}, false, err
	}
	if len(svc.Spec.Selector) == 0 {
		return networkingv1.NetworkPolicyEgressRule{}, false, fmt.Errorf("service %s/%s has no pod selector", ns, name)
	}
	var target *intstr.IntOrString
	for _, p := range svc.Spec.Ports {
		if p.Port != port || (p.Protocol != "" && p.Protocol != corev1.ProtocolTCP) {
			continue
		}
		t := p.TargetPort
		if t.Type == intstr.Int && t.IntVal == 0 {
			t = intstr.FromInt32(p.Port) // unset: the same as the port
		}
		target = &t
	}
	if target == nil {
		return networkingv1.NetworkPolicyEgressRule{}, false, fmt.Errorf("service %s/%s has no TCP port %d", ns, name, port)
	}
	selector := make(map[string]string, len(svc.Spec.Selector))
	for k, v := range svc.Spec.Selector {
		selector[k] = v
	}
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: selector},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: target}},
	}, true, nil
}

// addrRule allows one address on one TCP port.
func addrRule(a netip.Addr, port int32) networkingv1.NetworkPolicyEgressRule {
	a = a.Unmap()
	p := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: netip.PrefixFrom(a, a.BitLen()).String()}}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: &p}},
	}
}

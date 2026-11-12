package reconcile

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/dseif0x/agents-operator/internal/store"
)

func stubLookup(t *testing.T, m map[string][]netip.Addr) {
	t.Helper()
	old := lookupHost
	lookupHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		if a, ok := m[host]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { lookupHost = old })
}

func haService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "home-assistant", Namespace: "home-assistant"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/name": "home-assistant"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8123), Protocol: corev1.ProtocolTCP},
				{Name: "named", Port: 9090, TargetPort: intstr.FromString("metrics")},
				{Name: "same", Port: 7000},
			},
		},
	}
}

func TestMCPRulesForService(t *testing.T) {
	h := newHarness(t)
	if _, err := h.cs.CoreV1().Services("home-assistant").Create(h.ctx, haService(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	stubLookup(t, nil)
	for _, tc := range []struct {
		url    string
		target intstr.IntOrString
	}{
		{"http://home-assistant.home-assistant:8080/api/mcp", intstr.FromInt32(8123)},
		{"http://home-assistant.home-assistant.svc:8080/api/mcp", intstr.FromInt32(8123)},
		{"http://Home-Assistant.home-assistant.svc.cluster.local.:8080/x", intstr.FromInt32(8123)},
		{"http://home-assistant.home-assistant:9090/", intstr.FromString("metrics")},
		{"http://home-assistant.home-assistant:7000/", intstr.FromInt32(7000)},
	} {
		rules, err := h.r.mcpRulesFor(h.ctx, tc.url)
		if err != nil || len(rules) != 1 {
			t.Fatalf("%s: %v %v", tc.url, rules, err)
		}
		r := rules[0]
		peer := r.To[0]
		if peer.IPBlock != nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "home-assistant" ||
			peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "home-assistant" || len(peer.PodSelector.MatchLabels) != 1 {
			t.Fatalf("%s: peer = %+v", tc.url, peer)
		}
		if len(r.Ports) != 1 || *r.Ports[0].Port != tc.target || *r.Ports[0].Protocol != corev1.ProtocolTCP {
			t.Fatalf("%s: ports = %+v", tc.url, r.Ports)
		}
	}
	// A port the Service does not have, a .svc name that does not exist.
	for _, u := range []string{"http://home-assistant.home-assistant:1234/", "http://nope.home-assistant.svc:80/"} {
		if _, err := h.r.mcpRulesFor(h.ctx, u); err == nil {
			t.Fatalf("%s: no error", u)
		}
	}
}

func TestMCPRulesForAddressesAndHosts(t *testing.T) {
	h := newHarness(t)
	stubLookup(t, map[string][]netip.Addr{
		"nas.lan":    {netip.MustParseAddr("10.13.2.2"), netip.MustParseAddr("10.13.2.2"), netip.MustParseAddr("::ffff:10.13.2.3")},
		"ha.example": {netip.MustParseAddr("192.168.1.7")},
	})
	cases := map[string][]string{
		"http://10.13.1.5:8123/api/mcp": {"10.13.1.5/32:8123"},
		"https://[fd00::5]/mcp":         {"fd00::5/128:443"},
		"http://nas.lan/mcp":            {"10.13.2.2/32:80", "10.13.2.3/32:80"},
		// Two labels but no such Service: resolved as a host.
		"https://ha.example:8443/mcp": {"192.168.1.7/32:8443"},
	}
	for u, want := range cases {
		rules, err := h.r.mcpRulesFor(h.ctx, u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		var got []string
		for _, r := range rules {
			got = append(got, r.To[0].IPBlock.CIDR+":"+r.Ports[0].Port.String())
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: %v, want %v", u, got, want)
		}
	}
	for _, u := range []string{"http://unknown.host.example/", "ftp://10.0.0.1/", "not a url", "http://10.0.0.1:0/"} {
		if _, err := h.r.mcpRulesFor(h.ctx, u); err == nil {
			t.Fatalf("%s: no error", u)
		}
	}
}

func TestMCPAutoAllowOpensPickedServers(t *testing.T) {
	h := newHarness(t)
	h.r.cfg.NetworkPolicy = true
	h.r.cfg.MCPAutoAllow = true
	if _, err := h.cs.CoreV1().Services("home-assistant").Create(h.ctx, haService(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	stubLookup(t, nil)
	for _, a := range []*store.Account{
		{ID: "ha", UserID: "u1", Kind: store.AcctMCPServer, Name: "Home Assistant", BaseURL: "http://home-assistant.home-assistant:8080/api/mcp"},
		{ID: "broken", UserID: "u1", Kind: store.AcctMCPServer, Name: "Gone", BaseURL: "http://gone.nowhere.svc:80/mcp"},
		{ID: "unpicked", UserID: "u1", Kind: store.AcctMCPServer, Name: "Other", BaseURL: "http://10.13.9.9:80/mcp"},
	} {
		if err := h.st.Accounts().Create(h.ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.cs.CoreV1().Secrets(h.ns).Create(h.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: UserSecretName("u1"), Namespace: h.ns},
		Data: map[string][]byte{store.AccountSecretKey("ha"): []byte("t1"), store.AccountSecretKey("broken"): []byte("t2"),
			store.AccountSecretKey("unpicked"): []byte("t3")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s := &store.Session{OwnerID: "u1", Name: "mcp", Agent: "shell", State: store.StateCreating, PVCSize: "5Gi",
		MCPServers: []string{"ha", "broken"}}
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	h.converge(s.ID)
	p := h.egressPolicy(s.ID)
	if p == nil {
		t.Fatal("no policy for a session with an MCP server")
	}
	// Only the picked, resolvable server; the broken one is reported, not fatal.
	if len(p.Spec.Egress) != 1 || p.Spec.Egress[0].To[0].NamespaceSelector == nil {
		t.Fatalf("egress = %+v", p.Spec.Egress)
	}
	if got := h.session(s.ID); got == nil || got.State == store.StateFailed {
		t.Fatalf("session = %+v", got)
	}

	// Switched off: no policy for MCP servers alone.
	h2 := newHarness(t)
	h2.r.cfg.NetworkPolicy = true
	s2 := &store.Session{OwnerID: "u1", Name: "mcp", Agent: "shell", State: store.StateCreating, PVCSize: "5Gi", MCPServers: []string{"ha"}}
	if err := h2.st.Sessions().Create(h2.ctx, s2); err != nil {
		t.Fatal(err)
	}
	h2.converge(s2.ID)
	if h2.egressPolicy(s2.ID) != nil {
		t.Fatal("policy without MCPAutoAllow")
	}
}

func TestMCPRulesForHostNetworkService(t *testing.T) {
	// Home Assistant on the host network: its endpoint is the node's own
	// address, which pod selectors do not match, so it is opened as an address.
	h := newHarness(t)
	svc := haService()
	if _, err := h.cs.CoreV1().Services("home-assistant").Create(h.ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cs.CoreV1().Nodes().Create(h.ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "brute"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.13.1.187"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	name, port := "http", int32(8123)
	tcp := corev1.ProtocolTCP
	if _, err := h.cs.DiscoveryV1().EndpointSlices("home-assistant").Create(h.ctx, &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "ha-x", Labels: map[string]string{discoveryv1.LabelServiceName: "home-assistant"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.13.1.187"}}, {Addresses: []string{"10.42.3.9"}}},
		Ports:       []discoveryv1.EndpointPort{{Name: &name, Port: &port, Protocol: &tcp}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rules, err := h.r.mcpRulesFor(h.ctx, "http://home-assistant.home-assistant:8080/api/mcp")
	if err != nil {
		t.Fatal(err)
	}
	// The selector rule plus the node address; the pod address stays a selector match.
	if len(rules) != 2 || rules[0].To[0].PodSelector == nil || rules[1].To[0].IPBlock == nil ||
		rules[1].To[0].IPBlock.CIDR != "10.13.1.187/32" || rules[1].Ports[0].Port.IntValue() != 8123 {
		t.Fatalf("rules = %+v", rules)
	}
}

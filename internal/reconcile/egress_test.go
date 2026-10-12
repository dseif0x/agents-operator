package reconcile

import (
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

func TestBuildEgressPolicy(t *testing.T) {
	s := testSession()
	if p, err := BuildEgressPolicy(s, testCfg()); p != nil || err != nil {
		t.Fatalf("no destinations: %v %v", p, err)
	}
	s.Egress = []string{"192.168.1.10", "192.168.2.0/24:8080"}
	p, err := BuildEgressPolicy(s, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != ObjectName(s.ID) || p.Labels[k8s.LabelSession] != s.ID || p.Spec.PodSelector.MatchLabels[k8s.LabelSession] != s.ID || len(p.Spec.PodSelector.MatchLabels) != 1 {
		t.Fatalf("policy meta = %+v / %+v", p.ObjectMeta, p.Spec.PodSelector)
	}
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(p.Spec.Ingress) != 0 {
		t.Fatalf("policy types = %v", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Egress) != 2 {
		t.Fatalf("egress = %+v", p.Spec.Egress)
	}
	// A bare address: every port.
	if r := p.Spec.Egress[0]; r.To[0].IPBlock.CIDR != "192.168.1.10/32" || len(r.Ports) != 0 {
		t.Fatalf("rule 0 = %+v", r)
	}
	// With a port: that port, TCP and UDP.
	r := p.Spec.Egress[1]
	if r.To[0].IPBlock.CIDR != "192.168.2.0/24" || len(r.Ports) != 2 || r.Ports[0].Port.IntValue() != 8080 ||
		*r.Ports[0].Protocol != corev1.ProtocolTCP || *r.Ports[1].Protocol != corev1.ProtocolUDP {
		t.Fatalf("rule 1 = %+v", r)
	}
	s.Egress = []string{"not-an-ip"}
	if _, err := BuildEgressPolicy(s, testCfg()); err == nil {
		t.Fatal("bad destination built")
	}
}

func TestEgressEnv(t *testing.T) {
	s := testSession()
	s.Egress = []string{"192.168.1.10", "192.168.2.0/24:8080"}
	cfg := testCfg()
	envOf := func() string {
		for _, e := range BuildPod(s, cfg).Spec.Containers[0].Env {
			if e.Name == runner.EnvEgress {
				return e.Value
			}
		}
		return "<missing>"
	}
	// Without the chart's policy nothing was opened, so nothing is claimed.
	if v := envOf(); v != "" {
		t.Fatalf("EGRESS without network policy = %q", v)
	}
	cfg.NetworkPolicy = true
	if v := envOf(); v != "192.168.1.10,192.168.2.0/24:8080" {
		t.Fatalf("EGRESS = %q", v)
	}
}

func (h *harness) egressPolicy(id string) *networkingv1.NetworkPolicy {
	p, err := h.cs.NetworkingV1().NetworkPolicies(h.ns).Get(h.ctx, ObjectName(id), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return p
}

func TestEgressPolicyLifecycle(t *testing.T) {
	h := newHarness(t)
	h.r.cfg.NetworkPolicy = true
	s := &store.Session{OwnerID: "u1", Name: "lan", Agent: "shell", State: store.StateCreating, PVCSize: "5Gi",
		Egress: []string{"192.168.1.10:8080"}}
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	h.converge(s.ID)
	p := h.egressPolicy(s.ID)
	if p == nil || len(p.Spec.Egress) != 1 || p.Spec.Egress[0].To[0].IPBlock.CIDR != "192.168.1.10/32" {
		t.Fatalf("policy after start = %+v", p)
	}

	// Stop, edit the list, start again: the policy follows.
	stopAndRestart := func(egress []string) {
		t.Helper()
		if _, err := h.st.Sessions().SetState(h.ctx, s.ID, store.StateStopping, ""); err != nil {
			t.Fatal(err)
		}
		h.reconcile(s.ID)
		h.eventually("pod gone", func() bool { return h.r.observe(s.ID).pod == nil })
		h.reconcile(s.ID)
		cur := h.session(s.ID)
		cur.Egress = egress
		if _, err := h.st.Sessions().Update(h.ctx, cur); err != nil {
			t.Fatal(err)
		}
		bumped, err := h.st.Sessions().Bump(h.ctx, s.ID, store.StateCreating)
		if err != nil {
			t.Fatal(err)
		}
		gen := strconv.Itoa(bumped.Generation)
		h.reconcile(s.ID) // rotates the secret
		h.eventually("secret rotated in cache", func() bool {
			sec := h.r.observe(s.ID).secret
			return sec != nil && sec.Annotations[k8s.AnnotationGeneration] == gen
		})
		h.reconcile(s.ID) // syncs the policy, creates the pod
		h.eventually("pod in cache", func() bool { return h.r.observe(s.ID).pod != nil })
		h.markReady(s.ID)
		h.reconcile(s.ID)
	}
	stopAndRestart([]string{"192.168.1.10:8080", "10.1.0.0/16"})
	if p := h.egressPolicy(s.ID); p == nil || len(p.Spec.Egress) != 2 || p.Spec.Egress[1].To[0].IPBlock.CIDR != "10.1.0.0/16" {
		t.Fatalf("policy after edit = %+v", p)
	}
	stopAndRestart(nil)
	if h.egressPolicy(s.ID) != nil {
		t.Fatal("policy kept after the list was emptied")
	}

	// Delete takes it along.
	stopAndRestart([]string{"192.168.1.10"})
	if h.egressPolicy(s.ID) == nil {
		t.Fatal("policy not recreated")
	}
	if _, err := h.st.Sessions().SetState(h.ctx, s.ID, store.StateDeleting, ""); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	h.eventually("objects gone", func() bool {
		o := h.r.observe(s.ID)
		return o.pod == nil && o.pvc == nil && o.secret == nil
	})
	h.reconcile(s.ID)
	if h.session(s.ID) != nil || h.egressPolicy(s.ID) != nil {
		t.Fatal("policy or row left after delete")
	}
}

func TestEgressPolicyNeedsChartPolicy(t *testing.T) {
	// Without the chart's default-deny policy the hub creates none: alone,
	// an egress policy would cut the pod off instead of opening anything.
	h := newHarness(t)
	s := &store.Session{OwnerID: "u1", Name: "lan", Agent: "shell", State: store.StateCreating, PVCSize: "5Gi",
		Egress: []string{"192.168.1.10"}}
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	h.converge(s.ID)
	if h.egressPolicy(s.ID) != nil {
		t.Fatal("egress policy created without the chart's policy")
	}
	if list, _ := h.cs.NetworkingV1().NetworkPolicies(h.ns).List(h.ctx, metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("policies = %v", list.Items)
	}
}

func TestOrphanedEgressPolicyIsRemoved(t *testing.T) {
	h := newHarness(t)
	h.r.cfg.NetworkPolicy = true
	ghost := &store.Session{ID: "99999999-9999-4999-8999-999999999997", OwnerID: "ghost", Name: "ghost", Agent: "shell", Egress: []string{"192.168.1.10"}}
	p, err := BuildEgressPolicy(ghost, h.r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.CreationTimestamp = metav1.Now()
	if _, err := h.cs.NetworkingV1().NetworkPolicies(h.ns).Create(h.ctx, p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.r.ReconcileAll(h.ctx)
	found := false
	for h.r.queue.Len() > 0 {
		id, _ := h.r.queue.Get()
		found = found || id == ghost.ID
		h.r.queue.Done(id)
	}
	if !found {
		t.Fatal("orphaned policy not enqueued")
	}
	h.reconcile(ghost.ID)
	if h.egressPolicy(ghost.ID) == nil {
		t.Fatal("orphan removed too early")
	}
	h.r.now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	h.reconcile(ghost.ID)
	if h.egressPolicy(ghost.ID) != nil {
		t.Fatal("orphaned policy not removed")
	}
}

func TestEgressPolicyRejectedFailsSession(t *testing.T) {
	h := newHarness(t)
	h.r.cfg.NetworkPolicy = true
	// The service never stores this; a row edited by hand might.
	s := &store.Session{OwnerID: "u1", Name: "lan", Agent: "shell", State: store.StateCreating, PVCSize: "5Gi", Egress: []string{"nas.local"}}
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	h.eventually("pvc and secret in cache", func() bool {
		o := h.r.observe(s.ID)
		return o.pvc != nil && o.secret != nil
	})
	h.reconcile(s.ID)
	got := h.session(s.ID)
	if got.State != store.StateFailed || !strings.Contains(got.StateReason, "local network access") || h.pod(s.ID) != nil {
		t.Fatalf("session = %s %q, pod %v", got.State, got.StateReason, h.pod(s.ID) != nil)
	}
}

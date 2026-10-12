package reconcile

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/agents-operator/internal/egress"
	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Local network access per session. The chart's NetworkPolicy keeps session
// pods off private ranges; a session that lists destinations (store.Session
// Egress) gets a NetworkPolicy of its own that selects only its pod and
// allows exactly those, on every port or the one given. NetworkPolicies
// add up, so it widens the chart's policy for that pod and nothing else.
// The hub only does this when the chart's policy exists (Config.NetworkPolicy):
// alone, an egress policy would isolate the pod instead. The service has
// already checked the destinations against the hub's allowed and denied
// ranges. The policy follows the session (synced on every start, removed on
// delete) and is found again by its session label.

// BuildEgressPolicy returns the session's NetworkPolicy, or nil when it
// lists no destinations and has no sandbox cluster to reach.
func BuildEgressPolicy(s *store.Session, cfg Config) (*networkingv1.NetworkPolicy, error) {
	access, _ := effectiveAccess(s, cfg.Defaults())
	sandbox := access == store.K8sAccessSandbox
	if len(s.Egress) == 0 && !sandbox {
		return nil, nil
	}
	rules := make([]networkingv1.NetworkPolicyEgressRule, 0, len(s.Egress)+1)
	if sandbox {
		// The vcluster API server, behind its service's 443.
		port := intstr.FromInt32(8443)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": SandboxNamespace(s.ID, cfg.Defaults())}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": SandboxRelease, "release": SandboxRelease}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: &port}},
		})
	}
	for _, e := range s.Egress {
		r, err := egress.Parse(e)
		if err != nil {
			return nil, err
		}
		rule := networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: r.CIDR()}}},
		}
		if r.Port != 0 {
			port := intstr.FromInt32(int32(r.Port))
			rule.Ports = []networkingv1.NetworkPolicyPort{
				{Protocol: ptr.To(corev1.ProtocolTCP), Port: &port},
				{Protocol: ptr.To(corev1.ProtocolUDP), Port: &port},
			}
		}
		rules = append(rules, rule)
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName(s.ID), Namespace: cfg.Namespace, Labels: Labels(s)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{k8s.LabelSession: s.ID}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      rules,
		},
	}, nil
}

// egressError marks a failure the session owner has to fix (a destination
// the API server rejects, rights the hub lacks); anything else is retried.
type egressError struct{ msg string }

func (e *egressError) Error() string { return e.msg }

// ensureEgress converges the session's NetworkPolicy to its destinations;
// a session without any has its policy removed, which covers an edit that
// emptied the list while the session was stopped.
func (r *Reconciler) ensureEgress(ctx context.Context, sess *store.Session) error {
	if !r.cfg.NetworkPolicy {
		return nil
	}
	want, err := BuildEgressPolicy(sess, r.cfg)
	if err != nil {
		return &egressError{err.Error()}
	}
	if want == nil {
		return r.removeEgress(ctx, sess.ID)
	}
	api := r.cs.NetworkingV1().NetworkPolicies(r.cfg.Namespace)
	have, err := api.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := api.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return wrapEgress("create network policy", err)
		}
		r.event(ctx, sess, "network", egressSummary(sess))
	case err != nil:
		return wrapEgress("read network policy", err)
	case !reflect.DeepEqual(have.Spec, want.Spec) || !reflect.DeepEqual(have.Labels, want.Labels):
		have.Spec, have.Labels = want.Spec, want.Labels
		if _, err := api.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
			return wrapEgress("update network policy", err)
		}
		r.event(ctx, sess, "network", egressSummary(sess))
	}
	return nil
}

func egressSummary(sess *store.Session) string {
	msg := fmt.Sprintf("local network access: %d destination(s)", len(sess.Egress))
	if sess.K8sAccess == store.K8sAccessSandbox {
		msg += " plus the sandbox cluster's API"
	}
	return msg
}

// removeEgress deletes the session's NetworkPolicy, if any.
func (r *Reconciler) removeEgress(ctx context.Context, id string) error {
	if !r.cfg.NetworkPolicy {
		return nil
	}
	err := r.cs.NetworkingV1().NetworkPolicies(r.cfg.Namespace).Delete(ctx, ObjectName(id), metav1.DeleteOptions{})
	if err := ignoreNotFound(err); err != nil {
		return wrapEgress("delete network policy", err)
	}
	return nil
}

// egressSessionIDs lists the sessions that own a NetworkPolicy, so orphans
// get visited by ReconcileAll.
func (r *Reconciler) egressSessionIDs(ctx context.Context) []string {
	if !r.cfg.NetworkPolicy {
		return nil
	}
	list, err := r.cs.NetworkingV1().NetworkPolicies(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: k8s.LabelSession})
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		ids = append(ids, p.Labels[k8s.LabelSession])
	}
	return ids
}

// orphanEgress removes the NetworkPolicy of a session that no longer
// exists, once it is old enough not to belong to one being created.
func (r *Reconciler) orphanEgress(ctx context.Context, id string, old func(metav1.Time) bool) error {
	if !r.cfg.NetworkPolicy {
		return nil
	}
	p, err := r.cs.NetworkingV1().NetworkPolicies(r.cfg.Namespace).Get(ctx, ObjectName(id), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return wrapEgress("read network policy", err)
	case !old(p.CreationTimestamp) || p.DeletionTimestamp != nil:
		return nil
	}
	r.log.Warn("deleting orphaned network policy", "networkpolicy", p.Name)
	return r.removeEgress(ctx, id)
}

func wrapEgress(what string, err error) error {
	if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
		return &egressError{what + ": " + err.Error()}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// isEgressError reports whether err is one the owner has to fix.
func isEgressError(err error) (string, bool) {
	var ee *egressError
	if errors.As(err, &ee) {
		return ee.msg, true
	}
	return "", false
}

package reconcile

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Kubernetes access in "namespace" mode. A session in that mode gets an
// account of its own (the shared read-only one would hand every session the
// same rights): a ServiceAccount in the hub namespace, bound to the read
// ClusterRole like the shared account (in the hub namespace or cluster-wide,
// whichever the chart chose) and to the write ClusterRole in each namespace
// the session listed. The bindings live in other namespaces, where owner
// references cannot reach, so the reconciler removes them itself: when the
// pod is gone (stop, delete) and for orphans. Everything carries the session
// label, which is how they are found again.

// SessionAccountName is the ServiceAccount of a session in namespace mode.
func SessionAccountName(sessionID string) string { return ObjectName(sessionID) }

// BuildServiceAccount returns the per-session ServiceAccount.
func BuildServiceAccount(s *store.Session, cfg Config) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: SessionAccountName(s.ID), Namespace: cfg.Namespace, Labels: Labels(s)},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

func accessSubject(s *store.Session, cfg Config) []rbacv1.Subject {
	return []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: SessionAccountName(s.ID), Namespace: cfg.Namespace}}
}

func clusterRoleRef(name string) rbacv1.RoleRef {
	return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}
}

// BuildRoleBinding binds the session's account to a ClusterRole in one
// namespace: the read role in the hub namespace, the write role elsewhere.
func BuildRoleBinding(s *store.Session, cfg Config, namespace, clusterRole string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName(s.ID), Namespace: namespace, Labels: Labels(s)},
		RoleRef:    clusterRoleRef(clusterRole),
		Subjects:   accessSubject(s, cfg),
	}
}

// BuildClusterRoleBinding binds the session's account to the read role
// across the cluster, for charts with clusterWide read access.
func BuildClusterRoleBinding(s *store.Session, cfg Config) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName(s.ID), Labels: Labels(s)},
		RoleRef:    clusterRoleRef(cfg.ReadClusterRole),
		Subjects:   accessSubject(s, cfg),
	}
}

// accessError marks a failure the session owner has to fix (a namespace
// that does not exist, rights the hub lacks); anything else is retried.
type accessError struct{ msg string }

func (e *accessError) Error() string { return e.msg }

// ensureAccess converges the RBAC objects of a session to its access mode.
// Sessions not in namespace mode get theirs removed, which covers an edit
// that switched the mode while the session was stopped.
func (r *Reconciler) ensureAccess(ctx context.Context, sess *store.Session) error {
	if sess.K8sAccess != store.K8sAccessNamespace {
		return r.removeAccess(ctx, sess.ID)
	}
	if !r.cfg.NamespaceWrite {
		return &accessError{"namespace write access is not enabled on this hub (chart value runner.serviceAccount.namespaceWrite)"}
	}
	sa := BuildServiceAccount(sess, r.cfg)
	if _, err := r.cs.CoreV1().ServiceAccounts(r.cfg.Namespace).Get(ctx, sa.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := r.cs.CoreV1().ServiceAccounts(r.cfg.Namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return wrapAccess("create service account", err)
		}
		r.event(ctx, sess, "rbac", "created service account "+sa.Name)
	} else if err != nil {
		return wrapAccess("read service account", err)
	}

	// Read access, in the hub namespace or cluster-wide.
	keep := map[string]bool{}
	if r.cfg.ClusterWideRead {
		if err := r.ensureClusterRoleBinding(ctx, BuildClusterRoleBinding(sess, r.cfg)); err != nil {
			return err
		}
	} else {
		if err := r.ensureRoleBinding(ctx, BuildRoleBinding(sess, r.cfg, r.cfg.Namespace, r.cfg.ReadClusterRole)); err != nil {
			return err
		}
		keep[r.cfg.Namespace] = true
		if err := r.deleteClusterRoleBinding(ctx, ObjectName(sess.ID)); err != nil {
			return err
		}
	}
	// Write access in each listed namespace.
	for _, ns := range sess.K8sNamespaces {
		if err := r.ensureRoleBinding(ctx, BuildRoleBinding(sess, r.cfg, ns, r.cfg.WriteClusterRole)); err != nil {
			return err
		}
		keep[ns] = true
	}
	// Bindings in namespaces the session no longer lists.
	bindings, err := r.sessionRoleBindings(ctx, sess.ID)
	if err != nil {
		return err
	}
	for _, rb := range bindings {
		if keep[rb.Namespace] {
			continue
		}
		if err := ignoreNotFound(r.cs.RbacV1().RoleBindings(rb.Namespace).Delete(ctx, rb.Name, metav1.DeleteOptions{})); err != nil {
			return wrapAccess("delete role binding in "+rb.Namespace, err)
		}
	}
	r.event(ctx, sess, "rbac", "kubernetes access: read-only plus write in "+strings.Join(sess.K8sNamespaces, ", "))
	return nil
}

// ensureRoleBinding creates the binding, or replaces one that differs
// (roleRef is immutable, so a change means delete and create).
func (r *Reconciler) ensureRoleBinding(ctx context.Context, want *rbacv1.RoleBinding) error {
	c := r.cs.RbacV1().RoleBindings(want.Namespace)
	have, err := c.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return wrapAccess("read role binding in "+want.Namespace, err)
	case have.RoleRef == want.RoleRef && reflect.DeepEqual(have.Subjects, want.Subjects):
		return nil
	default:
		if err := ignoreNotFound(c.Delete(ctx, want.Name, metav1.DeleteOptions{})); err != nil {
			return wrapAccess("replace role binding in "+want.Namespace, err)
		}
	}
	if _, err := c.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return wrapAccess("create role binding in "+want.Namespace, err)
	}
	return nil
}

func (r *Reconciler) ensureClusterRoleBinding(ctx context.Context, want *rbacv1.ClusterRoleBinding) error {
	c := r.cs.RbacV1().ClusterRoleBindings()
	have, err := c.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return wrapAccess("read cluster role binding", err)
	case have.RoleRef == want.RoleRef && reflect.DeepEqual(have.Subjects, want.Subjects):
		return nil
	default:
		if err := ignoreNotFound(c.Delete(ctx, want.Name, metav1.DeleteOptions{})); err != nil {
			return wrapAccess("replace cluster role binding", err)
		}
	}
	if _, err := c.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return wrapAccess("create cluster role binding", err)
	}
	return nil
}

// deleteClusterRoleBinding removes a session's cluster-wide read binding.
// Without clusterWide read the hub has no rights on ClusterRoleBindings,
// and none should exist; a Forbidden answer is therefore not an error.
func (r *Reconciler) deleteClusterRoleBinding(ctx context.Context, name string) error {
	err := r.cs.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	return wrapAccess("delete cluster role binding", err)
}

// sessionRoleBindings lists a session's RoleBindings in every namespace.
func (r *Reconciler) sessionRoleBindings(ctx context.Context, id string) ([]rbacv1.RoleBinding, error) {
	list, err := r.cs.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: k8s.LabelSession + "=" + id})
	if err != nil {
		return nil, wrapAccess("list role bindings", err)
	}
	return list.Items, nil
}

// removeAccess deletes a session's account and every binding. It is a
// no-op on hubs without namespace write access, which cannot have created
// any and lack the rights to look.
func (r *Reconciler) removeAccess(ctx context.Context, id string) error {
	if !r.cfg.NamespaceWrite {
		return nil
	}
	bindings, err := r.sessionRoleBindings(ctx, id)
	if err != nil {
		return err
	}
	for _, rb := range bindings {
		if err := ignoreNotFound(r.cs.RbacV1().RoleBindings(rb.Namespace).Delete(ctx, rb.Name, metav1.DeleteOptions{})); err != nil {
			return wrapAccess("delete role binding in "+rb.Namespace, err)
		}
	}
	if err := r.deleteClusterRoleBinding(ctx, ObjectName(id)); err != nil {
		return err
	}
	if err := ignoreNotFound(r.cs.CoreV1().ServiceAccounts(r.cfg.Namespace).Delete(ctx, SessionAccountName(id), metav1.DeleteOptions{})); err != nil {
		return wrapAccess("delete service account", err)
	}
	return nil
}

// accessSessionIDs lists the sessions that own RBAC objects, so orphans
// get visited by ReconcileAll.
func (r *Reconciler) accessSessionIDs(ctx context.Context) []string {
	if !r.cfg.NamespaceWrite {
		return nil
	}
	var ids []string
	if sas, err := r.cs.CoreV1().ServiceAccounts(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: k8s.LabelSession}); err == nil {
		for _, sa := range sas.Items {
			ids = append(ids, sa.Labels[k8s.LabelSession])
		}
	}
	if rbs, err := r.cs.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: k8s.LabelSession}); err == nil {
		for _, rb := range rbs.Items {
			ids = append(ids, rb.Labels[k8s.LabelSession])
		}
	}
	return ids
}

// wrapAccess turns API answers the owner must act on (a namespace that does
// not exist, rights the hub lacks, a rejected object) into an accessError;
// the rest stays retryable.
func wrapAccess(what string, err error) error {
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
		return &accessError{what + ": " + err.Error()}
	}
	return fmt.Errorf("%s: %w", what, err)
}

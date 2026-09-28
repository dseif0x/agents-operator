package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/agents-operator/internal/auth"
	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Orchestrator is the seam between the session service and whatever creates
// the pods. Reconciler is the v1 implementation (Pods and PVCs directly); a
// later one could target the agent-sandbox Sandbox CRD.
type Orchestrator interface {
	// Notify asks for the session to be converged soon.
	Notify(sessionID string)
	// PodLogs returns the tail of the runner container's logs.
	PodLogs(ctx context.Context, sessionID string, tailLines int64) ([]byte, error)
}

// Notifier is told about every state change so the API can fan it out.
type Notifier interface {
	SessionChanged(ctx context.Context, s *store.Session)
	SessionDeleted(ctx context.Context, sessionID, ownerID string)
}

// NopNotifier ignores notifications.
type NopNotifier struct{}

func (NopNotifier) SessionChanged(context.Context, *store.Session) {}
func (NopNotifier) SessionDeleted(context.Context, string, string) {}

// Reconciler is a level-triggered controller for session objects.
type Reconciler struct {
	cfg      Config
	store    store.Store
	cs       kubernetes.Interface
	inf      *k8s.Informers
	notifier Notifier
	log      *slog.Logger
	queue    workqueue.TypedRateLimitingInterface[string]
	interval time.Duration
	now      func() time.Time

	startOnce sync.Once
}

// New wires a reconciler. Run must be called to process work.
func New(cfg Config, st store.Store, cs kubernetes.Interface, inf *k8s.Informers, notifier Notifier, interval time.Duration, log *slog.Logger) *Reconciler {
	if notifier == nil {
		notifier = NopNotifier{}
	}
	r := &Reconciler{
		cfg: cfg.Defaults(), store: st, cs: cs, inf: inf, notifier: notifier, log: log,
		queue:    workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		interval: interval, now: time.Now,
	}
	if r.interval == 0 {
		r.interval = 30 * time.Second
	}
	inf.OnPodChange(r.Notify)
	return r
}

// Notify implements Orchestrator.
func (r *Reconciler) Notify(sessionID string) { r.queue.Add(sessionID) }

// Run processes the queue until ctx is done. The informers must be synced
// before this is called: the reconciler rebuilds its view from Kubernetes
// first and only then touches rows.
func (r *Reconciler) Run(ctx context.Context) {
	r.startOnce.Do(func() {
		go func() {
			<-ctx.Done()
			r.queue.ShutDown()
		}()
		go r.tick(ctx)
		for i := 0; i < 2; i++ {
			go r.worker(ctx)
		}
	})
	<-ctx.Done()
}

func (r *Reconciler) tick(ctx context.Context) {
	r.ReconcileAll(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ReconcileAll(ctx)
		}
	}
}

func (r *Reconciler) worker(ctx context.Context) {
	for {
		id, shutdown := r.queue.Get()
		if shutdown {
			return
		}
		err := r.ReconcileOne(ctx, id)
		if err != nil {
			r.log.Warn("reconcile failed", "session", id, "err", err)
			r.queue.AddRateLimited(id)
		} else {
			r.queue.Forget(id)
		}
		r.queue.Done(id)
	}
}

// ReconcileAll enqueues every row and every labelled object, so orphans
// (objects without a row) get visited too.
func (r *Reconciler) ReconcileAll(ctx context.Context) {
	ids := map[string]struct{}{}
	rows, err := r.store.Sessions().ListAll(ctx)
	if err != nil {
		r.log.Warn("list sessions failed", "err", err)
	}
	for _, s := range rows {
		ids[s.ID] = struct{}{}
	}
	sel := labels.Everything()
	if pods, err := r.inf.Pods.Pods(r.cfg.Namespace).List(sel); err == nil {
		for _, p := range pods {
			ids[p.Labels[k8s.LabelSession]] = struct{}{}
		}
	}
	if pvcs, err := r.inf.PVCs.PersistentVolumeClaims(r.cfg.Namespace).List(sel); err == nil {
		for _, p := range pvcs {
			ids[p.Labels[k8s.LabelSession]] = struct{}{}
		}
	}
	if secrets, err := r.inf.Secrets.Secrets(r.cfg.Namespace).List(sel); err == nil {
		for _, s := range secrets {
			ids[s.Labels[k8s.LabelSession]] = struct{}{}
		}
	}
	delete(ids, "")
	for id := range ids {
		r.queue.Add(id)
	}
}

// observed is what exists in the cluster for one session.
type observed struct {
	pod    *corev1.Pod
	pvc    *corev1.PersistentVolumeClaim
	secret *corev1.Secret
}

func (r *Reconciler) observe(id string) observed {
	name := ObjectName(id)
	var o observed
	if p, err := r.inf.Pods.Pods(r.cfg.Namespace).Get(name); err == nil {
		o.pod = p
	}
	if p, err := r.inf.PVCs.PersistentVolumeClaims(r.cfg.Namespace).Get(name); err == nil {
		o.pvc = p
	}
	if s, err := r.inf.Secrets.Secrets(r.cfg.Namespace).Get(name); err == nil {
		o.secret = s
	}
	return o
}

// requeueSoon asks for another pass once the informer cache catches up.
func (r *Reconciler) requeueSoon(id string) { r.queue.AddAfter(id, 2*time.Second) }

// ReconcileOne converges a single session. It is exported for tests.
func (r *Reconciler) ReconcileOne(ctx context.Context, id string) error {
	sess, err := r.store.Sessions().Get(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	o := r.observe(id)
	if sess == nil {
		return r.reconcileOrphan(ctx, id, o)
	}
	switch sess.State {
	case store.StateCreating:
		return r.reconcileCreating(ctx, sess, o)
	case store.StateRunning:
		return r.reconcileRunning(ctx, sess, o)
	case store.StateStopping:
		return r.reconcileStopping(ctx, sess, o)
	case store.StateStopped:
		if o.pod != nil && o.pod.DeletionTimestamp == nil {
			return r.deletePod(ctx, sess.ID)
		}
		return nil
	case store.StateFailed:
		// Keep the pod around for logs; nothing to converge.
		return nil
	case store.StateDeleting:
		return r.reconcileDeleting(ctx, sess, o)
	default:
		return fmt.Errorf("unknown state %q", sess.State)
	}
}

func (r *Reconciler) reconcileOrphan(ctx context.Context, id string, o observed) error {
	cutoff := r.now().Add(-r.cfg.OrphanGrace)
	old := func(t metav1.Time) bool { return t.Time.Before(cutoff) }
	name := ObjectName(id)
	if o.pod != nil && old(o.pod.CreationTimestamp) && o.pod.DeletionTimestamp == nil {
		r.log.Warn("deleting orphaned pod", "pod", name)
		if err := r.deletePod(ctx, id); err != nil {
			return err
		}
	}
	if o.pvc != nil && old(o.pvc.CreationTimestamp) && o.pvc.DeletionTimestamp == nil {
		r.log.Warn("deleting orphaned pvc", "pvc", name)
		if err := ignoreNotFound(r.cs.CoreV1().PersistentVolumeClaims(r.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
			return err
		}
	}
	if o.secret != nil && old(o.secret.CreationTimestamp) && o.secret.DeletionTimestamp == nil {
		r.log.Warn("deleting orphaned secret", "secret", name)
		if err := ignoreNotFound(r.cs.CoreV1().Secrets(r.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileCreating(ctx context.Context, sess *store.Session, o observed) error {
	created := false
	if o.pvc == nil {
		pvc := BuildPVC(sess, r.cfg)
		if _, err := r.cs.CoreV1().PersistentVolumeClaims(r.cfg.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return r.fail(ctx, sess, "create pvc: "+err.Error())
		}
		r.event(ctx, sess, "pvc", "created "+pvc.Name)
		created = true
	}
	gen := strconv.Itoa(sess.Generation)
	if o.secret == nil || o.secret.Annotations[k8s.AnnotationGeneration] != gen {
		token, err := auth.NewRunnerToken()
		if err != nil {
			return err
		}
		userSecret, err := r.userSecret(ctx, sess.OwnerID)
		if err != nil {
			return err
		}
		secret := BuildSecret(sess, r.cfg, token, userSecret)
		if o.secret == nil {
			if _, err := r.cs.CoreV1().Secrets(r.cfg.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
				return r.fail(ctx, sess, "create secret: "+err.Error())
			}
		} else {
			secret.ResourceVersion = o.secret.ResourceVersion
			if _, err := r.cs.CoreV1().Secrets(r.cfg.Namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("rotate secret: %w", err)
			}
		}
		r.event(ctx, sess, "secret", "runner token issued (generation "+gen+")")
		created = true
	}
	if o.pod != nil {
		switch {
		case o.pod.DeletionTimestamp != nil:
			r.requeueSoon(sess.ID)
			return nil
		case o.pod.Annotations[k8s.AnnotationGeneration] != gen || PodTerminal(o.pod):
			// Stale pod from a previous generation, or a crashed one from a
			// failed session being restarted: remove it and come back.
			if err := r.deletePod(ctx, sess.ID); err != nil {
				return err
			}
			r.requeueSoon(sess.ID)
			return nil
		case PodReady(o.pod):
			return r.setState(ctx, sess, store.StateRunning, "")
		default:
			reason := PodReason(o.pod)
			if !o.pod.CreationTimestamp.IsZero() && r.now().Sub(o.pod.CreationTimestamp.Time) > r.cfg.CreatingTimeout {
				return r.fail(ctx, sess, "pod not ready after "+r.cfg.CreatingTimeout.String()+": "+reason)
			}
			if reason != sess.StateReason {
				return r.setState(ctx, sess, store.StateCreating, reason)
			}
			return nil
		}
	}
	if created {
		// Let the informer cache see the PVC and Secret before the pod
		// references them; avoids a spurious "not found" on slow caches.
		r.requeueSoon(sess.ID)
		return nil
	}
	pod := BuildPod(sess, r.cfg)
	if _, err := r.cs.CoreV1().Pods(r.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			r.requeueSoon(sess.ID)
			return nil
		}
		return r.fail(ctx, sess, "create pod: "+err.Error())
	}
	r.event(ctx, sess, "pod", "created "+pod.Name)
	r.k8sEvent(ctx, pod.Name, "Created", "agents-operator created session pod")
	return nil
}

func (r *Reconciler) reconcileRunning(ctx context.Context, sess *store.Session, o observed) error {
	switch {
	case o.pod == nil:
		return r.fail(ctx, sess, "pod disappeared")
	case o.pod.DeletionTimestamp != nil:
		return r.fail(ctx, sess, "pod was deleted outside agents-operator")
	case PodTerminal(o.pod):
		return r.fail(ctx, sess, PodReason(o.pod))
	}
	return nil
}

func (r *Reconciler) reconcileStopping(ctx context.Context, sess *store.Session, o observed) error {
	if o.pod == nil {
		return r.setState(ctx, sess, store.StateStopped, "")
	}
	if o.pod.DeletionTimestamp == nil {
		if err := r.deletePod(ctx, sess.ID); err != nil {
			return err
		}
	}
	r.requeueSoon(sess.ID)
	return nil
}

func (r *Reconciler) reconcileDeleting(ctx context.Context, sess *store.Session, o observed) error {
	name := ObjectName(sess.ID)
	ns := r.cfg.Namespace
	pending := false
	if o.pod != nil {
		pending = true
		if o.pod.DeletionTimestamp == nil {
			if err := r.deletePod(ctx, sess.ID); err != nil {
				return err
			}
		}
	}
	if o.pvc != nil {
		pending = true
		if o.pvc.DeletionTimestamp == nil {
			if err := ignoreNotFound(r.cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
				return err
			}
		}
	}
	if o.secret != nil {
		pending = true
		if o.secret.DeletionTimestamp == nil {
			if err := ignoreNotFound(r.cs.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
				return err
			}
		}
	}
	if pending {
		r.requeueSoon(sess.ID)
		return nil
	}
	if err := r.store.Sessions().Delete(ctx, sess.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	r.log.Info("session deleted", "session", sess.ID)
	r.notifier.SessionDeleted(ctx, sess.ID, sess.OwnerID)
	return nil
}

func (r *Reconciler) deletePod(ctx context.Context, id string) error {
	err := r.cs.CoreV1().Pods(r.cfg.Namespace).Delete(ctx, ObjectName(id), metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To[int64](15),
	})
	return ignoreNotFound(err)
}

// userSecret loads the per-user credential Secret's data, or nil.
func (r *Reconciler) userSecret(ctx context.Context, userID string) (map[string][]byte, error) {
	s, err := r.cs.CoreV1().Secrets(r.cfg.Namespace).Get(ctx, UserSecretName(userID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read user secret: %w", err)
	}
	return s.Data, nil
}

func (r *Reconciler) fail(ctx context.Context, sess *store.Session, reason string) error {
	r.log.Warn("session failed", "session", sess.ID, "reason", reason)
	return r.setState(ctx, sess, store.StateFailed, reason)
}

// setState writes the transition, records an event and notifies listeners.
func (r *Reconciler) setState(ctx context.Context, sess *store.Session, state, reason string) error {
	if sess.State == state && sess.StateReason == reason {
		return nil
	}
	updated, err := r.store.Sessions().SetState(ctx, sess.ID, state, reason)
	if err != nil {
		return err
	}
	msg := sess.State + " → " + state
	if reason != "" {
		msg += ": " + reason
	}
	r.log.Info("session state", "session", sess.ID, "from", sess.State, "to", state, "reason", reason)
	r.event(ctx, sess, "state", msg)
	r.notifier.SessionChanged(ctx, updated)
	return nil
}

func (r *Reconciler) event(ctx context.Context, sess *store.Session, kind, msg string) {
	if err := r.store.Events().Add(ctx, sess.ID, kind, msg); err != nil {
		r.log.Debug("record event failed", "err", err)
		return
	}
	_ = r.store.Events().Prune(ctx, sess.ID, store.EventsKeep)
}

// k8sEvent posts a Kubernetes Event on a pod, best effort.
func (r *Reconciler) k8sEvent(ctx context.Context, podName, reason, msg string) {
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: podName + ".", Namespace: r.cfg.Namespace},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: r.cfg.Namespace, Name: podName, APIVersion: "v1",
		},
		Reason: reason, Message: msg, Type: corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: "agents-operator"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if _, err := r.cs.CoreV1().Events(r.cfg.Namespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		r.log.Debug("post kubernetes event failed", "err", err)
	}
}

// PodLogs implements Orchestrator.
func (r *Reconciler) PodLogs(ctx context.Context, sessionID string, tailLines int64) ([]byte, error) {
	if tailLines <= 0 {
		tailLines = 500
	}
	req := r.cs.CoreV1().Pods(r.cfg.Namespace).GetLogs(ObjectName(sessionID), &corev1.PodLogOptions{
		Container: ContainerName, TailLines: ptr.To(tailLines),
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 4<<20))
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

var _ Orchestrator = (*Reconciler)(nil)

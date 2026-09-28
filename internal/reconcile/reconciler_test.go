package reconcile

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/store"
)

type harness struct {
	t   *testing.T
	ctx context.Context
	cs  kubernetes.Interface
	st  *store.Memory
	r   *Reconciler
	ns  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := fake.NewClientset()
	inf := k8s.NewInformers(cs, "agents-operator", 0)
	if err := inf.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(testCfg(), st, cs, inf, nil, time.Hour, log)
	return &harness{t: t, ctx: ctx, cs: cs, st: st, r: r, ns: "agents-operator"}
}

// eventually polls until fn returns true; informer caches are async.
func (h *harness) eventually(msg string, fn func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("timeout waiting for %s", msg)
}

func (h *harness) reconcile(id string) {
	h.t.Helper()
	if err := h.r.ReconcileOne(h.ctx, id); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

func (h *harness) pod(id string) *corev1.Pod {
	p, err := h.cs.CoreV1().Pods(h.ns).Get(h.ctx, ObjectName(id), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return p
}

func (h *harness) pvc(id string) *corev1.PersistentVolumeClaim {
	p, err := h.cs.CoreV1().PersistentVolumeClaims(h.ns).Get(h.ctx, ObjectName(id), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return p
}

func (h *harness) secret(id string) *corev1.Secret {
	s, err := h.cs.CoreV1().Secrets(h.ns).Get(h.ctx, ObjectName(id), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return s
}

func (h *harness) session(id string) *store.Session {
	s, err := h.st.Sessions().Get(h.ctx, id)
	if err != nil {
		return nil
	}
	return s
}

func (h *harness) markReady(id string) {
	h.t.Helper()
	p := h.pod(id)
	p.Status.Phase = corev1.PodRunning
	p.Status.PodIP = "10.0.0.5"
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := h.cs.CoreV1().Pods(h.ns).UpdateStatus(h.ctx, p, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	h.eventually("pod ready in cache", func() bool { return PodReady(h.r.observe(id).pod) })
}

// converge runs the creating flow through to running.
func (h *harness) converge(id string) {
	h.t.Helper()
	h.reconcile(id) // pvc + secret
	h.eventually("pvc and secret in cache", func() bool {
		o := h.r.observe(id)
		return o.pvc != nil && o.secret != nil
	})
	h.reconcile(id) // pod
	h.eventually("pod in cache", func() bool { return h.r.observe(id).pod != nil })
	h.markReady(id)
	h.reconcile(id) // running
}

func newSession(h *harness) *store.Session {
	s := &store.Session{OwnerID: "u1", Name: "one", Agent: "claude", RepoURL: "https://x/y.git", State: store.StateCreating, PVCSize: "5Gi"}
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

func TestCreateStartsPod(t *testing.T) {
	h := newHarness(t)
	// A per-user secret with an API key gets projected.
	_, _ = h.cs.CoreV1().Secrets(h.ns).Create(h.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: UserSecretName("u1"), Namespace: h.ns},
		Data:       map[string][]byte{store.CredAnthropicAPIKey: []byte("sk-ant")},
	}, metav1.CreateOptions{})
	s := newSession(h)
	h.converge(s.ID)

	if h.pvc(s.ID) == nil || h.secret(s.ID) == nil || h.pod(s.ID) == nil {
		t.Fatal("objects missing")
	}
	sec := h.secret(s.ID)
	if len(sec.Data["RUNNER_TOKEN"]) != 64 || string(sec.Data["ANTHROPIC_API_KEY"]) != "sk-ant" {
		t.Fatalf("secret data = %v", sec.Data)
	}
	if got := h.session(s.ID); got.State != store.StateRunning {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}
	evs, _ := h.st.Events().List(h.ctx, s.ID, 0)
	if len(evs) < 4 {
		t.Fatalf("expected events, got %d", len(evs))
	}
	// Idempotent: another pass changes nothing.
	h.reconcile(s.ID)
	if got := h.session(s.ID); got.State != store.StateRunning {
		t.Fatalf("state after idempotent pass = %s", got.State)
	}
}

func TestStopKeepsPVCAndStartRotatesToken(t *testing.T) {
	h := newHarness(t)
	s := newSession(h)
	h.converge(s.ID)
	token1 := string(h.secret(s.ID).Data["RUNNER_TOKEN"])

	// Stop: pod goes, PVC and Secret stay.
	if _, err := h.st.Sessions().SetState(h.ctx, s.ID, store.StateStopping, ""); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	h.eventually("pod deleted", func() bool { return h.r.observe(s.ID).pod == nil })
	h.reconcile(s.ID)
	if got := h.session(s.ID); got.State != store.StateStopped {
		t.Fatalf("state = %s", got.State)
	}
	if h.pvc(s.ID) == nil || h.secret(s.ID) == nil || h.pod(s.ID) != nil {
		t.Fatal("stop must keep pvc and secret and remove the pod")
	}

	// Start: generation bumps, token rotates, pod comes back on the same PVC.
	// The fake clientset assigns no UIDs, so mark the PVC to prove it survives.
	marked := h.pvc(s.ID)
	marked.Annotations = map[string]string{"test/marker": "kept"}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(h.ns).Update(h.ctx, marked, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.Sessions().Bump(h.ctx, s.ID, store.StateCreating); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID) // rotates secret
	h.eventually("secret rotated in cache", func() bool {
		sec := h.r.observe(s.ID).secret
		return sec != nil && sec.Annotations[k8s.AnnotationGeneration] == "2"
	})
	h.reconcile(s.ID) // creates pod
	h.eventually("pod in cache", func() bool { return h.r.observe(s.ID).pod != nil })
	token2 := string(h.secret(s.ID).Data["RUNNER_TOKEN"])
	if token1 == token2 || len(token2) != 64 {
		t.Fatal("runner token must rotate on start")
	}
	if h.pvc(s.ID).Annotations["test/marker"] != "kept" {
		t.Fatal("pvc was recreated")
	}
	if h.pod(s.ID).Annotations[k8s.AnnotationGeneration] != "2" {
		t.Fatal("pod generation annotation wrong")
	}
	h.markReady(s.ID)
	h.reconcile(s.ID)
	if got := h.session(s.ID); got.State != store.StateRunning {
		t.Fatalf("state = %s", got.State)
	}
}

func TestPodFailureMarksFailedAndRestartReplacesPod(t *testing.T) {
	h := newHarness(t)
	s := newSession(h)
	h.converge(s.ID)

	p := h.pod(s.ID)
	p.Status.Phase = corev1.PodFailed
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: ContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}
	if _, err := h.cs.CoreV1().Pods(h.ns).UpdateStatus(h.ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.eventually("failed pod in cache", func() bool { return PodTerminal(h.r.observe(s.ID).pod) })
	h.reconcile(s.ID)
	got := h.session(s.ID)
	if got.State != store.StateFailed || got.StateReason != "container exited with code 137 (OOMKilled)" {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}
	if h.pod(s.ID) == nil {
		t.Fatal("failed pod must be kept for logs")
	}

	// Start from failed: the dead pod is replaced.
	if _, err := h.st.Sessions().Bump(h.ctx, s.ID, store.StateCreating); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID) // rotates the secret and deletes the stale pod in one pass
	h.eventually("secret rotated and old pod gone", func() bool {
		o := h.r.observe(s.ID)
		return o.secret != nil && o.secret.Annotations[k8s.AnnotationGeneration] == "2" && o.pod == nil
	})
	h.reconcile(s.ID) // creates new pod
	h.eventually("new pod", func() bool { return h.r.observe(s.ID).pod != nil })
	if np := h.pod(s.ID); np.Annotations[k8s.AnnotationGeneration] != "2" || np.Status.Phase == corev1.PodFailed {
		t.Fatal("pod not replaced")
	}
}

func TestDeleteRemovesEverything(t *testing.T) {
	h := newHarness(t)
	s := newSession(h)
	h.converge(s.ID)
	if _, err := h.st.Sessions().SetState(h.ctx, s.ID, store.StateDeleting, ""); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	h.eventually("objects gone", func() bool {
		o := h.r.observe(s.ID)
		return o.pod == nil && o.pvc == nil && o.secret == nil
	})
	h.reconcile(s.ID)
	if h.session(s.ID) != nil {
		t.Fatal("row should be deleted")
	}
	if h.pod(s.ID) != nil || h.pvc(s.ID) != nil || h.secret(s.ID) != nil {
		t.Fatal("objects remain")
	}
}

func TestOrphansAreRemovedAfterGrace(t *testing.T) {
	h := newHarness(t)
	orphan := &store.Session{ID: "99999999-9999-4999-8999-999999999999", OwnerID: "ghost", Name: "ghost", Agent: "shell"}
	pod := BuildPod(orphan, h.r.cfg)
	pvc := BuildPVC(orphan, h.r.cfg)
	// The fake API server does not stamp creation times; a real one does.
	pod.CreationTimestamp = metav1.Now()
	pvc.CreationTimestamp = metav1.Now()
	if _, err := h.cs.CoreV1().Pods(h.ns).Create(h.ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(h.ns).Create(h.ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.eventually("orphans in cache", func() bool {
		o := h.r.observe(orphan.ID)
		return o.pod != nil && o.pvc != nil
	})
	// Within the grace period nothing happens.
	h.r.ReconcileAll(h.ctx)
	h.reconcile(orphan.ID)
	if h.pod(orphan.ID) == nil {
		t.Fatal("orphan deleted too early")
	}
	// After the grace period both objects go.
	h.r.now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	h.reconcile(orphan.ID)
	if h.pod(orphan.ID) != nil || h.pvc(orphan.ID) != nil {
		t.Fatal("orphans not deleted")
	}
}

func TestRunningPodDisappearsIsFailed(t *testing.T) {
	h := newHarness(t)
	s := newSession(h)
	h.converge(s.ID)
	if err := h.cs.CoreV1().Pods(h.ns).Delete(h.ctx, ObjectName(s.ID), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.eventually("pod gone", func() bool { return h.r.observe(s.ID).pod == nil })
	h.reconcile(s.ID)
	if got := h.session(s.ID); got.State != store.StateFailed || got.StateReason != "pod disappeared" {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}
	_, err := h.cs.CoreV1().Pods(h.ns).Get(h.ctx, ObjectName(s.ID), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("pod should stay gone")
	}
}

func TestCreatingTimeout(t *testing.T) {
	h := newHarness(t)
	s := newSession(h)
	h.reconcile(s.ID)
	h.eventually("pvc and secret", func() bool {
		o := h.r.observe(s.ID)
		return o.pvc != nil && o.secret != nil
	})
	h.reconcile(s.ID)
	h.eventually("pod", func() bool { return h.r.observe(s.ID).pod != nil })
	// Pending forever: reason is surfaced, then timeout fails the session.
	p := h.pod(s.ID)
	p.CreationTimestamp = metav1.Now()
	if _, err := h.cs.CoreV1().Pods(h.ns).Update(h.ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	p = h.pod(s.ID)
	p.Status.Phase = corev1.PodPending
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: ContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "no such image"}}}}
	if _, err := h.cs.CoreV1().Pods(h.ns).UpdateStatus(h.ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.eventually("pending status", func() bool {
		o := h.r.observe(s.ID)
		return o.pod != nil && len(o.pod.Status.ContainerStatuses) == 1
	})
	h.reconcile(s.ID)
	if got := h.session(s.ID); got.State != store.StateCreating || got.StateReason != "ImagePullBackOff: no such image" {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}
	h.r.now = func() time.Time { return time.Now().Add(time.Hour) }
	h.reconcile(s.ID)
	if got := h.session(s.ID); got.State != store.StateFailed {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}
}

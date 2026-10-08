package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Leader elects one hub process to run the parts that must not run twice:
// the reconciler, and the poller's side effects. The election is a
// coordination.k8s.io Lease in the hub namespace. Every replica serves
// the API, the UI and terminals; only the leader converges sessions.
//
// It exists for rolling updates: with maxSurge 1 the new pod comes up and
// serves while the old one still leads, the old one releases the Lease on
// SIGTERM, and the new one takes over within RetryPeriod. Losing the lease
// any other way (the API server unreachable for LeaseDuration) is treated
// like a crash by the caller: the process stops and the kubelet restarts it.
type Leader struct {
	Clientset kubernetes.Interface
	Namespace string
	// Name is the Lease; Identity is this process (the pod name).
	Name, Identity string
	Log            *slog.Logger
	// Timing defaults to 15s lease, 10s renew deadline, 2s retry.
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
}

// Run blocks until ctx is done. onStarted runs in its own goroutine with a
// context that ends when leadership ends. onLost is called when leadership
// ends for any reason other than ctx, after onStarted's context is
// cancelled. Cancelling ctx releases the Lease before Run returns, so the
// next candidate need not wait for it to expire.
func (l *Leader) Run(ctx context.Context, onStarted func(ctx context.Context), onLost func()) error {
	if l.LeaseDuration == 0 {
		l.LeaseDuration = 15 * time.Second
	}
	if l.RenewDeadline == 0 {
		l.RenewDeadline = 10 * time.Second
	}
	if l.RetryPeriod == 0 {
		l.RetryPeriod = 2 * time.Second
	}
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: l.Name, Namespace: l.Namespace},
			Client:     l.Clientset.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: l.Identity},
		},
		LeaseDuration:   l.LeaseDuration,
		RenewDeadline:   l.RenewDeadline,
		RetryPeriod:     l.RetryPeriod,
		ReleaseOnCancel: true,
		Name:            l.Name,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(lctx context.Context) {
				log.Info("leading", "lease", l.Name, "identity", l.Identity)
				onStarted(lctx)
			},
			OnStoppedLeading: func() {
				if ctx.Err() != nil {
					log.Info("released lease", "lease", l.Name)
					return
				}
				log.Warn("lost lease", "lease", l.Name)
				if onLost != nil {
					onLost()
				}
			},
			OnNewLeader: func(id string) {
				if id != l.Identity {
					log.Info("another replica leads", "lease", l.Name, "leader", id)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("leader election: %w", err)
	}
	// elector.Run returns when leadership is lost or ctx is done. A process
	// that stays alive after a loss keeps campaigning.
	for {
		elector.Run(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(l.RetryPeriod):
		}
	}
}

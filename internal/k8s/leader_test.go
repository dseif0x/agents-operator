package k8s

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestLeaderHandsOverOnCancel(t *testing.T) {
	cs := fake.NewClientset()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	newLeader := func(id string) *Leader {
		return &Leader{Clientset: cs, Namespace: "agents", Name: "hub", Identity: id, Log: log,
			// The record stores the lease duration in whole seconds, so no shorter.
			LeaseDuration: 2 * time.Second, RenewDeadline: 1 * time.Second, RetryPeriod: 100 * time.Millisecond}
	}
	type state struct {
		leading atomic.Bool
		lost    atomic.Int32
	}
	start := func(ctx context.Context, id string) *state {
		st := &state{}
		go func() {
			_ = newLeader(id).Run(ctx, func(lctx context.Context) {
				st.leading.Store(true)
				<-lctx.Done()
				st.leading.Store(false)
			}, func() { st.lost.Add(1) })
		}()
		return st
	}
	wait := func(msg string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", msg)
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	a := start(ctxA, "pod-a")
	wait("a leading", func() bool { return a.leading.Load() })

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	b := start(ctxB, "pod-b")
	time.Sleep(500 * time.Millisecond)
	if b.leading.Load() {
		t.Fatal("b led while a held the lease")
	}

	// A stops (SIGTERM during a rolling update): it releases the lease and
	// B takes over well within the lease duration.
	cancelA()
	wait("a stopped leading", func() bool { return !a.leading.Load() })
	wait("b leading", func() bool { return b.leading.Load() })
	if a.lost.Load() != 0 || b.lost.Load() != 0 {
		t.Fatalf("a release is not a loss: a=%d b=%d", a.lost.Load(), b.lost.Load())
	}
}

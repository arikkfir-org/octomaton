// Package leader elects, through a Lease, the one replica that runs the jobs only one replica may
// run at a time.
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"octomaton.dev/internal/system/metrics"
)

const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second
)

// Elector keeps a replica in the election for a Lease and runs Jobs while the replica leads.
type Elector struct {
	Client kubernetes.Interface
	// Namespace and Name locate the Lease.
	Namespace string
	Name      string
	// Pod names this replica. Its identity in the election adds a random suffix, so that a
	// restarted pod does not take its predecessor's Lease for its own.
	Pod string
	// Jobs runs while this replica leads; its context is cancelled when leadership is lost.
	Jobs    func(ctx context.Context)
	Metrics *metrics.Metrics
	Logger  *slog.Logger

	mu      sync.Mutex
	leading bool
}

// Leading reports whether this replica leads.
func (e *Elector) Leading() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leading
}

// Run takes part in the election until ctx is cancelled, rejoining it whenever leadership is lost.
// Cancelling ctx releases the Lease.
func (e *Elector) Run(ctx context.Context) {
	id := identity(e.Pod)
	e.Metrics.SetLeader(ctx, false)
	for ctx.Err() == nil {
		elector, err := leaderelection.NewLeaderElector(e.config(id))
		if err != nil {
			e.Logger.ErrorContext(ctx, "Cannot configure leader election", "error", err)
			return
		}
		elector.Run(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(retryPeriod):
		}
	}
}

func (e *Elector) config(id string) leaderelection.LeaderElectionConfig {
	return leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: e.Name, Namespace: e.Namespace},
			Client:     e.Client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: id},
		},
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Name:            e.Name,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) { e.lead(ctx, id) },
			OnStoppedLeading: func() { e.stepDown(id) },
			OnNewLeader: func(current string) {
				if current != id {
					e.Logger.Info("Observed leader", "leader", current)
				}
			},
		},
	}
}

func (e *Elector) lead(ctx context.Context, id string) {
	if !e.setLeading(ctx, true) {
		return
	}
	e.Logger.InfoContext(ctx, "Became leader", "identity", id)
	e.Jobs(ctx)
}

func (e *Elector) stepDown(id string) {
	if e.setLeading(context.Background(), false) {
		e.Logger.Info("No longer leader", "identity", id)
	}
}

// setLeading records a change of leadership and reports whether there was one. client-go starts
// leading in a goroutine and cancels its context before stepping down, so a replica that lost the
// Lease before that goroutine ran never takes leadership back.
func (e *Elector) setLeading(ctx context.Context, leading bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.leading == leading || (leading && ctx.Err() != nil) {
		return false
	}
	e.leading = leading
	e.Metrics.SetLeader(ctx, leading)
	return true
}

func identity(pod string) string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return pod + "_" + hex.EncodeToString(suffix)
}

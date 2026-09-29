package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// pingTimeout bounds one check of the Kubernetes API server.
const pingTimeout = 3 * time.Second

// Readiness answers the readiness probe. A replica is ready while the Kubernetes API server answers
// and, on the leader, once the PipelineRun informer has synced; a replica shutting down never is.
type Readiness struct {
	// Ping checks that the Kubernetes API server answers.
	Ping func(ctx context.Context) error
	// Leading reports whether this replica is the elected leader.
	Leading func() bool
	// Synced reports whether the leader's PipelineRun informer has synced.
	Synced func() bool
	// TTL is how long a Ping result is reused, so that frequent probes do not load the API server.
	TTL time.Duration

	stopping atomic.Bool

	mu        sync.Mutex
	checkedAt time.Time
	lastErr   error
}

func (rd *Readiness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := rd.check(r.Context()); err != nil {
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
}

// stop makes the replica unready for good.
func (rd *Readiness) stop() {
	rd.stopping.Store(true)
}

func (rd *Readiness) check(ctx context.Context) error {
	if rd.stopping.Load() {
		return errors.New("shutting down")
	}
	if err := rd.apiReachable(ctx); err != nil {
		return fmt.Errorf("kubernetes API unreachable: %w", err)
	}
	if rd.Leading() && !rd.Synced() {
		return errors.New("PipelineRun informer not synced yet")
	}
	return nil
}

func (rd *Readiness) apiReachable(ctx context.Context) error {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if !rd.checkedAt.IsZero() && time.Since(rd.checkedAt) < rd.TTL {
		return rd.lastErr
	}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	rd.lastErr = rd.Ping(ctx)
	rd.checkedAt = time.Now()
	return rd.lastErr
}

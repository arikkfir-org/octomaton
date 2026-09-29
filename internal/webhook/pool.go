package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"octomaton.dev/internal/metrics"
)

// Job is a unit of asynchronous webhook processing.
type Job struct {
	// Name describes the job in logs.
	Name string
	Run  func(ctx context.Context)
}

// Pool runs jobs on a fixed number of workers fed by a bounded queue.
type Pool struct {
	jobs       chan Job
	jobTimeout time.Duration
	logger     *slog.Logger
	metrics    *metrics.Metrics

	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu     sync.RWMutex
	closed bool
}

// NewPool starts workers goroutines consuming a queue of queueSize jobs; each job
// runs with a timeout of jobTimeout.
func NewPool(workers, queueSize int, jobTimeout time.Duration, logger *slog.Logger, m *metrics.Metrics) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		jobs:       make(chan Job, queueSize),
		jobTimeout: jobTimeout,
		logger:     logger,
		metrics:    m,
		baseCtx:    ctx,
		cancel:     cancel,
	}
	for range workers {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

// Submit queues a job without blocking; it returns false when the queue is full
// or the pool is shutting down.
func (p *Pool) Submit(job Job) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return false
	}
	select {
	case p.jobs <- job:
		p.metrics.SetQueueDepth(p.baseCtx, len(p.jobs))
		return true
	default:
		return false
	}
}

// Shutdown stops accepting jobs and waits for queued and running jobs to finish;
// when ctx expires first, running jobs are cancelled.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.jobs)
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		p.cancel()
		return nil
	case <-ctx.Done():
		p.cancel()
		<-done
		return fmt.Errorf("webhook jobs were cancelled: %w", ctx.Err())
	}
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		p.metrics.SetQueueDepth(p.baseCtx, len(p.jobs))
		p.run(job)
	}
}

func (p *Pool) run(job Job) {
	ctx, cancel := context.WithTimeout(p.baseCtx, p.jobTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("Webhook job panicked", "job", job.Name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	start := time.Now()
	job.Run(ctx)
	p.logger.Debug("Webhook job finished", "job", job.Name, "duration", time.Since(start).String())
}

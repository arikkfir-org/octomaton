// Package relay forwards verified webhook deliveries, unchanged, to other
// receivers (for example Argo CD's webhook endpoint), asynchronously.
package relay

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Timeout bounds each forwarded request.
const Timeout = 10 * time.Second

// forwardedHeaders are copied from the original delivery, signatures included,
// so receivers can verify it themselves.
var forwardedHeaders = []string{
	"Content-Type",
	"User-Agent",
	"X-GitHub-Delivery",
	"X-GitHub-Event",
	"X-GitHub-Hook-ID",
	"X-GitHub-Hook-Installation-Target-ID",
	"X-GitHub-Hook-Installation-Target-Type",
	"X-Hub-Signature",
	"X-Hub-Signature-256",
}

type delivery struct {
	header http.Header
	body   []byte
}

// Relay forwards deliveries to a fixed set of URLs on a bounded worker pool.
// Failures are logged as warnings and never retried.
type Relay struct {
	urls   []string
	client *http.Client
	logger *slog.Logger
	jobs   chan delivery
	wg     sync.WaitGroup

	mu     sync.RWMutex
	closed bool
}

// New starts a relay to urls with the given number of workers and queue size.
func New(urls []string, workers, queueSize int, logger *slog.Logger) *Relay {
	r := &Relay{
		urls:   urls,
		client: &http.Client{Timeout: Timeout},
		logger: logger,
		jobs:   make(chan delivery, queueSize),
	}
	for range workers {
		r.wg.Go(func() {
			for d := range r.jobs {
				r.send(d)
			}
		})
	}
	return r
}

// Forward queues a delivery for forwarding without blocking; a full queue drops it.
func (r *Relay) Forward(header http.Header, body []byte) {
	d := delivery{header: http.Header{}, body: body}
	for _, h := range forwardedHeaders {
		for _, v := range header.Values(h) {
			d.header.Add(h, v)
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return
	}
	select {
	case r.jobs <- d:
	default:
		r.logger.Warn("Relay queue is full; delivery not forwarded", "delivery", header.Get("X-GitHub-Delivery"))
	}
}

func (r *Relay) send(d delivery) {
	for _, u := range r.urls {
		if err := r.post(u, d); err != nil {
			r.logger.Warn("Could not relay delivery", "url", u, "delivery", d.header.Get("X-GitHub-Delivery"), "error", err)
		}
	}
}

func (r *Relay) post(u string, d delivery) error {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(d.body))
	if err != nil {
		return err
	}
	req.Header = d.header.Clone()
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

// Close stops accepting deliveries and waits (until ctx ends) for queued ones.
func (r *Relay) Close(ctx context.Context) {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.jobs)
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

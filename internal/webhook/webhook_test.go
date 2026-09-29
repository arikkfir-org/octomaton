package webhook

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"octomaton.dev/internal/metrics"
	"octomaton.dev/internal/metrics/metricstest"
)

var testSecret = []byte("s3cr3t")

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	valid := Sign(testSecret, body)
	tests := []struct {
		name   string
		secret []byte
		body   []byte
		header string
		want   bool
	}{
		{name: "valid", secret: testSecret, body: body, header: valid, want: true},
		{name: "wrong secret", secret: []byte("other"), body: body, header: valid},
		{name: "tampered body", secret: testSecret, body: []byte(`{"zen":"x"}`), header: valid},
		{name: "missing header", secret: testSecret, body: body, header: ""},
		{name: "sha1 prefix", secret: testSecret, body: body, header: strings.Replace(valid, "sha256=", "sha1=", 1)},
		{name: "prefix only", secret: testSecret, body: body, header: "sha256="},
		{name: "not hex", secret: testSecret, body: body, header: "sha256=zzzz"},
		{name: "truncated digest", secret: testSecret, body: body, header: valid[:len(valid)-2]},
		{name: "uppercase prefix", secret: testSecret, body: body, header: strings.Replace(valid, "sha256=", "SHA256=", 1)},
		{name: "empty secret never verifies", secret: nil, body: body, header: Sign(nil, body)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifySignature(tt.secret, tt.body, tt.header); got != tt.want {
				t.Fatalf("VerifySignature() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDedupe(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := NewDedupe(2, time.Minute)
	d.now = func() time.Time { return now }

	if !d.Add("a") || d.Add("a") {
		t.Fatalf("first Add must accept, second must reject")
	}
	d.Add("b")
	d.Add("c") // evicts the oldest, "a"
	if !d.Add("a") {
		t.Fatalf("an evicted ID must be accepted again")
	}
	d.Remove("a")
	if !d.Add("a") {
		t.Fatalf("a removed ID must be accepted again")
	}
	now = now.Add(2 * time.Minute)
	if !d.Add("c") {
		t.Fatalf("an expired ID must be accepted again")
	}
}

func testMetrics(t *testing.T) *metrics.Metrics { return metricstest.New(t).Metrics }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPool(t *testing.T) {
	m := testMetrics(t)
	p := NewPool(2, 10, time.Second, discardLogger(), m)
	var ran atomic.Int32
	for range 5 {
		if !p.Submit(Job{Name: "j", Run: func(context.Context) { ran.Add(1) }}) {
			t.Fatalf("Submit rejected a job with room in the queue")
		}
	}
	p.Submit(Job{Name: "panics", Run: func(context.Context) { panic("boom") }})
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if ran.Load() != 5 {
		t.Fatalf("ran %d jobs, want 5", ran.Load())
	}
	if p.Submit(Job{Name: "late", Run: func(context.Context) {}}) {
		t.Fatalf("Submit after Shutdown must fail")
	}
}

func TestPoolQueueFull(t *testing.T) {
	p := NewPool(1, 1, time.Second, discardLogger(), testMetrics(t))
	release := make(chan struct{})
	started := make(chan struct{})
	p.Submit(Job{Name: "blocker", Run: func(context.Context) { close(started); <-release }})
	<-started
	if !p.Submit(Job{Name: "queued", Run: func(context.Context) {}}) {
		t.Fatalf("the queue has room for one job")
	}
	if p.Submit(Job{Name: "overflow", Run: func(context.Context) {}}) {
		t.Fatalf("Submit must fail when the queue is full")
	}
	close(release)
	_ = p.Shutdown(context.Background())
}

func TestPoolShutdownTimeoutCancelsJobs(t *testing.T) {
	p := NewPool(1, 1, time.Minute, discardLogger(), testMetrics(t))
	cancelled := make(chan struct{})
	p.Submit(Job{Name: "slow", Run: func(ctx context.Context) { <-ctx.Done(); close(cancelled) }})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); err == nil {
		t.Fatalf("Shutdown must report jobs cancelled at the deadline")
	}
	<-cancelled
}

type stubRouter struct {
	mu      sync.Mutex
	routed  []string
	reason  string
	ran     chan string
	payload any
}

func (r *stubRouter) Route(event, delivery string, payload any) (Job, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routed = append(r.routed, event+"/"+delivery)
	r.payload = payload
	if r.reason != "" {
		return Job{}, r.reason
	}
	return Job{Name: event, Run: func(context.Context) { r.ran <- delivery }}, ""
}

type stubRelay struct {
	mu        sync.Mutex
	forwarded []string
}

func (s *stubRelay) Forward(header http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forwarded = append(s.forwarded, header.Get("X-GitHub-Event")+":"+string(body))
}

func newHandler(t *testing.T, router Router, queue int) (*Handler, *stubRelay, *metricstest.Metrics) {
	t.Helper()
	m := metricstest.New(t)
	pool := NewPool(1, queue, time.Second, discardLogger(), m.Metrics)
	t.Cleanup(func() { _ = pool.Shutdown(context.Background()) })
	relay := &stubRelay{}
	return &Handler{Secret: testSecret, Router: router, Pool: pool, Dedupe: NewDedupe(100, time.Hour), Metrics: m.Metrics, Logger: discardLogger(), Relay: relay}, relay, m
}

func post(h http.Handler, event, delivery string, body []byte, sign bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/github/hooks", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("Content-Type", "application/json")
	if sign {
		req.Header.Set(SignatureHeader, Sign(testSecret, body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const pushBody = `{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567","repository":{"id":1,"name":"r","full_name":"o/r","owner":{"login":"o"}},"installation":{"id":7}}`

func TestHandler(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		event      string
		body       string
		sign       bool
		reason     string
		wantStatus int
		wantRun    bool
		wantRelay  bool
	}{
		{name: "GET is rejected", method: http.MethodGet, event: "push", body: pushBody, sign: true, wantStatus: http.StatusMethodNotAllowed},
		{name: "unsigned is rejected", event: "push", body: pushBody, wantStatus: http.StatusUnauthorized},
		{name: "ping answers pong", event: "ping", body: `{"zen":"z"}`, sign: true, wantStatus: http.StatusOK},
		{name: "missing event header", event: "", body: `{}`, sign: true, wantStatus: http.StatusBadRequest},
		{name: "unhandled event is acknowledged", event: "star", body: `{}`, sign: true, wantStatus: http.StatusAccepted},
		{name: "invalid payload", event: "push", body: `not json`, sign: true, wantStatus: http.StatusBadRequest, wantRelay: true},
		{name: "ignored by the router", event: "push", body: pushBody, sign: true, reason: "not interesting", wantStatus: http.StatusAccepted, wantRelay: true},
		{name: "accepted push is processed and relayed", event: "push", body: pushBody, sign: true, wantStatus: http.StatusAccepted, wantRun: true, wantRelay: true},
		{name: "check_run is processed, not relayed", event: "check_run", body: `{"action":"rerequested"}`, sign: true, wantStatus: http.StatusAccepted, wantRun: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := &stubRouter{reason: tt.reason, ran: make(chan string, 1)}
			h, relay, _ := newHandler(t, router, 10)
			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, "/github/hooks", strings.NewReader(tt.body))
			req.Header.Set("X-GitHub-Event", tt.event)
			req.Header.Set("X-GitHub-Delivery", "d-1")
			if tt.sign {
				req.Header.Set(SignatureHeader, Sign(testSecret, []byte(tt.body)))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			if tt.wantRun {
				select {
				case d := <-router.ran:
					if d != "d-1" {
						t.Fatalf("job ran for delivery %q", d)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("the job did not run")
				}
			}
			relay.mu.Lock()
			relayed := len(relay.forwarded) > 0
			relay.mu.Unlock()
			if relayed != tt.wantRelay {
				t.Fatalf("relayed = %v, want %v", relayed, tt.wantRelay)
			}
		})
	}
}

func TestHandlerDedupesDeliveries(t *testing.T) {
	router := &stubRouter{ran: make(chan string, 2)}
	h, _, m := newHandler(t, router, 10)
	if rec := post(h, "push", "same", []byte(pushBody), true); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery: %d", rec.Code)
	}
	rec := post(h, "push", "same", []byte(pushBody), true)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("second delivery: %d %q, want 202 duplicate", rec.Code, rec.Body.String())
	}
	<-router.ran
	select {
	case <-router.ran:
		t.Fatalf("a duplicate delivery was processed")
	case <-time.After(100 * time.Millisecond):
	}
	if got := m.Count(t, "octomaton.webhooks.received", attribute.String("event", "push")); got != 2 {
		t.Fatalf("received metric = %v, want 2", got)
	}
}

func TestHandlerQueueFullAnswers503AndForgetsDelivery(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	router := routerFunc(func(event, delivery string, payload any) (Job, string) {
		return Job{Name: delivery, Run: func(context.Context) { started <- struct{}{}; <-block }}, ""
	})
	h, _, m := newHandler(t, router, 1)
	defer close(block)
	post(h, "push", "d1", []byte(pushBody), true) // taken by the worker
	<-started
	post(h, "push", "d2", []byte(pushBody), true) // fills the queue
	rec := post(h, "push", "d3", []byte(pushBody), true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the queue is full", rec.Code)
	}
	if !h.Dedupe.Add("d3") {
		t.Fatalf("a delivery answered with 503 must be accepted when redelivered")
	}
	if got := m.Count(t, "octomaton.webhooks.rejected", attribute.String("event", "push"), attribute.String("reason", "queue_full")); got != 1 {
		t.Fatalf("rejected metric = %v, want 1", got)
	}
}

type routerFunc func(event, delivery string, payload any) (Job, string)

func (f routerFunc) Route(event, delivery string, payload any) (Job, string) {
	return f(event, delivery, payload)
}

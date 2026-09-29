package relay

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type receiver struct {
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
}

func (r *receiver) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(body))
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()
		w.WriteHeader(status)
	}
}

func TestRelayForwardsDeliveries(t *testing.T) {
	ok, failing := &receiver{}, &receiver{}
	okSrv := httptest.NewServer(ok.handler(http.StatusOK))
	defer okSrv.Close()
	failSrv := httptest.NewServer(failing.handler(http.StatusInternalServerError))
	defer failSrv.Close()

	r := New([]string{failSrv.URL, okSrv.URL}, 2, 10, slog.New(slog.NewTextHandler(io.Discard, nil)))
	header := http.Header{}
	header.Set("X-GitHub-Event", "push")
	header.Set("X-GitHub-Delivery", "d-1")
	header.Set("X-Hub-Signature-256", "sha256=abc")
	header.Set("Content-Type", "application/json")
	header.Set("Authorization", "must not be forwarded")
	r.Forward(header, []byte(`{"ref":"refs/heads/main"}`))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.Close(ctx)

	for name, rec := range map[string]*receiver{"ok": ok, "failing": failing} {
		rec.mu.Lock()
		if len(rec.bodies) != 1 || rec.bodies[0] != `{"ref":"refs/heads/main"}` {
			t.Fatalf("%s receiver got %v", name, rec.bodies)
		}
		h := rec.headers[0]
		if h.Get("X-GitHub-Event") != "push" || h.Get("X-Hub-Signature-256") != "sha256=abc" || h.Get("X-GitHub-Delivery") != "d-1" || h.Get("Authorization") != "" {
			t.Fatalf("%s receiver headers = %v", name, h)
		}
		rec.mu.Unlock()
	}
	r.Forward(header, []byte("late")) // after Close: dropped, no panic
}

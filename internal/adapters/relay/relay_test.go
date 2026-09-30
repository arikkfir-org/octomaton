package relay

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRelayWarningsNameTheDelivery(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantWarn bool
	}{
		{name: "receiver refuses the delivery", status: http.StatusBadRequest, wantWarn: true},
		{name: "receiver fails", status: http.StatusInternalServerError, wantWarn: true},
		{name: "receiver accepts the delivery", status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer((&receiver{}).handler(tt.status))
			defer srv.Close()
			var logs bytes.Buffer
			r := New([]string{srv.URL}, 1, 10, slog.New(slog.NewJSONHandler(&logs, nil)))

			// Headers as net/http parses them from GitHub's request: canonical keys.
			header := http.Header{}
			header.Set("X-GitHub-Event", "push")
			header.Set("X-GitHub-Delivery", "d-42")
			r.Forward(header, []byte(`{}`))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r.Close(ctx)

			var warnings []map[string]any
			dec := json.NewDecoder(&logs)
			for dec.More() {
				var rec map[string]any
				if err := dec.Decode(&rec); err != nil {
					t.Fatalf("decoding log output: %v", err)
				}
				if rec["level"] == "WARN" {
					warnings = append(warnings, rec)
				}
			}
			if !tt.wantWarn {
				if len(warnings) != 0 {
					t.Fatalf("warnings = %v, want none", warnings)
				}
				return
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want one", warnings)
			}
			if got := warnings[0]["delivery"]; got != "d-42" {
				t.Fatalf("warning delivery = %v, want d-42", got)
			}
		})
	}
}

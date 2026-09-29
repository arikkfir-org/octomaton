package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadiness(t *testing.T) {
	tests := []struct {
		name            string
		pingErr         error
		leading, synced bool
		stopped         bool
		wantCode        int
		wantBody        string
	}{
		{name: "follower", wantCode: http.StatusOK, wantBody: "ok\n"},
		{name: "leader with a synced informer", leading: true, synced: true, wantCode: http.StatusOK, wantBody: "ok\n"},
		{name: "leader syncing its informer", leading: true, wantCode: http.StatusServiceUnavailable,
			wantBody: "not ready: PipelineRun informer not synced yet\n"},
		{name: "API server unreachable", pingErr: errors.New("connection refused"), wantCode: http.StatusServiceUnavailable,
			wantBody: "not ready: kubernetes API unreachable: connection refused\n"},
		{name: "shutting down", stopped: true, wantCode: http.StatusServiceUnavailable, wantBody: "not ready: shutting down\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rd := &Readiness{
				Ping:    func(context.Context) error { return tt.pingErr },
				Leading: func() bool { return tt.leading },
				Synced:  func() bool { return tt.synced },
			}
			if tt.stopped {
				rd.stop()
			}
			rec := httptest.NewRecorder()
			rd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tt.wantCode || rec.Body.String() != tt.wantBody {
				t.Fatalf("%d %q, want %d %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
		})
	}
}

func TestReadinessReusesPings(t *testing.T) {
	tests := []struct {
		name      string
		ttl       time.Duration
		wantPings int
	}{
		{name: "within the TTL", ttl: time.Hour, wantPings: 1},
		{name: "no TTL", ttl: 0, wantPings: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pings := 0
			rd := &Readiness{
				Ping:    func(context.Context) error { pings++; return nil },
				Leading: func() bool { return false },
				TTL:     tt.ttl,
			}
			for range 3 {
				if err := rd.check(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if pings != tt.wantPings {
				t.Fatalf("pings = %d, want %d", pings, tt.wantPings)
			}
		})
	}
}

func TestReadinessBoundsThePing(t *testing.T) {
	var deadline time.Time
	rd := &Readiness{
		Ping: func(ctx context.Context) error {
			deadline, _ = ctx.Deadline()
			return nil
		},
		Leading: func() bool { return false },
	}
	start := time.Now()
	if err := rd.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deadline.IsZero() || deadline.After(start.Add(pingTimeout+time.Second)) {
		t.Fatalf("ping deadline %v, want at most %v after %v", deadline, pingTimeout, start)
	}
}

package http

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func named(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(name)) })
}

func TestRoutes(t *testing.T) {
	mux := routes(named("hook"), named("ready"))
	tests := []struct {
		method, path string
		wantCode     int
		wantBody     string
	}{
		{method: http.MethodPost, path: "/github/hooks", wantCode: http.StatusOK, wantBody: "hook"},
		{method: http.MethodGet, path: "/healthz", wantCode: http.StatusOK, wantBody: "ok\n"},
		{method: http.MethodGet, path: "/readyz", wantCode: http.StatusOK, wantBody: "ready"},
		{method: http.MethodGet, path: "/metrics", wantCode: http.StatusNotFound},
		{method: http.MethodPost, path: "/webhook", wantCode: http.StatusNotFound},
		{method: http.MethodPost, path: "/github/hooks/extra", wantCode: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.wantCode {
				t.Fatalf("status %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Fatalf("body %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestServeFinishesRequestsInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	webhook := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("delivered"))
	})
	readiness := &Readiness{Ping: func(context.Context) error { return nil }, Leading: func() bool { return false }}
	s := NewServer("", webhook, readiness)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.serve(ctx, listener) }()

	if code, body := get(t, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz before shutdown: %d %q", code, body)
	}
	delivery := make(chan string, 1)
	go func() {
		resp, err := http.Post(base+WebhookPath, "application/json", nil)
		if err != nil {
			delivery <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		delivery <- string(body)
	}()
	<-entered
	cancel()
	for deadline := time.Now().Add(5 * time.Second); readiness.check(context.Background()) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still ready after shutdown began")
		}
	}
	close(release)
	if got := <-delivery; got != "delivered" {
		t.Fatalf("delivery in flight got %q, want it completed", got)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve = %v, want nil after a graceful shutdown", err)
	}
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Fatal("the server still accepts connections after shutdown")
	}
}

func TestRunFailsWhenTheAddressIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	s := NewServer(taken.Addr().String(), named("hook"), &Readiness{})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run succeeded on an address in use")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not fail on an address in use")
	}
}

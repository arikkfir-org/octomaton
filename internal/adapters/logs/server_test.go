package logs

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
	s := NewServer("", named("logs"))
	tests := []struct {
		method, path string
		wantCode     int
		wantBody     string
	}{
		{method: http.MethodGet, path: "/logs/ci-a/run-pod/step-test", wantCode: http.StatusOK, wantBody: "logs"},
		{method: http.MethodGet, path: "/healthz", wantCode: http.StatusOK, wantBody: "ok\n"},
		{method: http.MethodGet, path: "/readyz", wantCode: http.StatusOK, wantBody: "ok\n"},
		{method: http.MethodPost, path: "/logs/ci-a/run-pod/step-test", wantCode: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/logs/ci-a/run-pod", wantCode: http.StatusNotFound},
		{method: http.MethodGet, path: "/logs/ci-a/run-pod/step-test/more", wantCode: http.StatusNotFound},
		{method: http.MethodGet, path: "/metrics", wantCode: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.wantCode {
				t.Fatalf("status %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Fatalf("body %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestServeFinishesLogsInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	logs := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("the log\n"))
	})
	s := NewServer("", logs)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.serve(ctx, listener) }()

	if code, body := fetch(t, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz before shutdown: %d %q", code, body)
	}
	log := make(chan string, 1)
	go func() {
		resp, err := http.Get(base + "/logs/ci-a/run-pod/step-test")
		if err != nil {
			log <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		log <- string(body)
	}()
	<-entered
	cancel()
	for deadline := time.Now().Add(5 * time.Second); !s.stopping.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still ready after shutdown began")
		}
	}
	rec := httptest.NewRecorder()
	s.ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while stopping: %d, want 503", rec.Code)
	}
	close(release)
	if got := <-log; got != "the log\n" {
		t.Fatalf("log in flight got %q, want it whole", got)
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
	done := make(chan error, 1)
	go func() { done <- NewServer(taken.Addr().String(), named("logs")).Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run succeeded on an address in use")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not fail on an address in use")
	}
}

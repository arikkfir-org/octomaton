package logs

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// route is the logs' endpoint: the Tekton Dashboard's external logs URL ends in /logs, and the
// Dashboard appends /<namespace>/<pod>/<container> to it.
const route = "GET /logs/{namespace}/{pod}/{container}"

// shutdownTimeout bounds the wait for the logs in flight when the server stops.
const shutdownTimeout = 10 * time.Second

// Server serves the logs and the probes on one address.
type Server struct {
	http     *http.Server
	stopping atomic.Bool
}

// NewServer returns a server for address. Requests for logs are traced and measured with
// OpenTelemetry.
func NewServer(address string, logs http.Handler) *Server {
	s := &Server{}
	s.http = &http.Server{
		Addr:              address,
		Handler:           s.routes(otelhttp.NewHandler(logs, "logs")),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Leaves time for the last line of a log whose read ran out of time.
		WriteTimeout:   readTimeout + 30*time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
		ErrorLog:       slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}
	return s
}

func (s *Server) routes(logs http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(route, logs)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", s.ready)
	return mux
}

// ready answers the readiness probe: ready until the server stops.
func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	if s.stopping.Load() {
		http.Error(w, "not ready: shutting down", http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
}

// Run serves until ctx is cancelled, then fails the readiness probe, stops accepting connections
// and waits up to shutdownTimeout for the logs in flight. It returns an error only when the server
// cannot serve.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	return s.serve(ctx, listener)
}

func (s *Server) serve(ctx context.Context, listener net.Listener) error {
	served := make(chan error, 1)
	go func() {
		slog.InfoContext(ctx, "Serving HTTP", "address", listener.Addr().String())
		served <- s.http.Serve(listener)
	}()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	s.stopping.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		slog.WarnContext(ctx, "HTTP server shutdown incomplete", "error", err)
	}
	return nil
}

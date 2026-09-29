// Package http serves Octomaton's endpoints: GitHub's webhook deliveries and the Kubernetes probes.
package http

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// WebhookPath is where GitHub delivers the App's webhooks.
const WebhookPath = "/github/hooks"

// shutdownTimeout bounds the wait for requests in flight when the server stops.
const shutdownTimeout = 10 * time.Second

// Server serves the webhook and the probes on one address.
type Server struct {
	http      *http.Server
	readiness *Readiness
}

// NewServer returns a server for address. Webhook requests are traced and measured with
// OpenTelemetry.
func NewServer(address string, webhook http.Handler, readiness *Readiness) *Server {
	return &Server{
		readiness: readiness,
		http: &http.Server{
			Addr:              address,
			Handler:           routes(otelhttp.NewHandler(webhook, "webhook"), readiness),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
			ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		},
	}
}

func routes(webhook, readiness http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(WebhookPath, webhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })
	mux.Handle("/readyz", readiness)
	return mux
}

// Run serves until ctx is cancelled, then fails the readiness probe, stops accepting connections
// and waits up to shutdownTimeout for requests in flight. It returns an error only when the server
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
	s.readiness.stop()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		slog.WarnContext(ctx, "HTTP server shutdown incomplete", "error", err)
	}
	return nil
}

// Command octomaton-logs serves the logs of CI containers from Cloud Logging to the Tekton
// Dashboard, which asks for them when a step's pod is gone.
//
// It takes no arguments: environment variables configure it (see the config and telemetry
// packages), and it reads Cloud Logging with the Application Default Credentials.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"octomaton.dev/internal/adapters/logs"
	"octomaton.dev/internal/system/buildinfo"
	"octomaton.dev/internal/system/config"
	"octomaton.dev/internal/system/telemetry"
)

// service names the process in its telemetry.
const service = "octomaton-logs"

// flushTimeout bounds the flush of the telemetry exporters at exit.
const flushTimeout = 5 * time.Second

func main() {
	os.Exit(run())
}

// run serves the logs until SIGTERM or SIGINT; it returns the exit code.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.Setup(ctx, service, buildinfo.Version())
	if err != nil {
		slog.ErrorContext(ctx, "Cannot set up telemetry", "error", err)
		return 1
	}
	defer flush(tel)

	cfg, err := config.LoadLogs()
	if err != nil {
		slog.ErrorContext(ctx, "Invalid configuration", "error", err)
		return 1
	}
	// Tokens may be refreshed while the server drains, after the signal.
	tokens, err := google.DefaultTokenSource(context.WithoutCancel(ctx), logs.Scope)
	if err != nil {
		slog.ErrorContext(ctx, "Cannot find the Google credentials", "error", err)
		return 1
	}
	slog.InfoContext(ctx, "Starting octomaton-logs", "version", buildinfo.Version())
	return serve(ctx, cfg, tokens)
}

// serve wires the logs of cfg, read with tokens, and serves them until ctx is cancelled; it
// returns the exit code.
func serve(ctx context.Context, cfg *config.Logs, tokens oauth2.TokenSource) int {
	reader := &logs.CloudLogging{
		BaseURL:  logs.DefaultBaseURL,
		View:     cfg.View,
		Project:  cfg.Project(),
		Cluster:  cfg.Cluster,
		Location: cfg.Location,
		Client:   logs.NewClient(tokens),
	}
	handler := logs.NewHandler(reader, cfg.NamespacePrefix, slog.Default().With("component", "logs"))
	context.AfterFunc(ctx, func() { slog.Info("Shutting down") })
	if err := logs.NewServer(cfg.Address, handler).Run(ctx); err != nil {
		slog.ErrorContext(ctx, "HTTP server failed", "error", err)
		return 1
	}
	slog.Info("Stopped")
	return 0
}

func flush(tel *telemetry.Telemetry) {
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		slog.Warn("Telemetry not fully flushed", "error", err)
	}
}

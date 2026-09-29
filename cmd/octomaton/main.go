// Command octomaton receives GitHub App webhooks, creates the Tekton PipelineRuns that repositories
// declare in .octomaton.yaml, and reports their progress back to GitHub as check runs.
//
// It takes no arguments: environment variables configure it (see the config and telemetry
// packages). The octomaton-lint command validates .octomaton.yaml files.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"octomaton.dev/internal/buildinfo"
	"octomaton.dev/internal/config"
	"octomaton.dev/internal/telemetry"
)

// service names the process in its telemetry and prefixes its environment variables.
const service = "octomaton"

// flushTimeout bounds the flush of the telemetry exporters at exit.
const flushTimeout = 5 * time.Second

func main() {
	os.Exit(run())
}

// run starts Octomaton and serves until SIGTERM or SIGINT; it returns the exit code.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.Setup(ctx, service, buildinfo.Version())
	if err != nil {
		slog.ErrorContext(ctx, "Cannot set up telemetry", "error", err)
		return 1
	}
	defer flush(tel)

	cfg, err := config.Load()
	if err != nil {
		slog.ErrorContext(ctx, "Invalid configuration", "error", err)
		return 1
	}
	slog.InfoContext(ctx, "Starting Octomaton", "version", buildinfo.Version())
	a, err := newApp(cfg, tel.MetricsHandler())
	if err != nil {
		slog.ErrorContext(ctx, "Cannot start Octomaton", "error", err)
		return 1
	}
	return a.run(ctx)
}

func flush(tel *telemetry.Telemetry) {
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		slog.Warn("Telemetry not fully flushed", "error", err)
	}
}

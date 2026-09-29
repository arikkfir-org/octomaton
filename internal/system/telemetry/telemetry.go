// Package telemetry sets up Octomaton's logs, metrics and traces.
//
// On GKE, logs are JSON on stdout with the fields Cloud Logging reads (GKE's logging agent ships
// them), and metrics and traces go to Cloud Monitoring and Cloud Trace through Google Cloud's
// Telemetry (OTLP) API. Anywhere else, logs are text and nothing is exported.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/kelseyhightower/envconfig"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Config is read from the environment by Setup, prefixed with the service name: for the service
// octomaton, OCTOMATON_LOG_LEVEL.
type Config struct {
	// LogLevel is the lowest level logged: debug, info, warn or error.
	LogLevel slog.Level `envconfig:"LOG_LEVEL" default:"info"`
}

// Telemetry holds the providers Setup installed.
type Telemetry struct {
	shutdowns []func(context.Context) error
}

// Setup reads the Config of service from the environment, then installs the process-wide logger
// and propagators and, on GKE, the OpenTelemetry providers exporting to Google Cloud. Call
// Shutdown before the process exits to flush them.
func Setup(ctx context.Context, service, version string) (*Telemetry, error) {
	var c Config
	if err := envconfig.Process(service, &c); err != nil {
		return nil, err
	}
	gcp, err := detect(ctx)
	if err != nil {
		return nil, err
	}
	return setup(ctx, service, version, c, os.Stdout, gcp)
}

// setup installs the logger writing to out and, when gcp is not nil, the providers exporting to it.
func setup(ctx context.Context, service, version string, c Config, out io.Writer, gcp *exporters) (*Telemetry, error) {
	var project string
	if gcp != nil {
		project = gcp.project
	}
	installLogger(newLogHandler(c, out, project))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	t := &Telemetry{}
	if gcp == nil {
		return t, nil
	}
	if err := t.setupProviders(ctx, service, version, gcp); err != nil {
		return nil, errors.Join(err, t.Shutdown(ctx))
	}
	slog.InfoContext(ctx, "Exporting metrics and traces to Google Cloud", "project", project)
	return t, nil
}

// setupProviders installs the meter and tracer providers exporting to gcp.
func (t *Telemetry) setupProviders(ctx context.Context, service, version string, gcp *exporters) error {
	res, err := newResource(ctx, service, version, gcp.project)
	if err != nil {
		return fmt.Errorf("creating the OpenTelemetry resource: %w", err)
	}
	if err := t.setupMetrics(ctx, res, gcp); err != nil {
		return err
	}
	return t.setupTraces(ctx, res, gcp)
}

// Shutdown flushes and stops the providers, last installed first. Later calls do nothing.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for i := len(t.shutdowns) - 1; i >= 0; i-- {
		errs = append(errs, t.shutdowns[i](ctx))
	}
	t.shutdowns = nil
	return errors.Join(errs...)
}

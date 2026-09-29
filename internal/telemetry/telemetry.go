// Package telemetry sets up Octomaton's logs and OpenTelemetry.
//
// Logs go to stdout through slog: JSON shaped for Cloud Logging, or text. Metrics are recorded
// through OpenTelemetry and served for Prometheus scraping. Traces, and a copy of the logs, are
// exported only when the standard OTEL_TRACES_EXPORTER or OTEL_LOGS_EXPORTER variable names an
// exporter (for example otlp, configured by the OTEL_EXPORTER_OTLP_* variables). The standard
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES variables describe the process to the exporters.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/kelseyhightower/envconfig"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Config is read from the environment by Setup, prefixed with the service name: for the service
// octomaton, OCTOMATON_LOG_LEVEL and OCTOMATON_LOG_FORMAT.
type Config struct {
	// LogLevel is the lowest level logged: debug, info, warn or error.
	LogLevel slog.Level `envconfig:"LOG_LEVEL" default:"info"`
	// LogFormat is json (one object per line, as Cloud Logging reads them) or text.
	LogFormat string `envconfig:"LOG_FORMAT" default:"json"`
}

// Telemetry holds the providers Setup installed.
type Telemetry struct {
	metrics   http.Handler
	shutdowns []func(context.Context) error
}

// Setup reads the Config of service from the environment, then installs the process-wide logger,
// propagators and OpenTelemetry providers. Call Shutdown before the process exits to flush the
// exporters.
func Setup(ctx context.Context, service, version string) (*Telemetry, error) {
	var c Config
	if err := envconfig.Process(service, &c); err != nil {
		return nil, err
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return nil, fmt.Errorf("%s_LOG_FORMAT: %q is not json or text", strings.ToUpper(service), c.LogFormat)
	}
	return setup(ctx, service, version, c, os.Stdout)
}

func setup(ctx context.Context, service, version string, c Config, out io.Writer) (*Telemetry, error) {
	logs := newLogHandler(c, out)
	installLogger(logs)

	res, err := newResource(ctx, service, version)
	if err != nil {
		return nil, fmt.Errorf("creating the OpenTelemetry resource: %w", err)
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	t := &Telemetry{}
	exported, err := t.setupProviders(ctx, res, service, c.LogLevel)
	if err != nil {
		return nil, errors.Join(err, t.Shutdown(ctx))
	}
	if exported != nil {
		installLogger(slog.NewMultiHandler(logs, exported))
	}
	return t, nil
}

// setupProviders installs the meter, tracer and logger providers. It returns the handler exporting
// log records, or nil when logs are not exported.
func (t *Telemetry) setupProviders(ctx context.Context, res *resource.Resource, service string, level slog.Level) (slog.Handler, error) {
	if err := t.setupMetrics(res); err != nil {
		return nil, err
	}
	if err := t.setupTraces(ctx, res); err != nil {
		return nil, err
	}
	return t.setupLogs(ctx, res, service, level)
}

// MetricsHandler serves the metrics for Prometheus.
func (t *Telemetry) MetricsHandler() http.Handler {
	return t.metrics
}

// Shutdown flushes and stops the providers, last installed first.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for i := len(t.shutdowns) - 1; i >= 0; i-- {
		errs = append(errs, t.shutdowns[i](ctx))
	}
	return errors.Join(errs...)
}

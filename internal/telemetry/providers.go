package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// setupMetrics installs a meter provider whose Prometheus exporter feeds MetricsHandler, next to
// the Go runtime and process collectors.
func (t *Telemetry) setupMetrics(res *resource.Resource) error {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	exporter, err := otelprometheus.New(otelprometheus.WithRegisterer(registry), otelprometheus.WithoutScopeInfo(), otelprometheus.WithoutTargetInfo())
	if err != nil {
		return fmt.Errorf("creating the Prometheus exporter: %w", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(provider)
	t.shutdowns = append(t.shutdowns, provider.Shutdown)
	t.metrics = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return nil
}

// setupTraces installs a tracer provider. Its spans are exported only when OTEL_TRACES_EXPORTER
// names an exporter.
func (t *Telemetry) setupTraces(ctx context.Context, res *resource.Resource) error {
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if requested("OTEL_TRACES_EXPORTER") {
		exporter, err := autoexport.NewSpanExporter(ctx)
		if err != nil {
			return fmt.Errorf("creating the span exporter: %w", err)
		}
		if !autoexport.IsNoneSpanExporter(exporter) {
			opts = append(opts, sdktrace.WithBatcher(exporter))
		}
	}
	provider := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)
	t.shutdowns = append(t.shutdowns, provider.Shutdown)
	return nil
}

// setupLogs returns a slog handler exporting log records when OTEL_LOGS_EXPORTER names an
// exporter, and nil otherwise.
func (t *Telemetry) setupLogs(ctx context.Context, res *resource.Resource, service string, level slog.Level) (slog.Handler, error) {
	if !requested("OTEL_LOGS_EXPORTER") {
		return nil, nil
	}
	exporter, err := autoexport.NewLogExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating the log exporter: %w", err)
	}
	if autoexport.IsNoneLogExporter(exporter) {
		return nil, nil
	}
	provider := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
	global.SetLoggerProvider(provider)
	t.shutdowns = append(t.shutdowns, provider.Shutdown)
	return minLevel{Handler: otelslog.NewHandler(service, otelslog.WithLoggerProvider(provider)), level: level}, nil
}

// requested reports whether an OTEL_*_EXPORTER variable names an exporter. autoexport falls back to
// OTLP when the variable is unset, which fails without a collector, so unset means none.
func requested(variable string) bool {
	return os.Getenv(variable) != ""
}

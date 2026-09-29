package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// setupMetrics installs a meter provider exporting every minute (OTEL_METRIC_EXPORT_INTERVAL
// changes that).
func (t *Telemetry) setupMetrics(ctx context.Context, res *resource.Resource, gcp *exporters) error {
	exporter, err := gcp.newMetrics(ctx)
	if err != nil {
		return fmt.Errorf("creating the metric exporter: %w", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	otel.SetMeterProvider(provider)
	t.shutdowns = append(t.shutdowns, provider.Shutdown)
	return nil
}

// setupTraces installs a tracer provider exporting every span, in batches.
func (t *Telemetry) setupTraces(ctx context.Context, res *resource.Resource, gcp *exporters) error {
	exporter, err := gcp.newSpans(ctx)
	if err != nil {
		return fmt.Errorf("creating the span exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(provider)
	t.shutdowns = append(t.shutdowns, provider.Shutdown)
	return nil
}

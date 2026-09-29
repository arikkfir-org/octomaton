package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// newResource describes this process to the exporters. OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES (e.g. k8s.pod.name) are applied last, so they win.
func newResource(ctx context.Context, service, version string) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
}

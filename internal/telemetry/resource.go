package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/detectors/gcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// newResource describes the process to Google Cloud: the service and its replica (the host name,
// which is the pod's name), the GKE cluster and location the GCP detector finds, and the project,
// which the Telemetry API requires as gcp.project_id. OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
// (e.g. k8s.pod.name) are applied last, so they win.
func newResource(ctx context.Context, service, version, project string) (*resource.Resource, error) {
	host, _ := os.Hostname()
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(service),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(host),
			attribute.String("gcp.project_id", project),
		),
		resource.WithDetectors(gcp.NewDetector()),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if errors.Is(err, resource.ErrPartialResource) {
		slog.WarnContext(ctx, "Some resource attributes were not detected", "error", err)
		return res, nil
	}
	return res, err
}

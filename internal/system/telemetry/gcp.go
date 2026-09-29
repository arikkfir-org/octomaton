package telemetry

import (
	"context"
	"fmt"
	"os"

	"cloud.google.com/go/compute/metadata"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
)

// telemetryAPI is the gRPC endpoint of Google Cloud's Telemetry (OTLP) API.
const telemetryAPI = "telemetry.googleapis.com:443"

// The metadata server answers whether the process runs on Google Cloud, and in which project. The
// metadata client caches both, so tests replace these instead.
var (
	onGCE     = metadata.OnGCEWithContext
	projectOf = metadata.ProjectIDWithContext
)

// exporters send metrics and traces to a Google Cloud project.
type exporters struct {
	project    string
	newMetrics func(context.Context) (sdkmetric.Exporter, error)
	newSpans   func(context.Context) (sdktrace.SpanExporter, error)
}

// detect returns the exporters of the project the process runs in when it runs on GKE (in
// Kubernetes, with a GCP metadata server), and nil anywhere else.
func detect(ctx context.Context) (*exporters, error) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" || !onGCE(ctx) {
		return nil, nil
	}
	project, err := projectOf(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the project from the metadata server: %w", err)
	}
	return telemetryAPIExporters(project), nil
}

// telemetryAPIExporters send to project through the Telemetry API, authenticated with the
// Application Default Credentials: on GKE, the pod's Kubernetes ServiceAccount.
func telemetryAPIExporters(project string) *exporters {
	return &exporters{
		project: project,
		newMetrics: func(ctx context.Context) (sdkmetric.Exporter, error) {
			creds, err := oauth.NewApplicationDefault(ctx, "https://www.googleapis.com/auth/cloud-platform")
			if err != nil {
				return nil, fmt.Errorf("finding the Application Default Credentials: %w", err)
			}
			return otlpmetricgrpc.New(ctx, metricOptions(project, creds)...)
		},
		newSpans: func(ctx context.Context) (sdktrace.SpanExporter, error) {
			creds, err := oauth.NewApplicationDefault(ctx, "https://www.googleapis.com/auth/cloud-platform")
			if err != nil {
				return nil, fmt.Errorf("finding the Application Default Credentials: %w", err)
			}
			return otlptracegrpc.New(ctx, traceOptions(project, creds)...)
		},
	}
}

// quotaProject bills the requests to project; the Telemetry API needs a quota project for
// credentials other than a service account's.
func quotaProject(project string) map[string]string {
	return map[string]string{"x-goog-user-project": project}
}

func metricOptions(project string, creds credentials.PerRPCCredentials) []otlpmetricgrpc.Option {
	return []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(telemetryAPI),
		otlpmetricgrpc.WithDialOption(grpc.WithPerRPCCredentials(creds)),
		otlpmetricgrpc.WithHeaders(quotaProject(project)),
	}
}

func traceOptions(project string, creds credentials.PerRPCCredentials) []otlptracegrpc.Option {
	return []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(telemetryAPI),
		otlptracegrpc.WithDialOption(grpc.WithPerRPCCredentials(creds)),
		otlptracegrpc.WithHeaders(quotaProject(project)),
	}
}

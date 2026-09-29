package telemetry

import (
	"context"
	"errors"
	"net"
	"testing"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	grpcmd "google.golang.org/grpc/metadata"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name        string
		kubernetes  string // KUBERNETES_SERVICE_HOST
		onGCE       bool
		project     string
		projectErr  error
		wantProject string // empty: not on GKE
		wantErr     bool
	}{
		{name: "outside Kubernetes, even on Google Cloud", onGCE: true, project: "hub"},
		{name: "Kubernetes outside Google Cloud", kubernetes: "10.0.0.1"},
		{name: "GKE", kubernetes: "10.0.0.1", onGCE: true, project: "hub", wantProject: "hub"},
		{name: "GKE without a project", kubernetes: "10.0.0.1", onGCE: true, projectErr: errors.New("unreachable"), wantErr: true},
	}
	savedOnGCE, savedProjectOf := onGCE, projectOf
	t.Cleanup(func() { onGCE, projectOf = savedOnGCE, savedProjectOf })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", tt.kubernetes)
			onGCE = func(context.Context) bool { return tt.onGCE }
			projectOf = func(context.Context) (string, error) { return tt.project, tt.projectErr }
			gcp, err := detect(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("detect error = %v, want error %v", err, tt.wantErr)
			}
			var project string
			if gcp != nil {
				project = gcp.project
			}
			if project != tt.wantProject {
				t.Fatalf("detect project = %q, want %q", project, tt.wantProject)
			}
		})
	}
}

// staticCreds sends a fixed bearer token, over the test's plaintext connection.
type staticCreds string

func (c staticCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(c)}, nil
}

func (staticCreds) RequireTransportSecurity() bool { return false }

type traceService struct {
	collectortrace.UnimplementedTraceServiceServer
	headers chan grpcmd.MD
}

func (s traceService) Export(ctx context.Context, _ *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	md, _ := grpcmd.FromIncomingContext(ctx)
	s.headers <- md
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

type metricsService struct {
	collectormetrics.UnimplementedMetricsServiceServer
	headers chan grpcmd.MD
}

func (s metricsService) Export(ctx context.Context, _ *collectormetrics.ExportMetricsServiceRequest) (*collectormetrics.ExportMetricsServiceResponse, error) {
	md, _ := grpcmd.FromIncomingContext(ctx)
	s.headers <- md
	return &collectormetrics.ExportMetricsServiceResponse{}, nil
}

// TestTelemetryAPIRequests sends through the Telemetry API's options, redirected to a local
// server, and checks that every request carries the credentials and the quota project.
func TestTelemetryAPIRequests(t *testing.T) {
	headers := make(chan grpcmd.MD, 1)
	server := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(server, traceService{headers: headers})
	collectormetrics.RegisterMetricsServiceServer(server, metricsService{headers: headers})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	address, creds := listener.Addr().String(), staticCreds("test-token")

	tests := []struct {
		name   string
		export func(ctx context.Context) error
	}{
		{name: "traces", export: func(ctx context.Context) error {
			exporter, err := otlptracegrpc.New(ctx, append(traceOptions("test-project", creds),
				otlptracegrpc.WithEndpoint(address), otlptracegrpc.WithInsecure())...)
			if err != nil {
				return err
			}
			defer func() { _ = exporter.Shutdown(ctx) }()
			return exporter.ExportSpans(ctx, tracetest.SpanStubs{{Name: "webhook"}}.Snapshots())
		}},
		{name: "metrics", export: func(ctx context.Context) error {
			exporter, err := otlpmetricgrpc.New(ctx, append(metricOptions("test-project", creds),
				otlpmetricgrpc.WithEndpoint(address), otlpmetricgrpc.WithInsecure())...)
			if err != nil {
				return err
			}
			defer func() { _ = exporter.Shutdown(ctx) }()
			return exporter.Export(ctx, &metricdata.ResourceMetrics{Resource: resource.Empty(), ScopeMetrics: []metricdata.ScopeMetrics{{
				Metrics: []metricdata.Metrics{{Name: "octomaton.leader", Data: metricdata.Gauge[int64]{
					DataPoints: []metricdata.DataPoint[int64]{{Value: 1}},
				}}},
			}}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.export(context.Background()); err != nil {
				t.Fatalf("export: %v", err)
			}
			md := <-headers
			if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer test-token" {
				t.Errorf("authorization = %q", got)
			}
			if got := md.Get("x-goog-user-project"); len(got) != 1 || got[0] != "test-project" {
				t.Errorf("x-goog-user-project = %q", got)
			}
		})
	}
}

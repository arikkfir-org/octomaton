package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"octomaton.dev/internal/metrics"
)

// memMetrics keeps what the periodic reader exports: each metric's data points and the resource.
type memMetrics struct {
	mu       sync.Mutex
	resource *resource.Resource
	data     map[string]metricdata.Aggregation
}

func (e *memMetrics) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

func (e *memMetrics) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

func (e *memMetrics) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resource = rm.Resource
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			e.data[m.Name] = m.Data
		}
	}
	return nil
}

func (e *memMetrics) ForceFlush(context.Context) error { return nil }
func (e *memMetrics) Shutdown(context.Context) error   { return nil }

// keptSpans keeps the spans past Shutdown, which empties an InMemoryExporter.
type keptSpans struct{ *tracetest.InMemoryExporter }

func (keptSpans) Shutdown(context.Context) error { return nil }

// fakeGCP returns exporters that keep what they receive, for the project test-project.
func fakeGCP() (*exporters, *memMetrics, *tracetest.InMemoryExporter) {
	m := &memMetrics{data: map[string]metricdata.Aggregation{}}
	spans := tracetest.NewInMemoryExporter()
	return &exporters{
		project:    "test-project",
		newMetrics: func(context.Context) (sdkmetric.Exporter, error) { return m, nil },
		newSpans:   func(context.Context) (sdktrace.SpanExporter, error) { return keptSpans{spans}, nil },
	}, m, spans
}

// start sets telemetry up, on GKE when gcp is not nil, and returns the log output after setup.
func start(t *testing.T, c Config, gcp *exporters) (*Telemetry, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	tel, err := setup(context.Background(), "octomaton", "v0.0.0-test", c, &out, gcp)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	out.Reset()
	return tel, &out
}

func TestSetupReadsTheEnvironment(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		wantLevel slog.Level
		wantErr   string
	}{
		{name: "default", wantLevel: slog.LevelInfo},
		{name: "debug", level: "debug", wantLevel: slog.LevelDebug},
		{name: "upper case", level: "WARN", wantLevel: slog.LevelWarn},
		{name: "unknown level", level: "loud", wantErr: "OCTOMATON_LOG_LEVEL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", "") // outside GKE, even when the tests run in it
			if tt.level != "" {
				t.Setenv("OCTOMATON_LOG_LEVEL", tt.level)
			}
			tel, err := Setup(context.Background(), "octomaton", "v0.0.0-test")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Setup error = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
			ctx := context.Background()
			if !slog.Default().Enabled(ctx, tt.wantLevel) || slog.Default().Enabled(ctx, tt.wantLevel-1) {
				t.Fatalf("the default logger does not log from %v exactly", tt.wantLevel)
			}
		})
	}
}

func TestLocalLogsAreText(t *testing.T) {
	_, out := start(t, Config{LogLevel: slog.LevelWarn}, nil)
	slog.Info("Dropped")
	slog.Warn("Kept", "attempt", 2)
	if got := out.String(); strings.Contains(got, "Dropped") || !strings.Contains(got, "level=WARN msg=Kept attempt=2") {
		t.Fatalf("logs = %q", got)
	}
}

func TestGKELogsUseCloudLoggingFields(t *testing.T) {
	tests := []struct {
		log      func(msg string, args ...any)
		severity string
	}{
		{log: slog.Debug, severity: "DEBUG"},
		{log: slog.Info, severity: "INFO"},
		{log: slog.Warn, severity: "WARNING"},
		{log: slog.Error, severity: "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.severity, func(t *testing.T) {
			gcp, _, _ := fakeGCP()
			_, out := start(t, Config{LogLevel: slog.LevelDebug}, gcp)
			tt.log("Hello", "repository", "arikkfir-org/docs")
			var entry map[string]any
			if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
				t.Fatalf("not one JSON object: %q: %v", out.String(), err)
			}
			source, _ := entry["logging.googleapis.com/sourceLocation"].(map[string]any)
			if entry["severity"] != tt.severity || entry["message"] != "Hello" || entry["timestamp"] == nil ||
				entry["repository"] != "arikkfir-org/docs" || !strings.HasSuffix(source["file"].(string), "telemetry_test.go") {
				t.Fatalf("entry = %v", entry)
			}
		})
	}
}

func TestGKELogsLinkToTheirTrace(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("0102030405060708")
	tests := []struct {
		name string
		ctx  context.Context
		want map[string]any
	}{
		{name: "no span", ctx: context.Background(), want: map[string]any{
			"logging.googleapis.com/trace": nil, "logging.googleapis.com/spanId": nil, "logging.googleapis.com/trace_sampled": nil,
		}},
		{name: "sampled span", ctx: trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
		})), want: map[string]any{
			"logging.googleapis.com/trace":         "projects/test-project/traces/0102030405060708090a0b0c0d0e0f10",
			"logging.googleapis.com/spanId":        "0102030405060708",
			"logging.googleapis.com/trace_sampled": true,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gcp, _, _ := fakeGCP()
			_, out := start(t, Config{LogLevel: slog.LevelInfo}, gcp)
			slog.Default().With("component", "test").InfoContext(tt.ctx, "Hello")
			var entry map[string]any
			if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
				t.Fatalf("not one JSON object: %q: %v", out.String(), err)
			}
			for key, want := range tt.want {
				if entry[key] != want {
					t.Errorf("%s = %v, want %v", key, entry[key], want)
				}
			}
		})
	}
}

func TestGKEExportsMetricsAndTraces(t *testing.T) {
	gcp, exported, spans := fakeGCP()
	tel, _ := start(t, Config{LogLevel: slog.LevelInfo}, gcp)
	m, err := metrics.New(otel.Meter("octomaton.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.RunCreated(ctx, metrics.RunCreated)
	m.ReconcileDone(ctx, "ok", 250*time.Millisecond)
	_, span := otel.Tracer("test").Start(ctx, "webhook")
	span.End()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	sum, _ := exported.data["octomaton.runs.created"].(metricdata.Sum[int64])
	if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 || !sum.IsMonotonic ||
		sum.Temporality != metricdata.CumulativeTemporality {
		t.Errorf("octomaton.runs.created = %+v, want one cumulative point of 1", sum)
	}
	histogram, _ := exported.data["octomaton.reconcile.duration"].(metricdata.Histogram[float64])
	if len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Bounds[0] != .005 {
		t.Errorf("octomaton.reconcile.duration = %+v, want seconds buckets", histogram)
	}
	if got := spans.GetSpans(); len(got) != 1 || got[0].Name != "webhook" {
		t.Fatalf("spans = %v, want the webhook span", got)
	}
	host, _ := os.Hostname()
	for name, res := range map[string]*resource.Resource{"metrics": exported.resource, "spans": spans.GetSpans()[0].Resource} {
		for key, want := range map[attribute.Key]string{
			"service.name": "octomaton", "service.version": "v0.0.0-test", "service.instance.id": host, "gcp.project_id": "test-project",
		} {
			if got, _ := res.Set().Value(key); got.AsString() != want {
				t.Errorf("%s resource %s = %q, want %q", name, key, got.AsString(), want)
			}
		}
	}
}

func TestSetupFailsWithoutExporters(t *testing.T) {
	broken := errors.New("no credentials")
	tests := []struct {
		name    string
		break_  func(*exporters)
		wantErr string
	}{
		{name: "metrics", break_: func(e *exporters) {
			e.newMetrics = func(context.Context) (sdkmetric.Exporter, error) { return nil, broken }
		}, wantErr: "creating the metric exporter: no credentials"},
		{name: "traces", break_: func(e *exporters) {
			e.newSpans = func(context.Context) (sdktrace.SpanExporter, error) { return nil, broken }
		}, wantErr: "creating the span exporter: no credentials"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gcp, _, _ := fakeGCP()
			tt.break_(gcp)
			_, err := setup(context.Background(), "octomaton", "v0.0.0-test", Config{}, &bytes.Buffer{}, gcp)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("setup error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

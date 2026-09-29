package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"octomaton.dev/internal/metrics"
)

func start(t *testing.T, c Config) (*Telemetry, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	tel, err := setup(context.Background(), "octomaton", "v0.0.0-test", c, &out)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return tel, &out
}

func TestSetupReadsTheEnvironment(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		format    string
		wantLevel slog.Level
		wantErr   string
	}{
		{name: "defaults", wantLevel: slog.LevelInfo},
		{name: "debug text", level: "debug", format: "text", wantLevel: slog.LevelDebug},
		{name: "warn json", level: "WARN", format: "json", wantLevel: slog.LevelWarn},
		{name: "unknown level", level: "loud", wantErr: "OCTOMATON_LOG_LEVEL"},
		{name: "unknown format", format: "xml", wantErr: "OCTOMATON_LOG_FORMAT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, value := range map[string]string{"OCTOMATON_LOG_LEVEL": tt.level, "OCTOMATON_LOG_FORMAT": tt.format} {
				if value != "" {
					t.Setenv(name, value)
				}
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

func TestJSONLogsUseCloudLoggingFields(t *testing.T) {
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
			_, out := start(t, Config{LogLevel: slog.LevelDebug, LogFormat: "json"})
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

func TestLogLevelAndTextFormat(t *testing.T) {
	_, out := start(t, Config{LogLevel: slog.LevelWarn, LogFormat: "text"})
	slog.Info("Dropped")
	slog.Warn("Kept", "attempt", 2)
	if got := out.String(); strings.Contains(got, "Dropped") || !strings.Contains(got, "level=WARN msg=Kept attempt=2") {
		t.Fatalf("logs = %q", got)
	}
}

func TestMetricsHandlerServesPrometheusNames(t *testing.T) {
	tel, _ := start(t, Config{LogFormat: "json"})
	m, err := metrics.New(otel.Meter("octomaton.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.RunCreated(ctx, metrics.RunCreated)
	m.WebhookRejected(ctx, "push", "signature")
	m.ReconcileDone(ctx, "ok", 250*time.Millisecond)
	m.SetQueueDepth(ctx, 3)
	m.SetLeader(ctx, true)

	rec := httptest.NewRecorder()
	tel.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`octomaton_runs_created_total{result="created"} 1`,
		`octomaton_webhooks_rejected_total{event="push",reason="signature"} 1`,
		`octomaton_reconcile_duration_seconds_count{result="ok"} 1`,
		`octomaton_reconcile_duration_seconds_bucket{result="ok",le="0.1"} 0`,
		`octomaton_reconcile_duration_seconds_bucket{result="ok",le="0.25"} 1`,
		`octomaton_webhook_queue_depth 3`,
		`octomaton_leader 1`,
		`go_goroutines`,
		`process_cpu_seconds_total`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

func TestExportersFollowTheOTELVariables(t *testing.T) {
	for _, exporter := range []string{"", "none", "console"} {
		t.Run("exporter="+exporter, func(t *testing.T) {
			t.Setenv("OTEL_TRACES_EXPORTER", exporter)
			t.Setenv("OTEL_LOGS_EXPORTER", exporter)
			_, out := start(t, Config{LogLevel: slog.LevelInfo, LogFormat: "json"})
			slog.Info("Hello")
			if !strings.Contains(out.String(), `"message":"Hello"`) {
				t.Fatalf("stdout logs = %q", out.String())
			}
		})
	}
}

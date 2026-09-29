package metrics_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"octomaton.dev/internal/system/metrics"
	"octomaton.dev/internal/system/metrics/metricstest"
)

func TestCounters(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		record func(m *metrics.Metrics)
		attrs  []attribute.KeyValue
	}{
		{name: "octomaton.webhooks.received", record: func(m *metrics.Metrics) { m.WebhookReceived(ctx, "push") },
			attrs: []attribute.KeyValue{attribute.String("event", "push")}},
		{name: "octomaton.webhooks.rejected", record: func(m *metrics.Metrics) { m.WebhookRejected(ctx, "push", "queue_full") },
			attrs: []attribute.KeyValue{attribute.String("event", "push"), attribute.String("reason", "queue_full")}},
		{name: "octomaton.runs.created", record: func(m *metrics.Metrics) { m.RunCreated(ctx, metrics.RunSkipped) },
			attrs: []attribute.KeyValue{attribute.String("result", metrics.RunSkipped)}},
		{name: "octomaton.github.checkrun.errors", record: func(m *metrics.Metrics) { m.CheckRunError(ctx, "update") },
			attrs: []attribute.KeyValue{attribute.String("operation", "update")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metricstest.New(t)
			tt.record(m.Metrics)
			tt.record(m.Metrics)
			if got := m.Count(t, tt.name, tt.attrs...); got != 2 {
				t.Fatalf("%s%v = %d, want 2", tt.name, tt.attrs, got)
			}
		})
	}
}

func TestGauges(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name, metric string
		record       func(m *metrics.Metrics)
		want         int64
	}{
		{name: "queue depth", metric: "octomaton.webhook.queue.depth",
			record: func(m *metrics.Metrics) { m.SetQueueDepth(ctx, 5); m.SetQueueDepth(ctx, 3) }, want: 3},
		{name: "leader", metric: "octomaton.leader", record: func(m *metrics.Metrics) { m.SetLeader(ctx, true) }, want: 1},
		{name: "former leader", metric: "octomaton.leader",
			record: func(m *metrics.Metrics) { m.SetLeader(ctx, true); m.SetLeader(ctx, false) }, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metricstest.New(t)
			tt.record(m.Metrics)
			if got := m.Gauge(t, tt.metric); got != tt.want {
				t.Fatalf("%s = %d, want %d", tt.metric, got, tt.want)
			}
		})
	}
}

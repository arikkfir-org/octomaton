// Package metrics records Octomaton's metrics through OpenTelemetry. On GKE, the telemetry package
// exports them to Cloud Monitoring.
package metrics

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Results recorded by RunCreated.
const (
	RunCreated        = "created"
	RunSkipped        = "skipped"
	RunActionRequired = "action_required"
	RunFailed         = "failed"
	RunError          = "error"
	RunExisting       = "existing"
)

// durationBuckets are Prometheus's default buckets, in seconds; the SDK's default ones suit
// milliseconds.
var durationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Metrics holds Octomaton's instruments.
type Metrics struct {
	webhooksReceived  metric.Int64Counter
	webhooksRejected  metric.Int64Counter
	runsCreated       metric.Int64Counter
	checkRunErrors    metric.Int64Counter
	reconcileDuration metric.Float64Histogram
	queueDepth        metric.Int64Gauge
	leader            metric.Int64Gauge
}

// New creates the instruments on meter.
func New(meter metric.Meter) (*Metrics, error) {
	var errs []error
	counter := func(name, description string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(description))
		errs = append(errs, err)
		return c
	}
	gauge := func(name, description string) metric.Int64Gauge {
		g, err := meter.Int64Gauge(name, metric.WithDescription(description))
		errs = append(errs, err)
		return g
	}
	m := &Metrics{
		webhooksReceived: counter("octomaton.webhooks.received", "Webhook deliveries received, by event."),
		webhooksRejected: counter("octomaton.webhooks.rejected", "Webhook deliveries rejected, by event and reason."),
		runsCreated: counter("octomaton.runs.created",
			"Pipeline trigger outcomes: created, existing (deduplicated), skipped, action_required, failed (reported on a check run) or error."),
		checkRunErrors: counter("octomaton.github.checkrun.errors", "Failed GitHub check-run API calls, by operation."),
		queueDepth:     gauge("octomaton.webhook.queue.depth", "Webhook jobs waiting for a worker."),
		leader:         gauge("octomaton.leader", "1 while this replica is the elected reporter leader."),
	}
	var err error
	m.reconcileDuration, err = meter.Float64Histogram("octomaton.reconcile.duration", metric.WithUnit("s"),
		metric.WithDescription("Time spent reporting a PipelineRun's state to GitHub, by result."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	return m, errors.Join(append(errs, err)...)
}

// Discard returns metrics that record nothing.
func Discard() *Metrics {
	m, _ := New(noop.NewMeterProvider().Meter(""))
	return m
}

// WebhookReceived counts a delivery of event.
func (m *Metrics) WebhookReceived(ctx context.Context, event string) {
	m.webhooksReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("event", event)))
}

// WebhookRejected counts a delivery of event rejected for reason.
func (m *Metrics) WebhookRejected(ctx context.Context, event, reason string) {
	m.webhooksRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("event", event), attribute.String("reason", reason)))
}

// RunCreated counts a pipeline trigger outcome, one of the Run* results.
func (m *Metrics) RunCreated(ctx context.Context, result string) {
	m.runsCreated.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// CheckRunError counts a failed GitHub check-run API call.
func (m *Metrics) CheckRunError(ctx context.Context, operation string) {
	m.checkRunErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", operation)))
}

// ReconcileDone records how long reporting a PipelineRun's state took.
func (m *Metrics) ReconcileDone(ctx context.Context, result string, took time.Duration) {
	m.reconcileDuration.Record(ctx, took.Seconds(), metric.WithAttributes(attribute.String("result", result)))
}

// SetQueueDepth records the number of webhook jobs waiting for a worker.
func (m *Metrics) SetQueueDepth(ctx context.Context, depth int) {
	m.queueDepth.Record(ctx, int64(depth))
}

// SetLeader records whether this replica is the elected leader.
func (m *Metrics) SetLeader(ctx context.Context, leading bool) {
	var v int64
	if leading {
		v = 1
	}
	m.leader.Record(ctx, v)
}

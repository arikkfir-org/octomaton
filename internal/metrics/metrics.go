// Package metrics defines Octomaton's Prometheus metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Results recorded by RunsCreated.
const (
	RunCreated        = "created"
	RunSkipped        = "skipped"
	RunActionRequired = "action_required"
	RunFailed         = "failed"
	RunError          = "error"
	RunExisting       = "existing"
)

// Metrics holds every metric Octomaton exports, registered on its own registry.
type Metrics struct {
	Registry *prometheus.Registry

	// WebhooksReceived counts webhook deliveries by event.
	WebhooksReceived *prometheus.CounterVec
	// WebhooksRejected counts rejected webhook deliveries by event and reason.
	WebhooksRejected *prometheus.CounterVec
	// RunsCreated counts pipeline trigger outcomes by result.
	RunsCreated *prometheus.CounterVec
	// CheckRunErrors counts failed GitHub check-run API calls by operation.
	CheckRunErrors *prometheus.CounterVec
	// ReconcileDuration observes reporter reconcile latency by result.
	ReconcileDuration *prometheus.HistogramVec
	// QueueDepth is the number of webhook jobs waiting for a worker.
	QueueDepth prometheus.Gauge
	// Leader is 1 while this replica holds the reporter lease.
	Leader prometheus.Gauge
}

// New creates the metrics and a registry holding them plus Go and process collectors.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		WebhooksReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "octomaton_webhooks_received_total",
			Help: "Webhook deliveries received, by event.",
		}, []string{"event"}),
		WebhooksRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "octomaton_webhooks_rejected_total",
			Help: "Webhook deliveries rejected, by event and reason.",
		}, []string{"event", "reason"}),
		RunsCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "octomaton_runs_created_total",
			Help: "Pipeline trigger outcomes: created, existing (deduplicated), skipped, action_required, failed (reported on a check run) or error.",
		}, []string{"result"}),
		CheckRunErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "octomaton_github_checkrun_errors_total",
			Help: "Failed GitHub check-run API calls, by operation.",
		}, []string{"operation"}),
		ReconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "octomaton_reconcile_duration_seconds",
			Help:    "Time spent reporting a PipelineRun's state to GitHub, by result.",
			Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "octomaton_webhook_queue_depth",
			Help: "Webhook jobs waiting for a worker.",
		}),
		Leader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "octomaton_leader",
			Help: "1 while this replica is the elected reporter leader.",
		}),
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.WebhooksReceived, m.WebhooksRejected, m.RunsCreated, m.CheckRunErrors,
		m.ReconcileDuration, m.QueueDepth, m.Leader,
	)
	return m
}

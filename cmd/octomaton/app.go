package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"octomaton.dev/internal/buildinfo"
	"octomaton.dev/internal/config"
	"octomaton.dev/internal/githubapp"
	octohttp "octomaton.dev/internal/http"
	"octomaton.dev/internal/kube"
	"octomaton.dev/internal/leader"
	"octomaton.dev/internal/metrics"
	"octomaton.dev/internal/relay"
	"octomaton.dev/internal/reporter"
	"octomaton.dev/internal/tekton"
	"octomaton.dev/internal/trigger"
	"octomaton.dev/internal/webhook"
)

const (
	// leaseName is the Lease through which the replicas elect their leader.
	leaseName = "octomaton"
	// readinessTTL is how long one check of the Kubernetes API server answers readiness probes.
	readinessTTL = 5 * time.Second
	// webhookJobTimeout bounds the processing of one webhook delivery.
	webhookJobTimeout = 5 * time.Minute
	// drainTimeout bounds the wait for queued deliveries and relays at shutdown.
	drainTimeout = 15 * time.Second
)

// app is Octomaton's server: the HTTP endpoints and the webhook workers on every replica and, on
// the elected leader, the reporter, the scheduler and the maintenance jobs.
type app struct {
	cfg     *config.Config
	kube    *kube.Clients
	github  *githubapp.App
	metrics *metrics.Metrics

	trigger   *trigger.Service
	scheduler *trigger.Scheduler
	reporter  *reporter.Reporter
	pool      *webhook.Pool
	relay     *relay.Relay
	elector   *leader.Elector
	server    *octohttp.Server
}

// newApp connects to Kubernetes and GitHub, then wires the components.
func newApp(cfg *config.Config, metricsHandler http.Handler) (*app, error) {
	a := &app{cfg: cfg}
	if err := a.connect(); err != nil {
		return nil, err
	}
	a.wireTrigger()
	a.wireLeader()
	a.wireServer(metricsHandler)
	return a, nil
}

func (a *app) connect() error {
	var err error
	if a.kube, err = kube.NewClients("octomaton/" + buildinfo.Version()); err != nil {
		return err
	}
	if a.github, err = githubapp.New(a.cfg.GitHub.AppID, a.cfg.GitHub.Key()); err != nil {
		return fmt.Errorf("creating the GitHub App client: %w", err)
	}
	if a.metrics, err = metrics.New(otel.Meter("octomaton.dev")); err != nil {
		return fmt.Errorf("creating the metrics: %w", err)
	}
	return nil
}

// wireTrigger builds the service that turns deliveries into PipelineRuns, and the scheduler and
// reporter working with it.
func (a *app) wireTrigger() {
	runs := &tekton.Client{Dynamic: a.kube.Dynamic, Kube: a.kube.Kube}
	a.trigger = &trigger.Service{
		GitHub:       a.github,
		Runs:         runs,
		Namespaces:   &a.cfg.Namespaces,
		DashboardURL: a.cfg.Tekton.DashboardURL,
		OwnerAllowed: a.cfg.GitHub.OwnerAllowed,
		Logger:       component("trigger"),
		Metrics:      a.metrics,
	}
	a.scheduler = &trigger.Scheduler{Service: a.trigger}
	a.trigger.Schedules = a.scheduler
	a.reporter = &reporter.Reporter{
		Dynamic:      a.kube.Dynamic,
		Runs:         runs,
		GitHub:       a.github,
		DashboardURL: a.cfg.Tekton.DashboardURL,
		Logger:       component("reporter"),
		Metrics:      a.metrics,
		Resume:       a.trigger.Resume,
		ReleaseNext:  a.trigger.ReleaseNext,
	}
}

func (a *app) wireLeader() {
	a.elector = &leader.Elector{
		Client:    a.kube.Kube,
		Namespace: a.cfg.Pod.Namespace,
		Name:      leaseName,
		Pod:       a.cfg.Pod.Name,
		Jobs:      a.lead,
		Metrics:   a.metrics,
		Logger:    component("leader-election"),
	}
}

func (a *app) wireServer(metricsHandler http.Handler) {
	readiness := &octohttp.Readiness{Ping: a.kube.Ping, Leading: a.elector.Leading, Synced: a.reporter.Synced, TTL: readinessTTL}
	a.server = octohttp.NewServer(a.cfg.HTTP.Address, a.wireWebhook(), readiness, metricsHandler)
}

// wireWebhook returns the handler of GitHub's deliveries. It verifies them and queues them for the
// worker pool, and relays them when relay URLs are configured.
func (a *app) wireWebhook() http.Handler {
	a.pool = webhook.NewPool(a.cfg.Webhook.Workers, a.cfg.Webhook.QueueSize, webhookJobTimeout, component("webhook"), a.metrics)
	hook := &webhook.Handler{
		Secret:  []byte(a.cfg.GitHub.WebhookSecret),
		Router:  a.trigger,
		Pool:    a.pool,
		Dedupe:  webhook.NewDedupe(4096, time.Hour),
		Metrics: a.metrics,
		Logger:  component("webhook"),
	}
	if len(a.cfg.Relay.URLs) > 0 {
		a.relay = relay.New(a.cfg.Relay.URLs, 4, 256, component("relay"))
		hook.Relay = a.relay
	}
	return hook
}

// lead runs the leader's jobs until it loses the Lease.
func (a *app) lead(ctx context.Context) {
	var jobs sync.WaitGroup
	jobs.Go(func() { a.scheduler.Run(ctx) })
	jobs.Go(func() { a.trigger.RefreshTokens(ctx) })
	jobs.Go(func() { a.trigger.FreePVCs(ctx, a.cfg.Retention.FreePVCsAfter) })
	if err := a.reporter.Run(ctx); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "Reporter stopped", "error", err)
	}
	jobs.Wait()
}

// run serves until ctx is cancelled or the HTTP server fails, then stops in dependency order: the
// HTTP server (no new deliveries), the webhook workers and the relay (the queued deliveries), and
// the leader election; it returns the exit code.
func (a *app) run(ctx context.Context) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	context.AfterFunc(ctx, func() { slog.Info("Shutting down") })
	var election sync.WaitGroup
	election.Go(func() { a.elector.Run(ctx) })

	code := 0
	if err := a.server.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "HTTP server failed", "error", err)
		code = 1
	}
	cancel()
	a.drain()
	election.Wait()
	slog.Info("Stopped")
	return code
}

func (a *app) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := a.pool.Shutdown(ctx); err != nil {
		slog.WarnContext(ctx, "Webhook queue not fully drained", "error", err)
	}
	if a.relay != nil {
		a.relay.Close(ctx)
	}
}

func component(name string) *slog.Logger {
	return slog.Default().With("component", name)
}

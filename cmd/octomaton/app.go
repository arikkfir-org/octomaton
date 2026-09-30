package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"octomaton.dev/internal/adapters/github"
	octohttp "octomaton.dev/internal/adapters/http"
	"octomaton.dev/internal/adapters/kube"
	"octomaton.dev/internal/adapters/leader"
	"octomaton.dev/internal/adapters/relay"
	"octomaton.dev/internal/adapters/tekton"
	"octomaton.dev/internal/services/reports"
	"octomaton.dev/internal/services/runs"
	"octomaton.dev/internal/services/schedules"
	"octomaton.dev/internal/services/upkeep"
	"octomaton.dev/internal/system/buildinfo"
	"octomaton.dev/internal/system/config"
	"octomaton.dev/internal/system/metrics"
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
// the elected leader, the reports, the schedules and the upkeep.
type app struct {
	cfg     *config.Config
	metrics *metrics.Metrics
	kube    *kube.Clients
	github  *github.App
	runner  *tekton.Runner

	runs      *runs.Service
	reports   *reports.Service
	schedules *schedules.Scheduler
	upkeep    *upkeep.Service

	pool    *octohttp.Pool
	relay   *relay.Relay
	elector *leader.Elector
	server  *octohttp.Server
}

// newApp connects the adapters to Kubernetes and GitHub, then wires the services over them.
func newApp(cfg *config.Config) (*app, error) {
	a := &app{cfg: cfg}
	if err := a.connect(); err != nil {
		return nil, err
	}
	a.wireServices()
	a.wireLeader()
	a.wireServer()
	return a, nil
}

// connect builds the adapters: GitHub is the code host, Tekton the runner.
func (a *app) connect() error {
	namespaces, err := tekton.NewNamespaces(a.cfg.Namespaces.Template, a.cfg.Namespaces.Overrides)
	if err != nil {
		return err
	}
	if a.metrics, err = metrics.New(otel.Meter("octomaton.dev")); err != nil {
		return fmt.Errorf("creating the metrics: %w", err)
	}
	if a.kube, err = kube.NewClients("octomaton/" + buildinfo.Version()); err != nil {
		return err
	}
	a.github, err = github.New(int64(a.cfg.GitHub.AppID), a.cfg.GitHub.Key(),
		github.WithOwners(a.cfg.GitHub.AllowedOwners), github.WithMetrics(a.metrics))
	if err != nil {
		return fmt.Errorf("creating the GitHub App client: %w", err)
	}
	a.runner = &tekton.Runner{
		Dynamic:      a.kube.Dynamic,
		Kube:         a.kube.Kube,
		Namespaces:   namespaces,
		DashboardURL: a.cfg.Tekton.DashboardURL,
		Logger:       component("runner"),
		Metrics:      a.metrics,
	}
	return nil
}

// wireServices builds the services over the adapters: runs turns events into runs, reports mirrors
// them onto the code host, schedules fires cron triggers, and upkeep keeps tokens fresh and frees
// finished runs' resources.
func (a *app) wireServices() {
	a.runs = &runs.Service{
		Host: a.github, Runner: a.runner, OrganizationRepository: a.cfg.Organization.Repository,
		Logger: component("runs"), Metrics: a.metrics,
	}
	a.schedules = &schedules.Scheduler{Host: a.github, Runner: a.runner, Runs: a.runs, Logger: component("schedules")}
	a.runs.Schedules = a.schedules
	a.reports = &reports.Service{
		Host:        a.github,
		Runner:      a.runner,
		Logger:      component("reports"),
		Resume:      a.runs.Resume,
		ReleaseNext: a.runs.ReleaseNext,
	}
	a.upkeep = &upkeep.Service{Host: a.github, Runner: a.runner, Logger: component("upkeep")}
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

func (a *app) wireServer() {
	readiness := &octohttp.Readiness{Ping: a.kube.Ping, Leading: a.elector.Leading, Synced: a.runner.Synced, TTL: readinessTTL}
	a.server = octohttp.NewServer(a.cfg.HTTP.Address, a.wireWebhook(), readiness)
}

// wireWebhook returns the handler of GitHub's deliveries. It verifies and decodes them and queues
// their events for the runs service on the worker pool, and relays them when relay URLs are
// configured.
func (a *app) wireWebhook() http.Handler {
	a.pool = octohttp.NewPool(a.cfg.Webhook.Workers, a.cfg.Webhook.QueueSize, webhookJobTimeout, component("webhook"), a.metrics)
	hook := &octohttp.Handler{
		Secret:  []byte(a.cfg.GitHub.WebhookSecret),
		Decoder: a.github,
		Events:  a.runs,
		Pool:    a.pool,
		Dedupe:  octohttp.NewDedupe(4096, time.Hour),
		Metrics: a.metrics,
		Logger:  component("webhook"),
	}
	if len(a.cfg.Relay.URLs) > 0 {
		a.relay = relay.New(a.cfg.Relay.URLs, 4, 256, component("relay"))
		hook.Relay = a.relay
	}
	return hook
}

// lead runs the leader's jobs until it loses the Lease: reporting the runs the runner watches,
// firing schedules, and upkeep.
func (a *app) lead(ctx context.Context) {
	var jobs sync.WaitGroup
	jobs.Go(func() { a.schedules.Run(ctx) })
	jobs.Go(func() { a.upkeep.RefreshTokens(ctx) })
	jobs.Go(func() { a.upkeep.FreeResources(ctx, a.cfg.Retention.FreePVCsAfter) })
	if err := a.runner.Watch(ctx, a.reports); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "Watching runs stopped", "error", err)
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

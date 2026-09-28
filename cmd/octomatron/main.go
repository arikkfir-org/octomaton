// Command octomatron receives GitHub App webhooks, creates the Tekton
// PipelineRuns that repositories declare in .octomatron.yaml, and reports their
// progress back to GitHub as check runs.
//
// Usage:
//
//	octomatron [serve] [--config FILE] [--listen ADDR]
//	octomatron lint [--render] PATH...
//	octomatron version
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/arikkfir-org/octomatron/internal/config"
	"github.com/arikkfir-org/octomatron/internal/githubapp"
	"github.com/arikkfir-org/octomatron/internal/lint"
	"github.com/arikkfir-org/octomatron/internal/metrics"
	"github.com/arikkfir-org/octomatron/internal/relay"
	"github.com/arikkfir-org/octomatron/internal/reporter"
	"github.com/arikkfir-org/octomatron/internal/tekton"
	"github.com/arikkfir-org/octomatron/internal/trigger"
	"github.com/arikkfir-org/octomatron/internal/webhook"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	leaseName           = "octomatron"
	serviceAccountNSDir = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return serve(args[1:])
		case "lint":
			return runLint(args[1:], stdout, stderr)
		case "version":
			fmt.Fprintln(stdout, version)
			return 0
		case "help", "-h", "--help":
			usage(stdout)
			return 0
		}
	}
	return serve(args)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  octomatron [serve] [--config FILE] [--listen ADDR] [--workers N] [--queue-size N]
  octomatron lint [--render] PATH...   validate .octomatron.yaml (PATH is the file or its directory)
  octomatron version
`)
}

func runLint(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	render := fs.Bool("render", false, "print every PipelineRun rendered with placeholder values, as a YAML stream")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: octomatron lint [--render] PATH...\n\nPATH is a .octomatron.yaml file or the directory holding it.\n")
	}
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fs.Usage()
		return lint.ExitUsage
	}
	return lint.Run(fs.Args(), *render, stdout, stderr)
}

// webhookPath is where GitHub delivers the App's webhooks.
const webhookPath = "/github/hooks"

// routes maps the HTTP endpoints: the GitHub webhook, the probes and the metrics.
func routes(hook, ready, metrics http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(webhookPath, hook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })
	mux.Handle("/readyz", ready)
	mux.Handle("/metrics", metrics)
	return mux
}

func serve(args []string) int {
	defaultConfig := config.DefaultPath
	if v := os.Getenv("OCTOMATRON_CONFIG"); v != "" {
		defaultConfig = v
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfig, "path to the server configuration file (env OCTOMATRON_CONFIG)")
	listen := fs.String("listen", ":8080", "address to serve "+webhookPath+", /healthz, /readyz and /metrics on")
	workers := fs.Int("workers", 8, "number of webhook worker goroutines")
	queueSize := fs.Int("queue-size", 256, "number of accepted webhook deliveries that may wait for a worker")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := newLogger(os.Getenv("OCTOMATRON_LOG_LEVEL"))
	slog.SetDefault(logger)
	klog.SetSlogLogger(logger.With("component", "client-go"))
	logger.Info("Starting Octomatron", "version", version, "config", *configPath)

	cfg, creds, err := config.Load(*configPath)
	if err != nil {
		logger.Error("Invalid configuration", "error", err)
		return 1
	}
	restConfig, err := kubeConfig()
	if err != nil {
		logger.Error("Cannot configure the Kubernetes client", "error", err)
		return 1
	}
	restConfig.UserAgent = "octomatron/" + version
	restConfig.QPS, restConfig.Burst = 20, 50
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logger.Error("Cannot create the Kubernetes client", "error", err)
		return 1
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error("Cannot create the dynamic Kubernetes client", "error", err)
		return 1
	}
	app, err := githubapp.New(creds.AppID, creds.PrivateKey)
	if err != nil {
		logger.Error("Cannot create the GitHub App client", "error", err)
		return 1
	}

	m := metrics.New()
	runs := &tekton.Client{Dynamic: dyn, Kube: kube}
	svc := &trigger.Service{
		GitHub:       app,
		Runs:         runs,
		Namespaces:   &cfg.Namespaces,
		DashboardURL: cfg.Tekton.DashboardURL,
		OwnerAllowed: cfg.GitHub.OwnerAllowed,
		Logger:       logger.With("component", "trigger"),
		Metrics:      m,
	}
	scheduler := &trigger.Scheduler{Service: svc}
	svc.Schedules = scheduler
	pool := webhook.NewPool(*workers, *queueSize, 5*time.Minute, logger.With("component", "webhook"), m)
	hook := &webhook.Handler{
		Secret:  creds.WebhookSecret,
		Router:  svc,
		Pool:    pool,
		Dedupe:  webhook.NewDedupe(4096, time.Hour),
		Metrics: m,
		Logger:  logger.With("component", "webhook"),
	}
	var fwd *relay.Relay
	if len(cfg.Relay.URLs) > 0 {
		fwd = relay.New(cfg.Relay.URLs, 4, 256, logger.With("component", "relay"))
		hook.Relay = fwd
	}
	rep := &reporter.Reporter{
		Dynamic:      dyn,
		Runs:         runs,
		GitHub:       app,
		DashboardURL: cfg.Tekton.DashboardURL,
		Logger:       logger.With("component", "reporter"),
		Metrics:      m,
		Resume:       svc.Resume,
		ReleaseNext:  svc.ReleaseNext,
	}

	var leader atomic.Bool
	ready := &readiness{
		ping: func(ctx context.Context) error {
			return kube.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Error()
		},
		leader:   &leader,
		reporter: rep,
		ttl:      5 * time.Second,
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           routes(hook, ready, promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("Serving HTTP", "address", *listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	leaderJobs := func(ctx context.Context) {
		var jobs sync.WaitGroup
		jobs.Go(func() { scheduler.Run(ctx) })
		jobs.Go(func() { svc.RefreshTokens(ctx) })
		jobs.Go(func() { svc.FreePVCs(ctx, cfg.Retention.FreePVCsAfter.Duration) })
		if err := rep.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("Reporter stopped", "error", err)
		}
		jobs.Wait()
	}
	var electionDone sync.WaitGroup
	electionDone.Go(func() {
		runLeaderElection(ctx, kube, podNamespace(), identity(), leaderJobs, &leader, m, logger.With("component", "leader-election"))
	})

	exitCode := 0
	select {
	case <-ctx.Done():
		logger.Info("Shutting down")
	case err := <-serverErr:
		if err != nil {
			logger.Error("HTTP server failed", "error", err)
			exitCode = 1
		}
		stop()
	}
	ready.shuttingDown.Store(true)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP server shutdown incomplete", "error", err)
	}
	cancel()
	drainCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := pool.Shutdown(drainCtx); err != nil {
		logger.Warn("Webhook queue not fully drained", "error", err)
	}
	if fwd != nil {
		fwd.Close(drainCtx)
	}
	cancel()
	electionDone.Wait()
	logger.Info("Stopped")
	return exitCode
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// kubeConfig uses the in-cluster configuration, falling back to KUBECONFIG (or
// ~/.kube/config) for local runs.
func kubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
}

// podNamespace is where the leader election Lease lives: POD_NAMESPACE, else the
// service account's namespace, else "octomatron".
func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile(serviceAccountNSDir); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return "octomatron"
}

func identity() string {
	name := os.Getenv("POD_NAME")
	if name == "" {
		name, _ = os.Hostname()
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return name + "_" + hex.EncodeToString(suffix)
}

// runLeaderElection keeps this replica in the election for the Lease until ctx
// is cancelled; while leading, it runs the leader jobs.
func runLeaderElection(ctx context.Context, kube kubernetes.Interface, namespace, id string, jobs func(context.Context), leader *atomic.Bool, m *metrics.Metrics, logger *slog.Logger) {
	for ctx.Err() == nil {
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock: &resourcelock.LeaseLock{
				LeaseMeta:  metav1.ObjectMeta{Name: leaseName, Namespace: namespace},
				Client:     kube.CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{Identity: id},
			},
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			ReleaseOnCancel: true,
			Name:            leaseName,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					logger.Info("Became leader; starting the reporter, scheduler and maintenance jobs", "identity", id)
					leader.Store(true)
					m.Leader.Set(1)
					jobs(ctx)
				},
				OnStoppedLeading: func() {
					m.Leader.Set(0)
					if leader.Swap(false) {
						logger.Info("No longer leader", "identity", id)
					}
				},
				OnNewLeader: func(current string) {
					if current != id {
						logger.Info("Observed leader", "leader", current)
					}
				},
			},
		})
		if err != nil {
			logger.Error("Cannot configure leader election", "error", err)
			return
		}
		elector.Run(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// readiness reports ready when the Kubernetes API is reachable and, on the
// leader, the PipelineRun informer has synced.
type readiness struct {
	ping         func(ctx context.Context) error
	leader       *atomic.Bool
	reporter     *reporter.Reporter
	ttl          time.Duration
	shuttingDown atomic.Bool

	mu        sync.Mutex
	checkedAt time.Time
	lastErr   error
}

func (rd *readiness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := rd.check(r.Context()); err != nil {
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
}

func (rd *readiness) check(ctx context.Context) error {
	if rd.shuttingDown.Load() {
		return errors.New("shutting down")
	}
	if err := rd.apiReachable(ctx); err != nil {
		return fmt.Errorf("kubernetes API unreachable: %w", err)
	}
	if rd.leader.Load() && !rd.reporter.Synced() {
		return errors.New("PipelineRun informer not synced yet")
	}
	return nil
}

func (rd *readiness) apiReachable(ctx context.Context) error {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if !rd.checkedAt.IsZero() && time.Since(rd.checkedAt) < rd.ttl {
		return rd.lastErr
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rd.lastErr = rd.ping(ctx)
	rd.checkedAt = time.Now()
	return rd.lastErr
}

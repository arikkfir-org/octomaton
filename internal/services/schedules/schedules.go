// Package schedules fires the schedule triggers of every repository the App serves: each slot of a
// pipeline's cron schedule starts one run at the head of the repository's default branch. Only the
// leader fires schedules.
package schedules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
	"octomaton.dev/internal/services/runs"
)

// Defaults.
const (
	DefaultTick    = 30 * time.Second
	DefaultRefresh = 10 * time.Minute
	DefaultCatchUp = 10 * time.Minute
)

// Scheduler fires schedules. It reads them from each repository's .octomaton.yaml on its default
// branch, every RefreshEvery and when this replica sees a push to the default branch; each slot
// fires one run of the pipeline, at most CatchUp late.
type Scheduler struct {
	Host   ci.CodeHost
	Runner ci.Runner
	Runs   *runs.Service
	Logger *slog.Logger
	// Tick is how often due slots are looked for (default 30s).
	Tick time.Duration
	// RefreshEvery is how often every repository's schedules are read again (default 10m).
	RefreshEvery time.Duration
	// CatchUp is how late a slot may still fire (default 10m).
	CatchUp time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	running bool
	notify  chan notification
	repos   map[string]*scheduledRepo

	fired map[string]time.Time // used by FireDue's caller only
}

var _ runs.ScheduleNotifier = (*Scheduler)(nil)

type notification struct {
	installationID int64
	repo           ci.Repository
}

type scheduledRepo struct {
	installationID int64
	repo           ci.Repository
	pipelines      []scheduledPipeline
}

type scheduledPipeline struct {
	name      string
	schedules []pipelines.ScheduleTrigger
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Notify asks for a repository's schedules to be read again; it does nothing unless the scheduler
// runs in this replica.
func (s *Scheduler) Notify(installationID int64, repo ci.Repository) {
	s.mu.Lock()
	running, ch := s.running, s.notify
	s.mu.Unlock()
	if !running {
		return
	}
	select {
	case ch <- notification{installationID: installationID, repo: repo}:
	default:
	}
}

// Run fires due schedules until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.running, s.notify = true, make(chan notification, 64)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	tick := time.NewTicker(orDefault(s.Tick, DefaultTick))
	defer tick.Stop()
	refresh := time.NewTicker(orDefault(s.RefreshEvery, DefaultRefresh))
	defer refresh.Stop()
	s.RefreshAll(ctx)
	for {
		s.FireDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-refresh.C:
			s.RefreshAll(ctx)
		case n := <-s.notify:
			s.refreshRepo(ctx, n.installationID, n.repo)
		}
	}
}

// RefreshAll reads the schedules of every repository the App serves again.
func (s *Scheduler) RefreshAll(ctx context.Context) {
	accounts, err := s.Host.Accounts(ctx)
	if err != nil {
		s.Logger.ErrorContext(ctx, "Could not list the App's installations; keeping the known schedules", "error", err)
		return
	}
	s.mu.Lock()
	previous := s.repos
	s.mu.Unlock()
	repos := map[string]*scheduledRepo{}
	for _, a := range accounts {
		list, err := s.Host.Installation(a.InstallationID).Repositories(ctx)
		if err != nil {
			s.Logger.ErrorContext(ctx, "Could not list the installation's repositories; keeping their known schedules", "installation", a.InstallationID, "error", err)
			for k, v := range previous {
				if v.installationID == a.InstallationID {
					repos[k] = v
				}
			}
			continue
		}
		for _, r := range list {
			if r.DefaultBranch == "" {
				continue
			}
			if entry := s.load(ctx, a.InstallationID, r); entry != nil {
				repos[strings.ToLower(r.FullName)] = entry
			}
		}
	}
	s.mu.Lock()
	s.repos = repos
	s.mu.Unlock()
	s.Logger.DebugContext(ctx, "Refreshed schedules", "repositories", len(repos))
}

func (s *Scheduler) refreshRepo(ctx context.Context, installationID int64, repo ci.Repository) {
	entry := s.load(ctx, installationID, repo)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repos == nil {
		s.repos = map[string]*scheduledRepo{}
	}
	key := strings.ToLower(repo.FullName)
	if entry == nil {
		delete(s.repos, key)
	} else {
		s.repos[key] = entry
	}
}

// load reads a repository's schedules from its default branch; nil when it has none.
func (s *Scheduler) load(ctx context.Context, installationID int64, repo ci.Repository) *scheduledRepo {
	t := ci.Trigger{Version: ci.TriggerVersion, Event: ci.EventSchedule, InstallationID: installationID, Repository: repo, ConfigRef: repo.DefaultBranch}
	cfg, ok := s.Runs.LoadConfig(ctx, t)
	if !ok {
		return nil
	}
	entry := &scheduledRepo{installationID: installationID, repo: repo}
	for i := range cfg.Pipelines {
		if p := &cfg.Pipelines[i]; len(p.Schedules()) > 0 {
			entry.pipelines = append(entry.pipelines, scheduledPipeline{name: p.Name, schedules: p.Schedules()})
		}
	}
	if len(entry.pipelines) == 0 {
		return nil
	}
	return entry
}

// ScheduledRepositories counts the repositories with schedules.
func (s *Scheduler) ScheduledRepositories() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.repos)
}

// FireDue fires every schedule with a slot due within the catch-up window.
func (s *Scheduler) FireDue(ctx context.Context) {
	if s.fired == nil {
		s.fired = map[string]time.Time{}
	}
	now := s.now().UTC()
	catchUp := orDefault(s.CatchUp, DefaultCatchUp)
	s.mu.Lock()
	repos := make([]*scheduledRepo, 0, len(s.repos))
	for _, r := range s.repos {
		repos = append(repos, r)
	}
	s.mu.Unlock()
	for _, entry := range repos {
		for _, p := range entry.pipelines {
			for _, sched := range p.schedules {
				slot := sched.Last(now.Add(-catchUp), now)
				if slot.IsZero() {
					continue
				}
				key := entry.repo.FullName + "/" + p.name + "/" + sched.Cron
				if last, ok := s.fired[key]; ok && !slot.After(last) {
					continue
				}
				if err := s.fire(ctx, entry, p.name, sched, slot); err != nil {
					s.Logger.ErrorContext(ctx, "Schedule not fired; the next tick retries", "repository", entry.repo.FullName, "pipeline", p.name,
						"cron", sched.Cron, "slot", slot.Format(time.RFC3339), "error", err)
					continue
				}
				s.fired[key] = slot
			}
		}
	}
}

// fire starts one run of a pipeline for a schedule slot, at the head of the repository's default
// branch, unless a run for the slot exists already.
func (s *Scheduler) fire(ctx context.Context, entry *scheduledRepo, pipeline string, sched pipelines.ScheduleTrigger, slot time.Time) error {
	repo := entry.repo
	existing, err := s.Runner.List(ctx, ci.RunQuery{Repository: &repo, Pipeline: pipeline, Slot: slot})
	if err == nil && len(existing) > 0 {
		return nil
	}
	head, err := s.Host.Installation(entry.installationID).BranchHead(ctx, repo, repo.DefaultBranch)
	if err != nil {
		return fmt.Errorf("reading the head of %s: %w", repo.DefaultBranch, err)
	}
	t := ci.Trigger{
		Version:        ci.TriggerVersion,
		Event:          ci.EventSchedule,
		DeliveryID:     "schedule-" + strconv.FormatInt(slot.Unix(), 10),
		InstallationID: entry.installationID,
		Repository:     repo,
		Revision:       head,
		Ref:            "refs/heads/" + repo.DefaultBranch,
		Branch:         repo.DefaultBranch,
		Pipeline:       pipeline,
		Schedule:       &ci.Schedule{Cron: sched.Cron, Slot: slot.UTC().Format(time.RFC3339)},
	}
	cfg, ok := s.Runs.LoadConfig(ctx, t)
	if !ok {
		return nil
	}
	p := cfg.Pipeline(pipeline)
	if p == nil || !hasCron(p, sched.Cron) {
		return nil // the schedule was removed since it was read
	}
	s.Logger.InfoContext(ctx, "Firing schedule", "repository", repo.FullName, "pipeline", pipeline, "cron", sched.Cron, "slot", t.Schedule.Slot)
	err = s.Runs.Start(ctx, t, p)
	var refusal *runs.Refusal
	if errors.As(err, &refusal) {
		return nil // reported
	}
	return err
}

func hasCron(p *pipelines.Pipeline, cron string) bool {
	for _, s := range p.Schedules() {
		if s.Cron == cron {
			return true
		}
	}
	return false
}

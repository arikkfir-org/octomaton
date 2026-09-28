package trigger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arikkfir-org/switchboard/internal/checkrun"
	"github.com/arikkfir-org/switchboard/internal/repoconfig"
	"github.com/arikkfir-org/switchboard/internal/tekton"
	"k8s.io/apimachinery/pkg/labels"
)

// Scheduler defaults.
const (
	DefaultScheduleTick    = 30 * time.Second
	DefaultScheduleRefresh = 10 * time.Minute
	DefaultScheduleCatchUp = 10 * time.Minute
)

// Scheduler fires the schedule triggers of every repository the App is
// installed on. Schedules are read from each repository's .switchboard.yaml on
// its default branch, every RefreshEvery and when a push to the default branch
// is seen by this replica; each slot fires one run of the pipeline at the head
// of the default branch, up to CatchUp late. It runs on the leader only.
type Scheduler struct {
	Service      *Service
	Tick         time.Duration
	RefreshEvery time.Duration
	CatchUp      time.Duration

	mu      sync.Mutex
	running bool
	notify  chan scheduleNotification
	repos   map[string]*scheduledRepo

	fired map[string]time.Time // used by the Run goroutine only
}

type scheduleNotification struct {
	installationID int64
	repo           checkrun.Repository
}

type scheduledRepo struct {
	installationID int64
	repo           checkrun.Repository
	pipelines      []scheduledPipeline
}

type scheduledPipeline struct {
	name      string
	schedules []repoconfig.ScheduleTrigger
}

var _ ScheduleNotifier = (*Scheduler)(nil)

func durationOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// Notify asks the scheduler to re-read a repository's schedules; it does nothing
// unless the scheduler is running in this replica.
func (sc *Scheduler) Notify(installationID int64, repo checkrun.Repository) {
	sc.mu.Lock()
	running, ch := sc.running, sc.notify
	sc.mu.Unlock()
	if !running {
		return
	}
	select {
	case ch <- scheduleNotification{installationID: installationID, repo: repo}:
	default:
	}
}

// Run fires due schedules until ctx ends.
func (sc *Scheduler) Run(ctx context.Context) {
	sc.mu.Lock()
	sc.running, sc.notify = true, make(chan scheduleNotification, 64)
	sc.mu.Unlock()
	defer func() {
		sc.mu.Lock()
		sc.running = false
		sc.mu.Unlock()
	}()
	if sc.fired == nil {
		sc.fired = map[string]time.Time{}
	}
	tick := time.NewTicker(durationOr(sc.Tick, DefaultScheduleTick))
	defer tick.Stop()
	refresh := time.NewTicker(durationOr(sc.RefreshEvery, DefaultScheduleRefresh))
	defer refresh.Stop()
	sc.RefreshAll(ctx)
	for {
		sc.FireDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-refresh.C:
			sc.RefreshAll(ctx)
		case n := <-sc.notify:
			sc.refreshRepo(ctx, n.installationID, n.repo)
		}
	}
}

// RefreshAll re-reads the schedules of every repository of every allowed installation.
func (sc *Scheduler) RefreshAll(ctx context.Context) {
	s := sc.Service
	installations, err := s.GitHub.Installations(ctx)
	if err != nil {
		s.Logger.Error("Could not list installations; keeping the known schedules", "error", err)
		return
	}
	sc.mu.Lock()
	previous := sc.repos
	sc.mu.Unlock()
	repos := map[string]*scheduledRepo{}
	for _, inst := range installations {
		if !s.ownerAllowed(inst.Account) {
			continue
		}
		list, err := s.GitHub.Installation(inst.ID).Repositories(ctx)
		if err != nil {
			s.Logger.Error("Could not list installation repositories; keeping their known schedules", "installation", inst.ID, "error", err)
			for k, v := range previous {
				if v.installationID == inst.ID {
					repos[k] = v
				}
			}
			continue
		}
		for _, r := range list {
			if r.Archived || !s.ownerAllowed(r.Owner) || r.DefaultBranch == "" {
				continue
			}
			if entry := sc.load(ctx, inst.ID, fromRepository(r)); entry != nil {
				repos[strings.ToLower(r.FullName)] = entry
			}
		}
	}
	sc.mu.Lock()
	sc.repos = repos
	sc.mu.Unlock()
	s.Logger.Debug("Refreshed schedules", "repositories", len(repos))
}

func (sc *Scheduler) refreshRepo(ctx context.Context, installationID int64, repo checkrun.Repository) {
	entry := sc.load(ctx, installationID, repo)
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.repos == nil {
		sc.repos = map[string]*scheduledRepo{}
	}
	key := strings.ToLower(repo.FullName)
	if entry == nil {
		delete(sc.repos, key)
	} else {
		sc.repos[key] = entry
	}
}

// load reads a repository's schedules from its default branch.
func (sc *Scheduler) load(ctx context.Context, installationID int64, repo checkrun.Repository) *scheduledRepo {
	s := sc.Service
	c := checkrun.Context{Version: checkrun.ContextVersion, Event: checkrun.EventSchedule, InstallationID: installationID, Repository: repo, ConfigRef: repo.DefaultBranch}
	cfg, ok := s.loadConfig(ctx, s.GitHub.Installation(installationID), c, false)
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

// FireDue fires every schedule with a slot due within the catch-up window.
func (sc *Scheduler) FireDue(ctx context.Context) {
	if sc.fired == nil {
		sc.fired = map[string]time.Time{}
	}
	s := sc.Service
	now := s.now().UTC()
	catchUp := durationOr(sc.CatchUp, DefaultScheduleCatchUp)
	sc.mu.Lock()
	repos := make([]*scheduledRepo, 0, len(sc.repos))
	for _, r := range sc.repos {
		repos = append(repos, r)
	}
	sc.mu.Unlock()
	for _, entry := range repos {
		for _, p := range entry.pipelines {
			for _, sched := range p.schedules {
				slot := sched.Last(now.Add(-catchUp), now)
				if slot.IsZero() {
					continue
				}
				key := entry.repo.FullName + "/" + p.name + "/" + sched.Cron
				if last, ok := sc.fired[key]; ok && !slot.After(last) {
					continue
				}
				if err := sc.fire(ctx, entry, p.name, sched, slot); err != nil {
					s.Logger.Error("Schedule not fired; the next tick retries", "repository", entry.repo.FullName, "pipeline", p.name,
						"cron", sched.Cron, "slot", slot.Format(time.RFC3339), "error", err)
					continue
				}
				sc.fired[key] = slot
			}
		}
	}
}

// fire starts one run of a pipeline for a schedule slot, at the head of the
// repository's default branch, unless a run for the slot exists already.
func (sc *Scheduler) fire(ctx context.Context, entry *scheduledRepo, pipeline string, sched repoconfig.ScheduleTrigger, slot time.Time) error {
	s := sc.Service
	gh := s.GitHub.Installation(entry.installationID)
	repo := entry.repo
	ns, err := s.Namespaces.Resolve(checkrun.Context{Repository: repo}.TemplateRepository())
	if err != nil {
		return err
	}
	slotLabel := strconv.FormatInt(slot.Unix(), 10)
	existing, err := s.Runs.List(ctx, ns, labels.SelectorFromSet(map[string]string{
		tekton.LabelManagedBy:    tekton.ManagedByValue,
		tekton.LabelRepositoryID: strconv.FormatInt(repo.ID, 10),
		tekton.LabelPipeline:     pipeline,
		tekton.LabelSlot:         slotLabel,
	}).String())
	if err == nil && len(existing) > 0 {
		return nil
	}
	head, err := gh.BranchHead(ctx, repo.Owner, repo.Name, repo.DefaultBranch)
	if err != nil {
		return fmt.Errorf("reading the head of %s: %w", repo.DefaultBranch, err)
	}
	c := checkrun.Context{
		Version:        checkrun.ContextVersion,
		Event:          checkrun.EventSchedule,
		DeliveryID:     "schedule-" + slotLabel,
		InstallationID: entry.installationID,
		Repository:     repo,
		Revision:       head,
		Ref:            "refs/heads/" + repo.DefaultBranch,
		Branch:         repo.DefaultBranch,
		Pipeline:       pipeline,
		Schedule:       &checkrun.Schedule{Cron: sched.Cron, Slot: slot.UTC().Format(time.RFC3339)},
	}
	cfg, ok := s.loadConfig(ctx, gh, c, false)
	if !ok {
		return nil
	}
	p := cfg.Pipeline(pipeline)
	if p == nil || !hasCron(p, sched.Cron) {
		return nil // the schedule was removed since it was read
	}
	s.logFor(c).Info("Firing schedule", "cron", sched.Cron, "slot", c.Schedule.Slot)
	_, _, err = s.start(ctx, gh, c, p, startOptions{Dedupe: map[string]string{tekton.LabelSlot: slotLabel}})
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return nil // reported on a check run
	}
	return err
}

func hasCron(p *repoconfig.Pipeline, cron string) bool {
	for _, s := range p.Schedules() {
		if s.Cron == cron {
			return true
		}
	}
	return false
}

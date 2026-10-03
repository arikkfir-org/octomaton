package runs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
	"octomaton.dev/internal/services/reports"
	"octomaton.dev/internal/system/metrics"
)

// maxNameTries bounds the retries when other deliveries take the attempts a start tries.
const maxNameTries = 10

// errFromFork is returned for a trigger of a pull request from a fork: Octomaton never runs one.
var errFromFork = errors.New("pull request from a fork")

// Start starts a run of pipeline p for t, unless it runs already. A refused run is reported, and
// returned as a *Refusal.
func (s *Service) Start(ctx context.Context, t ci.Trigger, p *pipelines.Pipeline) error {
	_, err := s.start(ctx, s.Host.Installation(t.InstallationID), t, p, false)
	return err
}

// prepare reads the pipeline's definition and renders its params and concurrency group, and has the
// runner check the run, before anything exists, so that problems produce a single failed report.
func (s *Service) prepare(ctx context.Context, gh ci.Installation, t ci.Trigger, p *pipelines.Pipeline) (ci.RunSpec, *ci.Refusal) {
	refusal := func(format string, args ...any) *ci.Refusal {
		return &ci.Refusal{Title: "Could not start the pipeline", Reason: fmt.Sprintf(format, args...)}
	}
	repo, ref, where := definitionAt(t, p.PipelineRun)
	data, err := gh.ReadFile(ctx, repo, p.PipelineRun.Path, ref)
	if errors.Is(err, ci.ErrNotFound) {
		return ci.RunSpec{}, refusal("The pipelineRun file `%s` does not exist at %s.", p.PipelineRun, where)
	}
	if err != nil {
		return ci.RunSpec{}, refusal("Could not read the pipelineRun file `%s`: %v", p.PipelineRun, err)
	}
	tc := pipelines.ContextOf(t)
	params, err := p.RenderParams(tc)
	if err != nil {
		return ci.RunSpec{}, refusal("Could not render the params of pipeline `%s`: %v", p.Name, err)
	}
	conc, err := p.ConcurrencyFor(tc)
	if err != nil {
		return ci.RunSpec{}, refusal("Could not render the concurrency group of pipeline `%s`: %v", p.Name, err)
	}
	spec := ci.RunSpec{
		Trigger: t, Definition: data, Path: p.PipelineRun.String(), Params: params, Timeout: p.TimeoutDuration(),
		Token: p.Token(), Secrets: p.Secrets, TaskReports: p.TaskChecks, Concurrency: conc,
	}
	if err := s.Runner.Check(ctx, spec); err != nil {
		var r *ci.Refusal
		if errors.As(err, &r) {
			return ci.RunSpec{}, r
		}
		return ci.RunSpec{}, refusal("%v", err)
	}
	return spec, nil
}

// start creates the run of pipeline p for t, or finds the one there: a redelivery (or another event
// for the same commit and head) finds its run; a re-run creates the next attempt. The run is created
// held, then its report, task reports and token are made, and it is let go per its concurrency
// policy. Any failure after the run exists cancels it and fails its report.
func (s *Service) start(ctx context.Context, gh ci.Installation, t ci.Trigger, p *pipelines.Pipeline, rerun bool) (ci.Run, error) {
	t.DisplayName = p.DisplayName
	log := s.logFor(t)
	if t.FromFork() {
		// The callers ignore forks already; this keeps any other path from running a fork's code.
		return ci.Run{}, errFromFork
	}
	refuse := func(r *ci.Refusal) (ci.Run, error) {
		log.WarnContext(ctx, "Pipeline refused", "title", r.Title, "reason", r.Reason)
		s.Metrics.RunCreated(ctx, metrics.RunFailed)
		if t.Comment == nil {
			s.openCompleted(ctx, gh, t, t.ReportName(), ci.Failure, r.Title, r.Reason)
		}
		return ci.Run{}, &Refusal{Pipeline: p.Name, Reason: r.Reason}
	}
	spec, refusal := s.prepare(ctx, gh, t, p)
	if refusal != nil {
		return refuse(refusal)
	}
	runs, err := s.Runner.List(ctx, ci.RunQuery{Repository: &t.Repository, Pipeline: p.Name, Revision: t.Revision})
	if err != nil {
		s.Metrics.RunCreated(ctx, metrics.RunError)
		return ci.Run{}, err
	}
	same := sameRun(t)
	if !rerun {
		if there, ok := current(runs, same); ok && !there.CancelRequested {
			log.InfoContext(ctx, "The run exists already", "run", there.ID.String())
			s.Metrics.RunCreated(ctx, metrics.RunExisting)
			return there, nil
		}
	}
	attempt := nextAttempt(runs)
	var run ci.Run
	for tries := 0; ; tries++ {
		run, err = s.Runner.Create(ctx, spec, attempt)
		if errors.Is(err, ci.ErrExists) && tries < maxNameTries {
			// Another delivery took this attempt meanwhile: it is this run when it is the same one,
			// otherwise this is the next attempt.
			if !rerun && run.ID.Name != "" && same(run) && !run.CancelRequested {
				s.Metrics.RunCreated(ctx, metrics.RunExisting)
				return run, nil
			}
			attempt++
			continue
		}
		break
	}
	if err != nil {
		var r *ci.Refusal
		if !errors.As(err, &r) {
			r = &ci.Refusal{Title: "Could not start the pipeline", Reason: err.Error()}
		}
		return refuse(r)
	}

	reportID, err := s.openReport(ctx, gh, run, 0)
	if err == nil && run.TaskReports {
		err = s.openTaskReports(ctx, gh, run, false)
	}
	if err == nil && run.Token != nil {
		err = s.mintToken(ctx, run)
	}
	if err == nil {
		err = s.release(ctx, run)
	}
	if err != nil {
		s.Metrics.RunCreated(ctx, metrics.RunError)
		s.abort(ctx, gh, run, reportID, err)
		return ci.Run{}, err
	}
	s.Metrics.RunCreated(ctx, metrics.RunCreated)
	log.InfoContext(ctx, "Started pipeline", "run", run.ID.String(), "reportID", reportID, "attempt", run.Attempt,
		"concurrencyGroup", spec.Concurrency.Group, "policy", spec.Concurrency.Policy)
	return run, nil
}

// sameRun reports whether a run is the one a start for t would create: the run of the same comment
// command, review request delivery or schedule slot, or else of the same head (at the same commit, as
// the query selects).
func sameRun(t ci.Trigger) func(ci.Run) bool {
	return func(r ci.Run) bool {
		o := r.Trigger
		switch {
		case t.Comment != nil:
			return o.Comment != nil && o.Comment.ID == t.Comment.ID
		case t.ReviewRequest != nil:
			// Each request is its own run, even at the same commit; only its redeliveries find it. New commits run
			// every pending request from one delivery.
			return o.ReviewRequest != nil && o.DeliveryID == t.DeliveryID && strings.EqualFold(o.ReviewRequest.Reviewer, t.ReviewRequest.Reviewer)
		case t.Schedule != nil:
			return o.Schedule != nil && o.Schedule.Slot == t.Schedule.Slot
		default:
			return o.Comment == nil && o.ReviewRequest == nil && o.Schedule == nil && o.Head() == t.Head()
		}
	}
}

// current returns, of the runs same selects, the one the code host shows.
func current(runs []ci.Run, same func(ci.Run) bool) (ci.Run, bool) {
	var there *ci.Run
	for i := range runs {
		if same(runs[i]) && (there == nil || outranks(runs[i], *there)) {
			there = &runs[i]
		}
	}
	if there == nil {
		return ci.Run{}, false
	}
	return *there, true
}

// nextAttempt follows the highest attempt there (not the count: older attempts may be gone).
func nextAttempt(runs []ci.Run) int {
	attempt := 1
	for _, r := range runs {
		if r.Attempt >= attempt {
			attempt = r.Attempt + 1
		}
	}
	return attempt
}

// outranks reports whether the code host shows a's report over b's (runs of one commit): the newer
// report, then the later attempt.
func outranks(a, b ci.Run) bool {
	if a.ReportID != b.ReportID {
		return a.ReportID > b.ReportID
	}
	return a.Attempt > b.Attempt
}

// queuedSummary links a run and describes its trigger.
func (s *Service) queuedSummary(run ci.Run) string {
	return strings.TrimSuffix(reports.Header(s.Runner.Link(run.ID), run.Trigger), "\n")
}

// openReport opens the run's report (or takes over one the code host already has, id) and records
// it on the run.
func (s *Service) openReport(ctx context.Context, gh ci.Installation, run ci.Run, id ci.ReportID) (ci.ReportID, error) {
	t := run.Trigger
	r := ci.Report{
		Name: t.ReportName(), Revision: t.Revision, Status: ci.StatusQueued, ExternalID: run.ID.String(), URL: s.Runner.Link(run.ID).URL,
		Title: "Queued", Summary: s.queuedSummary(run), Trigger: &t,
	}
	if id == 0 {
		var err error
		if id, err = gh.OpenReport(ctx, t.Repository, r); err != nil {
			return 0, err
		}
	} else if err := gh.UpdateReport(ctx, t.Repository, id, r); err != nil {
		return id, err
	}
	return id, s.Runner.Record(ctx, run.ID, ci.Record{ReportID: &id, Reported: new(ci.ReportedQueued)})
}

// openTaskReports opens a queued report for each task of the run's pipeline, named
// "<check> / <task>", and records them on the run. With find, reports the code host already has
// for the run are taken over instead of opened again.
func (s *Service) openTaskReports(ctx context.Context, gh ci.Installation, run ci.Run, find bool) error {
	t := run.Trigger
	ids := maps.Clone(run.TaskReportIDs)
	if ids == nil {
		ids = map[string]ci.ReportID{}
	}
	opened := false
	var err error
	for _, task := range run.Tasks {
		if ids[task] != 0 {
			continue
		}
		name := reports.TaskName(t.ReportName(), task)
		var id ci.ReportID
		if find {
			if id, err = gh.FindReport(ctx, t.Repository, t.Revision, name, run.ID.String()); err != nil {
				break
			}
		}
		if id == 0 {
			r := ci.Report{
				Name: name, Revision: t.Revision, Status: ci.StatusQueued, ExternalID: run.ID.String(), URL: s.Runner.TaskURL(run.ID, task),
				Title: "Queued", Summary: fmt.Sprintf("Task `%s` of %s.", task, s.queuedSummary(run)), Trigger: &t,
			}
			if id, err = gh.OpenReport(ctx, t.Repository, r); err != nil {
				break
			}
		}
		ids[task], opened = id, true
	}
	if !opened {
		return err
	}
	return errors.Join(err, s.Runner.Record(ctx, run.ID, ci.Record{TaskReportIDs: ids}))
}

// mintToken mints the run's token and stores it where the run reads it.
func (s *Service) mintToken(ctx context.Context, run ci.Run) error {
	tok, err := run.Token.Mint(ctx, s.Host, run.Trigger)
	if err != nil {
		return fmt.Errorf("minting the run's token: %w", err)
	}
	return s.Runner.SetToken(ctx, run, tok)
}

// abort cancels a run that could not be started, fails its report and lets it go.
func (s *Service) abort(ctx context.Context, gh ci.Installation, run ci.Run, reportID ci.ReportID, cause error) {
	log := s.logFor(run.Trigger).With("run", run.ID.String())
	log.ErrorContext(ctx, "Could not start the run; cancelling it", "error", cause)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.Runner.Cancel(ctx, run.ID, ci.Cancellation{Reason: "could not be started"}); err != nil {
		log.ErrorContext(ctx, "Could not cancel the run", "error", err)
	}
	summary := fmt.Sprintf("Octomaton could not start %s `%s`:\n\n```\n%v\n```\n\nRe-run this check to try again.", s.Runner.Link(run.ID).Kind, run.ID, cause)
	if reportID != 0 {
		s.failReport(ctx, gh, run.Trigger, reportID, "The run could not be started", summary)
	} else {
		// Its report could not be opened: open the failure instead, with retries of its own.
		s.openCompleted(ctx, gh, run.Trigger, run.Trigger.ReportName(), ci.Failure, "The run could not be started", summary)
	}
	// The failure is reported: the run is let go, so its cancellation is not reported over it.
	if err := s.Runner.Record(ctx, run.ID, ci.Record{Reported: new(ci.ReportedCompleted), Done: true}); err != nil {
		log.ErrorContext(ctx, "Could not let the run go", "error", err)
	}
}

// definitionAt locates a pipeline's definition for t: in the trigger's repository where its
// configuration was read, or in another repository of the same owner at its default branch. where
// describes the location for messages.
func definitionAt(t ci.Trigger, ref pipelines.PipelineRunRef) (repo ci.Repository, at, where string) {
	if ref.Repository == "" {
		return t.Repository, t.ConfigAt(), "`" + ci.ShortSHA(t.ConfigAt()) + "`"
	}
	repo = sibling(t.Repository, ref.Repository)
	return repo, "", "the default branch of `" + repo.FullName + "`"
}

package runs

import (
	"context"
	"errors"
	"slices"
	"strings"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
)

// RerunLost starts the next attempt of a finished run that failed only because tasks lost their pods
// to disruptions (lost), as Re-run would, with one more taken from its reruns left. The attempt takes
// over the run's report and its task reports still open, so the checks stay in progress across the
// two; the run's other task reports are opened afresh. It follows the pipeline's concurrency rules:
// under supersede, an attempt whose commit is no longer the head stands itself down.
//
// It returns the attempt, also when an earlier call created it. A *ci.Refusal says why it did not
// rerun the run: its merge group is gone, or its pipeline is gone or would not start. Any other
// error is a call that failed; the attempt is not created, or was created and its failure reported.
func (s *Service) RerunLost(ctx context.Context, run ci.Run, lost []ci.Disruption) (ci.Run, error) {
	t := run.Trigger
	log := s.logFor(t).With("run", run.ID.String())
	if t.FromFork() {
		return ci.Run{}, errFromFork
	}
	if run.Reruns == nil || run.Reruns.Left <= 0 {
		return ci.Run{}, refused("it has no reruns left")
	}
	runs, err := s.Runner.List(ctx, ci.RunQuery{Repository: &t.Repository, Pipeline: t.Pipeline, Revision: t.Revision})
	if err != nil {
		return ci.Run{}, err
	}
	for _, r := range runs {
		if r.Reruns != nil && r.Reruns.Of == run.ID.Name {
			return r, nil
		}
	}
	gh := s.Host.Installation(t.InstallationID)
	if t.MergeGroup != nil {
		// A merge group's branch goes when the queue destroys it: nothing waits for its checks.
		_, err := gh.BranchHead(ctx, t.Repository, strings.TrimPrefix(t.MergeGroup.HeadRef, "refs/heads/"))
		if errors.Is(err, ci.ErrNotFound) {
			return ci.Run{}, refused("its merge group is gone")
		}
		if err != nil {
			return ci.Run{}, err
		}
	}
	cfg, err := s.loadConfig(ctx, gh, t, reporting{})
	if err != nil {
		return ci.Run{}, err
	}
	var p *pipelines.Pipeline
	if cfg != nil {
		p = cfg.Pipeline(t.Pipeline)
	}
	if p == nil {
		return ci.Run{}, refused("no valid configuration defines pipeline `" + t.Pipeline + "` anymore")
	}
	spec, refusal := s.prepare(ctx, gh, t, p)
	if refusal != nil {
		if refusal.Cause != nil {
			return ci.Run{}, refusal.Cause
		}
		return ci.Run{}, refused(refusal.Reason)
	}
	spec.Reruns = &ci.Reruns{
		Left: run.Reruns.Left - 1, Of: run.ID.Name,
		Lost: append(slices.Clone(run.Reruns.Lost), ci.LostAttempt{Attempt: run.Attempt, Run: run.ID.Name, Tasks: lost}),
	}
	spec.ReportID, spec.TaskReportIDs = run.ReportID, openTaskReports(run)
	attempt := nextAttempt(runs)
	var next ci.Run
	for tries := 0; ; tries++ {
		next, err = s.Runner.Create(ctx, spec, attempt)
		if errors.Is(err, ci.ErrExists) && tries < maxNameTries {
			attempt++
			continue
		}
		break
	}
	var r *ci.Refusal
	switch {
	case errors.As(err, &r) && r.Cause == nil:
		return ci.Run{}, refused(r.Reason)
	case err != nil:
		return ci.Run{}, err
	}
	if err := s.launch(ctx, gh, next); err != nil {
		return ci.Run{}, err
	}
	log.InfoContext(ctx, "Rerunning a run that lost tasks to disruptions", "rerun", next.ID.String(), "attempt", next.Attempt,
		"reportID", next.ReportID, "lost", len(lost), "rerunsLeft", spec.Reruns.Left)
	return next, nil
}

func refused(reason string) *ci.Refusal {
	return &ci.Refusal{Title: "Not rerun", Reason: reason}
}

// openTaskReports are the task reports of a run that are not completed: a rerun takes them over.
func openTaskReports(run ci.Run) map[string]ci.ReportID {
	open := map[string]ci.ReportID{}
	for task, id := range run.TaskReportIDs {
		if run.TaskReportStates[task] != ci.StatusCompleted {
			open[task] = id
		}
	}
	return open
}

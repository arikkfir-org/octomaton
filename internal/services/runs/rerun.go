package runs

import (
	"context"
	"fmt"
	"strings"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
)

// Rerun replays the trigger stored with each report as a new attempt. Re-running a pipeline's report
// (or one of its task reports) always runs the pipeline: path filters are not applied again, and the
// requester's write access stands in for pull request trust. Re-running the "octomaton" report
// evaluates the whole event again.
func (s *Service) Rerun(ctx context.Context, e *ci.RerunEvent) {
	log := s.Logger.With("repository", e.Repository.FullName, "requester", e.Requester, "delivery", e.DeliveryID, "approve", e.Approve)
	gh := s.Host.Installation(e.InstallationID)

	level, err := gh.Permission(ctx, e.Repository, e.Requester)
	if err != nil {
		log.ErrorContext(ctx, "Could not verify the requester's permission", "error", err)
		return
	}
	if !level.CanWrite() {
		log.WarnContext(ctx, "Ignoring a re-run request from a user without write access", "permission", level)
		return
	}
	refs := e.Reports
	if e.SuiteID != 0 {
		if refs, err = gh.SuiteReports(ctx, e.Repository, e.SuiteID); err != nil {
			log.ErrorContext(ctx, "Could not list the suite's reports", "suite", e.SuiteID, "error", err)
			return
		}
	}

	configs := map[string]*pipelines.Config{}
	done := map[string]bool{}
	for _, ref := range refs {
		t, ok := s.reportTrigger(ctx, gh, e, ref)
		if !ok {
			continue
		}
		t.InstallationID, t.DeliveryID, t.RerunBy = e.InstallationID, e.DeliveryID, e.Requester
		key := t.Revision + " " + t.Pipeline + " " + t.Head()
		if done[key] {
			continue // the task reports of a pipeline re-run the pipeline once
		}
		done[key] = true

		if t.Pipeline == "" {
			// The configuration's report: evaluate the whole event again. Untrusted pull requests
			// still need an explicit approval afterwards.
			s.Evaluate(ctx, t, EvalOptions{ReportConfigErrors: true, ApprovedBy: t.ApprovedBy, RerunBy: e.Requester})
			continue
		}
		if e.Approve || ref.Conclusion == ci.ActionRequired {
			t.ApprovedBy = e.Requester
		}
		cfg, ok := configs[t.ConfigAt()]
		if !ok {
			if cfg, ok = s.loadConfig(ctx, gh, t, true); !ok {
				continue
			}
			configs[t.ConfigAt()] = cfg
		}
		p := cfg.Pipeline(t.Pipeline)
		if p == nil {
			s.openCompleted(ctx, gh, t, t.Pipeline, ci.Failure, "Pipeline not found",
				fmt.Sprintf("`%s` at `%s` does not define pipeline `%s` anymore.", pipelines.FileName, ci.ShortSHA(t.ConfigAt()), t.Pipeline), nil, "")
			continue
		}
		s.logFor(t).InfoContext(ctx, "Re-running the pipeline", "requester", e.Requester, "approved", t.ApprovedBy != "")
		if _, err := s.start(ctx, gh, t, p, true); err != nil {
			s.logFor(t).WarnContext(ctx, "The re-run did not start", "error", err)
		}
	}
}

// reportTrigger returns the trigger stored with a report, reading it from the code host when the
// event did not carry it.
func (s *Service) reportTrigger(ctx context.Context, gh ci.Installation, e *ci.RerunEvent, ref ci.ReportRef) (ci.Trigger, bool) {
	log := s.Logger.With("repository", e.Repository.FullName, "reportID", ref.ID, "report", ref.Name)
	t := ref.Trigger
	if t == nil {
		var err error
		if t, err = gh.ReportTrigger(ctx, e.Repository, ref.ID); err != nil {
			log.WarnContext(ctx, "Could not read the report's trigger; cannot re-run it", "error", err)
			return ci.Trigger{}, false
		}
		if t == nil {
			log.WarnContext(ctx, "The report has no trigger; cannot re-run it")
			return ci.Trigger{}, false
		}
	}
	if !strings.EqualFold(t.Repository.FullName, e.Repository.FullName) || t.Revision != ref.Revision {
		log.WarnContext(ctx, "The report's trigger does not match the report; ignoring it", "triggerRepository", t.Repository.FullName, "triggerRevision", t.Revision)
		return ci.Trigger{}, false
	}
	return *t, true
}

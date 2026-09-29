package trigger

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/services/pipelines"
)

// RerunRequest asks to re-run check runs, from check_run.rerequested,
// check_run.requested_action or check_suite.rerequested.
type RerunRequest struct {
	InstallationID int64
	Repository     checkrun.Repository
	// CheckRuns to re-run; ignored when SuiteID is set.
	CheckRuns []*github.CheckRun
	// SuiteID re-runs the latest check run of every name in the suite.
	SuiteID int64
	// Requester is the user who asked; they need write access to the repository.
	Requester string
	// Approve is set for the "Approve and run" action.
	Approve    bool
	DeliveryID string
}

// Rerun replays the trigger context stored on each check run as a new attempt.
// Re-running a pipeline's check (or one of its task checks) always runs the
// pipeline: path filters are not re-applied and the requester's write access
// stands in for pull request trust. Re-running the "octomaton" check
// re-evaluates the whole event.
func (s *Service) Rerun(ctx context.Context, req RerunRequest) {
	owner, repo := req.Repository.Owner, req.Repository.Name
	log := s.Logger.With("repository", req.Repository.FullName, "requester", req.Requester, "delivery", req.DeliveryID, "approve", req.Approve)
	gh := s.GitHub.Installation(req.InstallationID)

	level, err := gh.PermissionLevel(ctx, owner, repo, req.Requester)
	if err != nil {
		log.Error("Could not verify the requester's permission", "error", err)
		return
	}
	if !githubapp.CanWrite(level) {
		log.Warn("Ignoring re-run request from a user without write access", "permission", level)
		return
	}

	runs := req.CheckRuns
	if req.SuiteID != 0 {
		if runs, err = gh.SuiteCheckRuns(ctx, owner, repo, req.SuiteID); err != nil {
			s.Metrics.CheckRunError(ctx, "list")
			log.Error("Could not list the check suite's check runs", "suite", req.SuiteID, "error", err)
			return
		}
	}

	configs := map[string]*pipelines.Config{}
	done := map[string]bool{}
	for _, cr := range runs {
		c, ok := s.checkRunContext(ctx, gh, req, cr)
		if !ok {
			continue
		}
		c.InstallationID = req.InstallationID
		c.DeliveryID = req.DeliveryID
		c.RerunBy = req.Requester
		key := c.Revision + " " + c.Pipeline + " " + c.Head()
		if done[key] {
			continue // task checks of a pipeline re-run the pipeline once
		}
		done[key] = true

		if c.Pipeline == "" {
			// The configuration check: evaluate the whole event again. Untrusted
			// pull requests still need an explicit approval afterwards.
			s.Evaluate(ctx, c, EvalOptions{ReportConfigErrors: true, ApprovedBy: c.ApprovedBy, RerunBy: req.Requester})
			continue
		}
		if req.Approve || cr.GetConclusion() == "action_required" {
			c.ApprovedBy = req.Requester
		}
		cfgKey := c.ConfigAt()
		cfg, ok := configs[cfgKey]
		if !ok {
			if cfg, ok = s.loadConfig(ctx, gh, c, true); !ok {
				continue
			}
			configs[cfgKey] = cfg
		}
		p := cfg.Pipeline(c.Pipeline)
		if p == nil {
			s.createCompleted(ctx, gh, c, c.Pipeline, "failure", "Pipeline not found",
				fmt.Sprintf("`%s` at `%s` does not define pipeline `%s` anymore.", pipelines.FileName, checkrun.ShortSHA(c.ConfigAt()), c.Pipeline), nil, "")
			continue
		}
		opts := startOptions{Rerun: true}
		if c.Comment != nil {
			opts.Dedupe = commentDedupe(c.Comment.ID)
		}
		s.logFor(c).Info("Re-running pipeline", "requester", req.Requester, "approved", c.ApprovedBy != "")
		if _, _, err := s.start(ctx, gh, c, p, opts); err != nil {
			s.logFor(c).Warn("Re-run did not start", "error", err)
		}
	}
}

// checkRunContext returns the trigger context stored in a check run's output,
// fetching the check run when the webhook payload does not include the text.
func (s *Service) checkRunContext(ctx context.Context, gh githubapp.Client, req RerunRequest, cr *github.CheckRun) (checkrun.Context, bool) {
	log := s.Logger.With("repository", req.Repository.FullName, "checkRunID", cr.GetID(), "check", cr.GetName())
	c, found, err := checkrun.DecodeMarker(cr.GetOutput().GetText())
	if !found && err == nil {
		full, getErr := gh.GetCheckRun(ctx, req.Repository.Owner, req.Repository.Name, cr.GetID())
		if getErr != nil {
			s.Metrics.CheckRunError(ctx, "get")
			log.Error("Could not fetch check run", "error", getErr)
			return checkrun.Context{}, false
		}
		c, found, err = checkrun.DecodeMarker(full.GetOutput().GetText())
	}
	switch {
	case err != nil:
		log.Warn("Check run has an unreadable trigger context; cannot re-run it", "error", err)
		return checkrun.Context{}, false
	case !found:
		log.Warn("Check run has no trigger context; cannot re-run it")
		return checkrun.Context{}, false
	case !strings.EqualFold(c.Repository.FullName, req.Repository.FullName) || c.Revision != cr.GetHeadSHA():
		log.Warn("Check run's trigger context does not match the check run; ignoring it", "contextRepository", c.Repository.FullName, "contextRevision", c.Revision)
		return checkrun.Context{}, false
	}
	return c, true
}

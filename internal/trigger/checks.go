package trigger

import (
	"context"
	"net/url"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/tekton"
)

// createCompleted creates a completed check run whose output carries the trigger
// context, so it can be re-run later.
func (s *Service) createCompleted(ctx context.Context, gh githubapp.Client, c checkrun.Context, name, conclusion, title, summary string, actions []*github.CheckRunAction, detailsURL string) {
	now := github.Timestamp{Time: s.now()}
	opts := github.CreateCheckRunOptions{
		Name:        name,
		HeadSHA:     c.Revision,
		Status:      new(tekton.StatusCompleted),
		Conclusion:  new(conclusion),
		StartedAt:   &now,
		CompletedAt: &now,
		Output: &github.CheckRunOutput{
			Title:   new(title),
			Summary: new(checkrun.Truncate(summary, checkrun.MaxSummaryLength)),
			Text:    new(checkrun.WithMarker("", c)),
		},
		Actions: actions,
	}
	if detailsURL != "" {
		opts.DetailsURL = new(detailsURL)
	}
	if _, err := gh.CreateCheckRun(ctx, c.Repository.Owner, c.Repository.Name, opts); err != nil {
		s.Metrics.CheckRunError(ctx, "create")
		s.logFor(c).Error("Could not create check run", "check", name, "conclusion", conclusion, "error", err)
	}
}

// failCheckRun completes an existing check run with a failure.
func (s *Service) failCheckRun(ctx context.Context, gh githubapp.Client, c checkrun.Context, id int64, title, summary string) {
	now := github.Timestamp{Time: s.now()}
	_, err := gh.UpdateCheckRun(ctx, c.Repository.Owner, c.Repository.Name, id, githubapp.CheckRunUpdate{
		Status:      new(tekton.StatusCompleted),
		Conclusion:  new(tekton.ConclusionFailure),
		CompletedAt: &now,
		Output: &github.CheckRunOutput{
			Title:   new(title),
			Summary: new(checkrun.Truncate(summary, checkrun.MaxSummaryLength)),
			Text:    new(checkrun.WithMarker("", c)),
		},
	})
	if err != nil {
		s.Metrics.CheckRunError(ctx, "update")
		s.logFor(c).Error("Could not update check run", "checkRunID", id, "error", err)
	}
}

// TaskURL links to one task of a run in the Tekton Dashboard.
func TaskURL(dashboard, namespace, run, task string) string {
	u := checkrun.DashboardURL(dashboard, namespace, run)
	if u == "" {
		return ""
	}
	return u + "?pipelineTask=" + url.QueryEscape(task)
}

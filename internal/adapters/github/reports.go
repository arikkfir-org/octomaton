package github

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/services/ci"
)

// A report is a check run. Its output text ends with a marker holding the report's trigger, so a
// check run can be re-run after its run is gone.

// checkRunUpdate is the body of a check-run update; unlike go-github's UpdateCheckRunOptions it
// can set started_at.
type checkRunUpdate struct {
	Name        string                   `json:"name,omitempty"`
	DetailsURL  *string                  `json:"details_url,omitempty"`
	ExternalID  *string                  `json:"external_id,omitempty"`
	Status      *string                  `json:"status,omitempty"`
	Conclusion  *string                  `json:"conclusion,omitempty"`
	StartedAt   *github.Timestamp        `json:"started_at,omitempty"`
	CompletedAt *github.Timestamp        `json:"completed_at,omitempty"`
	Output      *github.CheckRunOutput   `json:"output,omitempty"`
	Actions     []*github.CheckRunAction `json:"actions,omitempty"`
}

// OpenReport creates a check run.
func (c *installation) OpenReport(ctx context.Context, repo ci.Repository, r ci.Report) (ci.ReportID, error) {
	opts := github.CreateCheckRunOptions{
		Name:        r.Name,
		HeadSHA:     r.Revision,
		Status:      optional(string(r.Status)),
		Conclusion:  optional(string(r.Conclusion)),
		ExternalID:  optional(r.ExternalID),
		DetailsURL:  optional(r.URL),
		StartedAt:   timestamp(r.Started),
		CompletedAt: timestamp(r.Completed),
		Output:      output(r),
		Actions:     actions(r.Actions),
	}
	cr, _, err := c.gh.Checks.CreateCheckRun(ctx, repo.Owner, repo.Name, opts)
	if err != nil {
		c.app.metrics.CheckRunError(ctx, "create")
		return 0, fmt.Errorf("creating check run %q: %w", r.Name, err)
	}
	return ci.ReportID(cr.GetID()), nil
}

// UpdateReport changes a check run's non-zero fields.
func (c *installation) UpdateReport(ctx context.Context, repo ci.Repository, id ci.ReportID, r ci.Report) error {
	body := checkRunUpdate{
		Name:        r.Name,
		DetailsURL:  optional(r.URL),
		ExternalID:  optional(r.ExternalID),
		Status:      optional(string(r.Status)),
		Conclusion:  optional(string(r.Conclusion)),
		StartedAt:   timestamp(r.Started),
		CompletedAt: timestamp(r.Completed),
		Output:      output(r),
		Actions:     actions(r.Actions),
	}
	req, err := c.gh.NewRequest(ctx, http.MethodPatch, fmt.Sprintf("repos/%v/%v/check-runs/%v", repo.Owner, repo.Name, id), body)
	if err == nil {
		_, err = c.gh.Do(req, nil)
	}
	if err != nil {
		c.app.metrics.CheckRunError(ctx, "update")
		return fmt.Errorf("updating check run %d: %w", id, err)
	}
	return nil
}

// FindReport returns the newest of the App's check runs on revision with the given name and
// external ID, or 0.
func (c *installation) FindReport(ctx context.Context, repo ci.Repository, revision, name, externalID string) (ci.ReportID, error) {
	opts := &github.ListCheckRunsOptions{CheckName: &name, Filter: new("all"), AppID: &c.app.id, ListOptions: github.ListOptions{PerPage: 100}}
	var found int64
	for {
		res, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, repo.Owner, repo.Name, revision, opts)
		if err != nil {
			return 0, fmt.Errorf("listing check runs of %s: %w", revision, err)
		}
		for _, cr := range res.CheckRuns {
			if cr.GetName() == name && cr.GetExternalID() == externalID && cr.GetID() > found {
				found = cr.GetID()
			}
		}
		if resp.NextPage == 0 {
			return ci.ReportID(found), nil
		}
		opts.Page = resp.NextPage
	}
}

// ReportTrigger reads the trigger stored in a check run's marker.
func (c *installation) ReportTrigger(ctx context.Context, repo ci.Repository, id ci.ReportID) (*ci.Trigger, error) {
	cr, resp, err := c.gh.Checks.GetCheckRun(ctx, repo.Owner, repo.Name, int64(id))
	if err != nil {
		c.app.metrics.CheckRunError(ctx, "get")
		if isNotFound(resp) {
			return nil, ci.ErrNotFound
		}
		return nil, fmt.Errorf("fetching check run %d: %w", id, err)
	}
	t, found, err := DecodeMarker(cr.GetOutput().GetText())
	if err != nil || !found {
		return nil, err
	}
	return &t, nil
}

// SuiteReports lists the latest check run of each name in a check suite.
func (c *installation) SuiteReports(ctx context.Context, repo ci.Repository, suiteID int64) ([]ci.ReportRef, error) {
	var refs []ci.ReportRef
	opts := &github.ListCheckRunsOptions{Filter: new("latest"), ListOptions: github.ListOptions{PerPage: 100}}
	for {
		res, resp, err := c.gh.Checks.ListCheckRunsCheckSuite(ctx, repo.Owner, repo.Name, suiteID, opts)
		if err != nil {
			c.app.metrics.CheckRunError(ctx, "list")
			return nil, fmt.Errorf("listing check runs of suite %d: %w", suiteID, err)
		}
		for _, cr := range res.CheckRuns {
			refs = append(refs, reportRef(cr))
		}
		if resp.NextPage == 0 {
			return refs, nil
		}
		opts.Page = resp.NextPage
	}
}

// reportRef names a check run, with the trigger its output carries when it can be read.
func reportRef(cr *github.CheckRun) ci.ReportRef {
	ref := ci.ReportRef{
		ID:         ci.ReportID(cr.GetID()),
		Name:       cr.GetName(),
		Revision:   cr.GetHeadSHA(),
		Conclusion: ci.Conclusion(cr.GetConclusion()),
	}
	if t, found, err := DecodeMarker(cr.GetOutput().GetText()); found && err == nil {
		ref.Trigger = &t
	}
	return ref
}

// output is a report's check-run output: the summary within MaxSummaryLength, the text ending with
// the trigger's marker. A report without any output sets none.
func output(r ci.Report) *github.CheckRunOutput {
	if r.Title == "" && r.Summary == "" && r.Text == "" && r.Trigger == nil {
		return nil
	}
	text := Truncate(r.Text, MaxOutputLength)
	if r.Trigger != nil {
		text = WithMarker(r.Text, *r.Trigger)
	}
	return &github.CheckRunOutput{
		Title:   new(r.Title),
		Summary: new(Truncate(r.Summary, MaxSummaryLength)),
		Text:    new(text),
	}
}

func actions(in []ci.Action) []*github.CheckRunAction {
	var out []*github.CheckRunAction
	for _, a := range in {
		out = append(out, &github.CheckRunAction{Label: a.Label, Description: a.Description, Identifier: a.ID})
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timestamp(t time.Time) *github.Timestamp {
	if t.IsZero() {
		return nil
	}
	return &github.Timestamp{Time: t}
}

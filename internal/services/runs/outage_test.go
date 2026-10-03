package runs

import (
	"context"
	"errors"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// gitHubDown is what a call returns once the code host's retries are spent.
var gitHubDown = errors.New("GET https://api.github.com/repos/octo-org/demo/contents/.octomaton.yaml: 504 Gateway Timeout")

func TestAnUnreadableConfigurationIsAlwaysReported(t *testing.T) {
	labeled := branchPR(sha1)
	labeled.Action = "labeled"
	tests := []struct {
		name    string
		trigger ci.Trigger
	}{
		{name: "a push", trigger: pushTrigger(sha1, "main")},
		{name: "new commits on a pull request", trigger: branchPR(sha1)},
		{name: "a pull request action that reports no invalid configuration", trigger: labeled},
		{name: "a review request", trigger: reviewTrigger("d-1", "octo-reviewer")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.host.Fail("ReadFile", gitHubDown)
			h.svc.Handle(context.Background(), &ci.TriggerEvent{Trigger: tt.trigger})
			r := h.onlyReport(ci.ConfigReportName)
			if r.Conclusion != ci.Failure || r.Title != "Could not read .octomaton.yaml" || r.Revision != sha1 || r.Trigger == nil || r.Trigger.Event != tt.trigger.Event {
				t.Fatalf("report = %+v", r)
			}
			mustContain(t, r.Summary, "504 Gateway Timeout", "Re-run this check to try again.")
		})
	}
}

func TestRerunningTheReportAfterAnOutage(t *testing.T) {
	h := newHarness(t)
	setupReview(h, reviewConfig)
	h.host.FailFile(repo, "main", ".octomaton.yaml", gitHubDown)
	h.requestReview("d-1", "octo-reviewer")
	if runs := h.runner.Runs(); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none while GitHub is down", runs)
	}
	failed := h.onlyReport(ci.ConfigReportName)

	// GitHub is back: re-running the report evaluates the request again, and clears the report.
	h.host.FailFile(repo, "main", ".octomaton.yaml", nil)
	h.svc.Rerun(context.Background(), rerun("maintainer", failed))
	runs := h.runner.Runs()
	if len(runs) != 1 || runs[0].Trigger.ReviewRequest == nil || runs[0].Trigger.ReviewRequest.Reviewer != "octo-reviewer" {
		t.Fatalf("runs = %+v, want the review", runs)
	}
	reports := h.host.ReportsNamed(ci.ConfigReportName)
	if len(reports) != 2 || reports[1].Conclusion != ci.Success || reports[1].Title != "Evaluated again" || reports[1].Revision != sha1 {
		t.Fatalf("configuration reports = %+v, want the failure then a success", reports)
	}
}

func TestARunWhoseReportCannotBeOpened(t *testing.T) {
	tests := []struct {
		name     string
		failures int // of OpenReport: the run's report, then the failure's
		reported bool
	}{
		{name: "reports its failure instead", failures: 1, reported: true},
		// Both spent their retries: the error log is the one trace left.
		{name: "is only logged when the failure can't be reported either", failures: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, ciConfig, ciRun)
			h.host.FailNext("OpenReport", gitHubDown, tt.failures)
			h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true})
			if run := h.run("demo-ci-1111111-1"); !run.CancelRequested {
				t.Fatalf("the run must be cancelled: %+v", run)
			}
			reports := h.host.ReportsNamed("ci")
			if !tt.reported {
				if len(reports) != 0 {
					t.Fatalf("reports = %+v, want none", reports)
				}
				return
			}
			r := h.onlyReport("ci")
			if r.Conclusion != ci.Failure || r.Title != "The run could not be started" || r.Trigger == nil || r.Trigger.Pipeline != "ci" {
				t.Fatalf("report = %+v", r)
			}
			mustContain(t, r.Summary, "504 Gateway Timeout", "Re-run this check to try again.")
		})
	}
}

func TestFailuresAreReportedPastTheJobsDeadline(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(h *harness)
		report    string
		wantTitle string
	}{
		{
			name:   "an unreadable configuration",
			setup:  func(h *harness) { h.host.Fail("ReadFile", context.DeadlineExceeded) },
			report: ci.ConfigReportName, wantTitle: "Could not read .octomaton.yaml",
		},
		{
			// The run's report fails with the job's context; its failure is reported on a context of its own.
			name:   "a run whose report could not be opened",
			setup:  func(h *harness) { h.files(sha1, ciConfig, ciRun) },
			report: "ci", wantTitle: "The run could not be started",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			h.svc.Evaluate(ctx, branchPR(sha1), EvalOptions{ReportConfigErrors: true})
			if r := h.onlyReport(tt.report); r.Conclusion != ci.Failure || r.Title != tt.wantTitle {
				t.Fatalf("report = %+v", r)
			}
		})
	}
}

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

func TestHandleSaysWhatFailedCallsLeftUndone(t *testing.T) {
	queue := "gh-readonly-queue/main/pr-5-" + baseSHA
	mergeGroup := ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventMergeGroup, Action: "checks_requested", InstallationID: installationID,
		Repository: repo, Revision: sha1, Ref: "refs/heads/" + queue, Branch: queue,
		MergeGroup: &ci.MergeGroup{HeadRef: "refs/heads/" + queue, HeadSHA: sha1, BaseRef: "refs/heads/main", BaseSHA: baseSHA},
	}
	kubeDown := errors.New("the API server is down")
	reRun := func(e *ci.RerunEvent) *ci.RerunEvent {
		e.InstallationID, e.Repository, e.Requester, e.DeliveryID = installationID, repo, "maintainer", "r"
		return e
	}
	tests := []struct {
		name       string
		event      ci.Event
		setup      func(h *harness)
		wantUndone bool
	}{
		{name: "a started run", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}},
		{name: "an unreadable configuration", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}, wantUndone: true,
			setup: func(h *harness) { h.host.Fail("ReadFile", gitHubDown) }},
		{name: "an invalid configuration", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")},
			setup: func(h *harness) { h.files(sha1, "apiVersion: nope\n", "") }},
		{name: "an invalid configuration whose report would not open", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}, wantUndone: true,
			setup: func(h *harness) { h.files(sha1, "apiVersion: nope\n", ""); h.host.Fail("OpenReport", gitHubDown) }},
		{name: "a refused run", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")},
			setup: func(h *harness) { h.runner.Fail("Create", &ci.Refusal{Title: "Refused", Reason: "no"}) }},
		{name: "a refused run whose report would not open", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}, wantUndone: true,
			setup: func(h *harness) {
				h.runner.Fail("Create", &ci.Refusal{Title: "Refused", Reason: "no"})
				h.host.Fail("OpenReport", gitHubDown)
			}},
		{name: "an unreadable pipeline definition", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}, wantUndone: true,
			setup: func(h *harness) { h.host.FailFile(repo, sha1, ".tekton/ci.yaml", gitHubDown) }},
		{name: "a run the runner would not create", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}, wantUndone: true,
			setup: func(h *harness) { h.runner.Fail("Create", kubeDown) }},
		// As the Tekton runner refuses a run when the cluster would not answer.
		{name: "a run refused because the cluster would not create it", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}, wantUndone: true,
			setup: func(h *harness) {
				h.runner.Fail("Create", &ci.Refusal{Title: "Could not create the PipelineRun", Reason: "Kubernetes refused it", Cause: kubeDown})
			}},
		{name: "a run refused because the cluster would not check it", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}, wantUndone: true,
			setup: func(h *harness) {
				h.runner.Fail("Check", &ci.Refusal{Title: "Could not start the pipeline", Reason: "Could not verify the namespace", Cause: kubeDown})
			}},
		{name: "a run whose report would not open", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")}, wantUndone: true,
			setup: func(h *harness) { h.host.Fail("OpenReport", gitHubDown) }},
		{name: "a declined command", event: command(1, "maintainer", "/deploy"),
			setup: func(h *harness) {
				setupComment(h)
				h.host.SetPullRequest(repo, openPR(func(p *ci.PullRequestState) { p.Draft = true }))
			}},
		{name: "a declined command whose reply would not post", event: command(1, "maintainer", "/deploy"), wantUndone: true,
			setup: func(h *harness) {
				setupComment(h)
				h.host.SetPullRequest(repo, openPR(func(p *ci.PullRequestState) { p.Draft = true }))
				h.host.Fail("Comment", gitHubDown)
			}},
		{name: "a refused command", event: command(1, "maintainer", "/deploy"),
			setup: func(h *harness) {
				setupComment(h)
				h.runner.Fail("Create", &ci.Refusal{Title: "Refused", Reason: "the PipelineRun references Secret \"x\""})
			}},
		{name: "a command refused because the cluster would not create its run", event: command(1, "maintainer", "/deploy"), wantUndone: true,
			setup: func(h *harness) {
				setupComment(h)
				h.runner.Fail("Create", &ci.Refusal{Title: "Could not create the PipelineRun", Reason: "Kubernetes refused it", Cause: kubeDown})
			}},
		{name: "a command whose pull request would not read", event: command(1, "maintainer", "/deploy"), wantUndone: true,
			setup: func(h *harness) { setupComment(h); h.host.Fail("PullRequest", gitHubDown) }},
		{name: "a re-run whose requester's permission would not read", wantUndone: true, event: reRun(&ci.RerunEvent{}),
			setup: func(h *harness) { h.host.Fail("Permission", gitHubDown) }},
		{name: "a re-run whose suite would not list", wantUndone: true, event: reRun(&ci.RerunEvent{SuiteID: 1}),
			setup: func(h *harness) { h.host.Fail("SuiteReports", gitHubDown) }},
		{name: "a re-run whose report's trigger would not read", wantUndone: true,
			event: reRun(&ci.RerunEvent{Reports: []ci.ReportRef{{ID: 1, Name: "ci", Revision: sha1}}}),
			setup: func(h *harness) { h.host.Fail("ReportTrigger", gitHubDown) }},
		{name: "a dropped merge group", event: &ci.MergeGroupDestroyed{Trigger: mergeGroup},
			setup: func(h *harness) { h.evaluate(mergeGroup, EvalOptions{}) }},
		{name: "a dropped merge group whose runs would not list", event: &ci.MergeGroupDestroyed{Trigger: mergeGroup}, wantUndone: true,
			setup: func(h *harness) { h.runner.Fail("List", kubeDown) }},
		{name: "a dropped merge group whose run would not cancel", event: &ci.MergeGroupDestroyed{Trigger: mergeGroup}, wantUndone: true,
			setup: func(h *harness) { h.evaluate(mergeGroup, EvalOptions{}); h.runner.Fail("Cancel", kubeDown) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, ciConfig, ciRun)
			if tt.setup != nil {
				tt.setup(h)
			}
			err := h.svc.Handle(context.Background(), tt.event)
			if (err != nil) != tt.wantUndone {
				t.Fatalf("Handle = %v, want undone %v", err, tt.wantUndone)
			}
		})
	}
}

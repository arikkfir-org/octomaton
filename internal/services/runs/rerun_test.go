package runs

import (
	"context"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/ci/citest"
)

// rerun asks to re-run reports. The event carries no triggers, as when GitHub leaves the check
// runs' text out of the payload: the service reads them from the host.
func rerun(requester string, reports ...citest.Report) *ci.RerunEvent {
	e := &ci.RerunEvent{InstallationID: installationID, Repository: repo, Requester: requester, DeliveryID: "rerun"}
	for _, r := range reports {
		e.Reports = append(e.Reports, ci.ReportRef{ID: r.ID, Name: r.Name, Revision: r.Revision, Conclusion: r.Conclusion})
	}
	return e
}

func TestRerunCreatesANewAttempt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
	h.finish("demo-ci-1111111-1", ci.Failure)
	first := h.onlyReport("ci")

	h.svc.Rerun(ctx, rerun("reader", first))
	if len(h.runner.Runs()) != 1 {
		t.Fatalf("a user without write access cannot re-run")
	}
	h.svc.Rerun(ctx, rerun("maintainer", first))
	second := h.run("demo-ci-1111111-2")
	if tr := second.Trigger; tr.RerunBy != "maintainer" || tr.DeliveryID != "rerun" || tr.Event != ci.EventPush || tr.Branch != "main" {
		t.Fatalf("re-run trigger = %+v", tr)
	}
	if n := len(h.host.ReportsNamed("ci")); n != 2 {
		t.Fatalf("a re-run gets its own report, found %d", n)
	}
}

func TestRerunOfSkippedReportRunsThePipeline(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, pathsConfig, ciRun)
	h.host.SetPullRequestFiles(repo, 5, ci.ChangedFiles{Files: []string{"README.md"}, Complete: true})
	h.evaluate(branchPR(sha1), EvalOptions{})
	skipped := h.onlyReport("ci")
	if skipped.Conclusion != ci.Skipped || len(h.runner.Runs()) != 0 {
		t.Fatalf("setup: the pipeline must be skipped")
	}
	h.svc.Rerun(context.Background(), rerun("maintainer", skipped))
	if len(h.runner.Runs()) != 1 {
		t.Fatalf("re-running a skipped report runs the pipeline anyway")
	}
}

// TestRerunIgnoresForks re-runs reports whose stored trigger is a pull request from a fork, as an
// earlier version could have created: nothing runs and nothing is reported.
func TestRerunIgnoresForks(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
	}{
		{name: "a pipeline's report", pipeline: "ci"},
		{name: "the configuration's report", pipeline: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, ciConfig, ciRun)
			fork := prTrigger(sha1, 9, "stranger/demo")
			fork.Pipeline = tt.pipeline
			e := rerun("maintainer")
			e.Reports = []ci.ReportRef{{ID: 99, Name: "ci", Revision: sha1, Conclusion: ci.Failure, Trigger: &fork}}
			h.svc.Handle(context.Background(), e)
			if len(h.runner.Runs()) != 0 || len(h.host.Reports()) != 0 {
				t.Fatalf("runs = %+v, reports = %+v, want none", h.runner.Runs(), h.host.Reports())
			}
		})
	}
}

func TestRerunSuiteRunsEachPipelineOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, strings.Replace(ciConfig, "githubToken: {workspace: github-token}", "taskChecks: true", 1), ciRun)
	h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
	h.finish("demo-ci-1111111-1", ci.Failure)
	// The suite holds ci, ci / build and ci / test, which all carry the pipeline's trigger.
	h.svc.Rerun(ctx, &ci.RerunEvent{InstallationID: installationID, Repository: repo, SuiteID: h.host.SuiteID(repo, sha1), Requester: "maintainer", DeliveryID: "suite"})
	if runs := h.runner.Runs(); len(runs) != 2 {
		t.Fatalf("runs = %d, want the original and one re-run", len(runs))
	}
}

func TestRerunOfTheConfigReportEvaluatesAgain(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, "apiVersion: v0\n", "")
	h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true})
	config := h.onlyReport(ci.ConfigReportName)
	// The file cannot change at the same commit, but a transient problem can go away.
	h.files(sha1, ciConfig, ciRun)
	h.svc.Rerun(context.Background(), rerun("maintainer", config))
	if len(h.runner.Runs()) != 1 {
		t.Fatalf("re-running the configuration's report evaluates the event again")
	}
}

func TestRerunIgnoresReportsWithoutAMatchingTrigger(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	other := branchPR(sha1)
	other.Repository.FullName = "someone/else"
	stale := branchPR(sha1)
	stale.Revision = sha2
	for _, tr := range []*ci.Trigger{nil, &other, &stale} {
		id := h.host.AddReport(repo, ci.Report{Name: "ci", Revision: sha1, Trigger: tr})
		r, _ := h.host.Report(id)
		h.svc.Rerun(context.Background(), rerun("maintainer", r))
	}
	if len(h.runner.Runs()) != 0 {
		t.Fatalf("reports without a matching trigger must not be re-run")
	}
}

func TestRerunOfAPipelineThatIsGone(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	gone := branchPR(sha1)
	gone.Pipeline = "release"
	id := h.host.AddReport(repo, ci.Report{Name: "release", Revision: sha1, Conclusion: ci.Failure, Trigger: &gone})
	r, _ := h.host.Report(id)
	h.svc.Rerun(context.Background(), rerun("maintainer", r))
	reports := h.host.ReportsNamed("release")
	if len(h.runner.Runs()) != 0 || len(reports) != 2 || reports[1].Title != "Pipeline not found" {
		t.Fatalf("reports = %+v", reports)
	}
}

func TestRerunAfterCommentUsesTheCommentsConfiguration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setupComment(h)
	h.svc.Handle(ctx, command(101, "maintainer", "/deploy staging"))
	h.svc.Rerun(ctx, rerun("maintainer", h.onlyReport("deploy")))
	runs := h.runner.Runs()
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	for _, r := range runs {
		if r.Trigger.Comment == nil || r.Trigger.Comment.ID != 101 || r.Trigger.ConfigRef != "main" {
			t.Fatalf("a re-run of a comment's run keeps the comment: %+v", r.Trigger)
		}
	}
}

package trigger

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/adapters/github/githubtest"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/tekton"
)

func checkRunFor(cr githubtest.CheckRun) *github.CheckRun {
	// Webhook payloads may carry the output text; pass none so that Octomaton
	// has to fetch the check run, as it must when GitHub leaves it out.
	return &github.CheckRun{ID: new(cr.ID), Name: new(cr.Name), HeadSHA: new(cr.HeadSHA), Conclusion: new(cr.Conclusion)}
}

func rerunRequest(requester string, runs ...githubtest.CheckRun) RerunRequest {
	req := RerunRequest{InstallationID: installationID, Repository: repo(), Requester: requester, DeliveryID: "rerun"}
	for _, cr := range runs {
		req.CheckRuns = append(req.CheckRuns, checkRunFor(cr))
	}
	return req
}

func TestRerunCreatesANewAttempt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.svc.Evaluate(ctx, pushCtx(sha1, "main"), EvalOptions{})
	h.finish("demo-ci-1111111-1", "False", "Failed")
	first := h.onlyCheck("ci")

	h.svc.Rerun(ctx, rerunRequest("reader", first))
	if len(h.allRuns()) != 1 {
		t.Fatalf("a user without write access cannot re-run")
	}
	h.svc.Rerun(ctx, rerunRequest("maintainer", first))
	second := h.run("demo-ci-1111111-2")
	c := contextAnnotation(t, second)
	if c.RerunBy != "maintainer" || c.DeliveryID != "rerun" || c.Event != checkrun.EventPush || c.Branch != "main" {
		t.Fatalf("re-run context = %+v", c)
	}
	if n := len(h.checks("ci")); n != 2 {
		t.Fatalf("a re-run gets its own check run, found %d", n)
	}
}

func TestRerunOfSkippedCheckRunsThePipeline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, pathsConfig, ciRun)
	h.gh.SetPullRequestFiles(fullName, 5, []string{"README.md"})
	h.svc.Evaluate(ctx, trustedPR(sha1), EvalOptions{})
	skipped := h.onlyCheck("ci")
	if skipped.Conclusion != "skipped" || len(h.allRuns()) != 0 {
		t.Fatalf("setup: the pipeline must be skipped")
	}
	h.svc.Rerun(ctx, rerunRequest("maintainer", skipped))
	if len(h.allRuns()) != 1 {
		t.Fatalf("re-running a skipped check runs the pipeline anyway")
	}
}

func TestApproveAndRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.svc.Evaluate(ctx, prContext(sha1, 9, "NONE", "stranger/demo"), EvalOptions{})
	pending := h.onlyCheck("ci")
	if pending.Conclusion != "action_required" {
		t.Fatalf("setup: approval must be required")
	}
	req := rerunRequest("reader", pending)
	req.Approve = true
	h.svc.Rerun(ctx, req)
	if len(h.allRuns()) != 0 {
		t.Fatalf("a user without write access cannot approve")
	}
	req.Requester = "maintainer"
	h.svc.Rerun(ctx, req)
	runs := h.allRuns()
	if len(runs) != 1 {
		t.Fatalf("an approval runs the pipeline")
	}
	if c := contextAnnotation(t, &runs[0]); c.ApprovedBy != "maintainer" || c.PullRequest.Number != 9 {
		t.Fatalf("approved context = %+v", c)
	}
	// Re-running an action_required check also counts as an approval.
	h2 := newHarness(t)
	h2.files(sha1, ciConfig, ciRun)
	h2.svc.Evaluate(ctx, prContext(sha1, 9, "NONE", "stranger/demo"), EvalOptions{})
	h2.svc.Rerun(ctx, rerunRequest("maintainer", h2.onlyCheck("ci")))
	if runs := h2.allRuns(); len(runs) != 1 || contextAnnotation(t, &runs[0]).ApprovedBy != "maintainer" {
		t.Fatalf("re-running an action_required check must run it as approved")
	}
}

func TestRerunSuiteRunsEachPipelineOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, strings.Replace(ciConfig, "githubToken: {workspace: github-token}", "taskChecks: true", 1), ciRun)
	h.svc.Evaluate(ctx, pushCtx(sha1, "main"), EvalOptions{})
	h.finish("demo-ci-1111111-1", "False", "Failed")
	// The task checks carry their pipeline's context: the suite holds ci, ci / build and ci / test.
	for _, name := range []string{"ci / build", "ci / test"} {
		cr := h.onlyCheck(name)
		_, _ = h.svc.GitHub.Installation(installationID).UpdateCheckRun(ctx, owner, repoName, cr.ID, checkrunUpdateWithText(contextAnnotation(t, h.run("demo-ci-1111111-1"))))
	}
	suite := h.gh.SuiteID(fullName, sha1)
	h.svc.Rerun(ctx, RerunRequest{InstallationID: installationID, Repository: repo(), SuiteID: suite, Requester: "maintainer", DeliveryID: "suite"})
	if runs := h.allRuns(); len(runs) != 2 {
		t.Fatalf("runs = %d, want the original and one re-run", len(runs))
	}
}

func TestRerunOfTheConfigCheckReevaluates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, "apiVersion: v0\n", "")
	h.svc.Evaluate(ctx, trustedPR(sha1), EvalOptions{ReportConfigErrors: true})
	cfgCheck := h.onlyCheck(checkrun.ConfigCheckName)
	// The file cannot change at the same commit, but a transient problem can go away.
	h.files(sha1, ciConfig, ciRun)
	h.svc.Rerun(ctx, rerunRequest("maintainer", cfgCheck))
	if len(h.allRuns()) != 1 {
		t.Fatalf("re-running the configuration check re-evaluates the event")
	}
}

func TestRerunIgnoresChecksWithoutOrWithMismatchedContext(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	foreign := h.gh.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci", HeadSHA: sha1, Text: "no marker here"})
	other := trustedPR(sha1)
	other.Repository.FullName = "someone/else"
	mismatched := h.gh.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci", HeadSHA: sha1, Text: checkrun.MustMarker(other)})
	for _, id := range []int64{foreign, mismatched} {
		cr, _ := h.gh.CheckRun(id)
		h.svc.Rerun(ctx, rerunRequest("maintainer", cr))
	}
	if len(h.allRuns()) != 0 {
		t.Fatalf("check runs without a matching context must not be re-run")
	}
}

func checkrunUpdateWithText(c checkrun.Context) githubapp.CheckRunUpdate {
	return githubapp.CheckRunUpdate{Output: &github.CheckRunOutput{Text: new(checkrun.MustMarker(c))}}
}

func TestRerunAfterCommentUsesTheCommentsConfiguration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setupComment(h)
	h.svc.HandleComment(ctx, commentReq(101, "maintainer", "/deploy staging"))
	check := h.onlyCheck("deploy")
	h.svc.Rerun(ctx, rerunRequest("maintainer", check))
	runs := h.allRuns()
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	for i := range runs {
		if runs[i].GetLabels()[tekton.LabelComment] != "101" {
			t.Fatalf("a re-run of a comment's run keeps the comment: %v", runs[i].GetLabels())
		}
	}
}

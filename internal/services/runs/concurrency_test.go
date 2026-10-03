package runs

import (
	"context"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
)

func TestSupersedeCancelsOlderCommitsRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.files(sha2, ciConfig, ciRun)
	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: ci.PullRequest{Number: 5, HeadSHA: sha1}, State: "open"})
	h.evaluate(branchPR(sha1), EvalOptions{})
	if first := h.run("demo-ci-1111111-1"); first.Phase != ci.Released || first.CancelRequested {
		t.Fatalf("the first run must go: %+v", first)
	}

	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: ci.PullRequest{Number: 5, HeadSHA: sha2}, State: "open"})
	h.evaluate(branchPR(sha2), EvalOptions{})
	first, second := h.run("demo-ci-1111111-1"), h.run("demo-ci-2222222-1")
	if !first.CancelRequested || first.Cancellation.SupersededBy != second.ID.Name {
		t.Fatalf("the older commit's run must be superseded: %+v", first)
	}
	if second.Phase != ci.Released || second.CancelRequested {
		t.Fatalf("the newest commit's run must go: %+v", second)
	}

	// A late delivery for the old commit must not stand the head's run down.
	late := branchPR(sha1)
	late.DeliveryID = "late"
	h.svc.Evaluate(ctx, late, EvalOptions{})
	if stale := h.run("demo-ci-1111111-2"); !stale.CancelRequested || stale.Cancellation.NewerCommit != sha2 {
		t.Fatalf("a run for a commit that is no longer the head must stand itself down: %+v", stale)
	}
	if h.run("demo-ci-2222222-1").CancelRequested {
		t.Fatalf("the head's run must keep running")
	}
}

func TestSupersedeOfSameCommitKeepsTheNewestReport(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: ci.PullRequest{Number: 5, HeadSHA: sha1}, State: "open"})
	h.evaluate(branchPR(sha1), EvalOptions{})
	// A re-run of the same commit is a new attempt with a newer report: it wins.
	cfg, _ := h.svc.LoadConfig(ctx, branchPR(sha1))
	tr := branchPR(sha1)
	tr.Pipeline = "ci"
	if _, err := h.svc.start(ctx, h.host.Installation(installationID), tr, cfg.Pipeline("ci"), true); err != nil {
		t.Fatalf("start: %v", err)
	}
	first, second := h.run("demo-ci-1111111-1"), h.run("demo-ci-1111111-2")
	if !first.CancelRequested || first.Cancellation.SupersededBy != second.ID.Name {
		t.Fatalf("the older attempt must be superseded by the newer one: %+v", first)
	}
	if second.CancelRequested || second.Phase != ci.Released {
		t.Fatalf("the newer attempt must run: %+v", second)
	}
}

func TestSupersedeWaitsForARivalOpeningItsReport(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: ci.PullRequest{Number: 5, HeadSHA: sha1}, State: "open"})
	h.evaluate(branchPR(sha1), EvalOptions{})
	first := h.run("demo-ci-1111111-1")
	// A second attempt of the commit, held, whose start has not opened its report yet.
	rival, err := h.runner.Create(ctx, h.runner.Spec(first.ID), 2)
	if err != nil {
		t.Fatal(err)
	}
	h.runner.Update(first.ID, func(r *ci.Run) { r.Phase = ci.Held })
	if err := h.svc.release(ctx, h.run(first.ID.Name)); err != nil {
		t.Fatal(err)
	}
	if got := h.run(first.ID.Name); got.Phase != ci.Held || got.WaitingFor != rival.ID.Name {
		t.Fatalf("a run must wait, held, for a rival still opening its report: %+v", got)
	}
}

const queueConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      push: {branches: [main]}
    concurrency: {group: "deploy", policy: POLICY}
`

func TestQueuePolicy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := strings.Replace(queueConfig, "POLICY", "queue", 1)
	h.files(sha1, cfg, ciRun)
	h.files(sha2, cfg, ciRun)
	h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
	h.evaluate(pushTrigger(sha2, "main"), EvalOptions{})
	first, second := h.run("demo-ci-1111111-1"), h.run("demo-ci-2222222-1")
	if first.Phase == ci.Held || second.Phase != ci.Held {
		t.Fatalf("one run at a time: first %s, second %s", first.Phase, second.Phase)
	}
	if spec := h.runner.Spec(second.ID); spec.Concurrency != (ci.Concurrency{Group: "deploy", Key: "deploy", Policy: ci.Queue}) {
		t.Fatalf("concurrency = %+v", spec.Concurrency)
	}
	// Still running: nothing to let go.
	if err := h.svc.ReleaseNext(ctx, first); err != nil {
		t.Fatal(err)
	}
	if h.run(second.ID.Name).Phase != ci.Held {
		t.Fatalf("the second run must wait while the first runs")
	}
	h.finish(first.ID.Name, ci.Success)
	if err := h.svc.ReleaseNext(ctx, h.run(first.ID.Name)); err != nil {
		t.Fatal(err)
	}
	if h.run(second.ID.Name).Phase != ci.Released {
		t.Fatalf("the second run must go when the first finishes")
	}
}

func TestLatestPolicy(t *testing.T) {
	h := newHarness(t)
	cfg := strings.Replace(queueConfig, "POLICY", "latest", 1)
	for _, sha := range []string{sha1, sha2, sha3} {
		h.files(sha, cfg, ciRun)
	}
	h.host.SetBranch(repo, "main", sha3)
	h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
	h.evaluate(pushTrigger(sha2, "main"), EvalOptions{})
	if h.run("demo-ci-2222222-1").Phase != ci.Held {
		t.Fatalf("the second run must wait")
	}
	h.evaluate(pushTrigger(sha3, "main"), EvalOptions{})
	second, third := h.run("demo-ci-2222222-1"), h.run("demo-ci-3333333-1")
	if !second.CancelRequested || second.Cancellation.SupersededBy != third.ID.Name {
		t.Fatalf("only the newest waiting run survives: %+v", second)
	}
	if third.Phase != ci.Held || third.CancelRequested {
		t.Fatalf("the newest run waits for the running one: %+v", third)
	}
	if h.run("demo-ci-1111111-1").CancelRequested {
		t.Fatalf("the running run is not touched")
	}
}

func TestCancelMergeGroup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	queue := "gh-readonly-queue/main/pr-5-" + baseSHA
	mg := ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventMergeGroup, Action: "checks_requested", InstallationID: installationID,
		Repository: repo, Revision: sha1, Ref: "refs/heads/" + queue, Branch: queue,
		MergeGroup: &ci.MergeGroup{HeadRef: "refs/heads/" + queue, HeadSHA: sha1, BaseRef: "refs/heads/main", BaseSHA: baseSHA},
	}
	h.evaluate(mg, EvalOptions{})
	if h.run("demo-ci-1111111-1").CancelRequested {
		t.Fatalf("the merge group's run must start")
	}
	h.svc.Handle(ctx, &ci.MergeGroupDestroyed{Trigger: mg, Reason: "dequeued"})
	if run := h.run("demo-ci-1111111-1"); !run.CancelRequested || run.Cancellation.Reason != "merge group destroyed (dequeued)" {
		t.Fatalf("the destroyed merge group's run must be cancelled: %+v", run)
	}
}

func TestResumeFinishesAnInterruptedStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	tr := pushTrigger(sha1, "main")
	tr.Pipeline = "ci"
	cfg, _ := h.svc.LoadConfig(ctx, tr)
	spec, refusal, _ := h.svc.prepare(ctx, h.host.Installation(installationID), tr, cfg.Pipeline("ci"))
	if refusal != nil {
		t.Fatal(refusal)
	}
	// What a replica left behind when it stopped right after creating the held run: GitHub already
	// has the report, which the run does not record.
	run, err := h.runner.Create(ctx, spec, 1)
	if err != nil {
		t.Fatal(err)
	}
	existing := h.host.AddReport(repo, ci.Report{Name: "ci", Revision: sha1, ExternalID: run.ID.String()})

	if err := h.svc.Resume(ctx, run); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := h.run(run.ID.Name); got.ReportID != existing || got.Phase != ci.Released {
		t.Fatalf("resumed run = %+v, want report %d and let go", got, existing)
	}
	if n := len(h.host.ReportsNamed("ci")); n != 1 {
		t.Fatalf("the existing report must be taken over, found %d", n)
	}
	if _, ok := h.runner.Token(run.ID); !ok {
		t.Fatalf("the missing token must be minted")
	}
	if err := h.svc.Resume(ctx, ci.Run{ID: run.ID}); err == nil {
		t.Fatalf("a run without its trigger cannot be resumed")
	}
}

func TestQueueHeadWaitsItsTurn(t *testing.T) {
	tests := []struct {
		name          string
		reportPending bool
		wantReleased  bool
	}{
		{name: "the next run of the queue goes when its turn comes", wantReleased: true},
		{name: "a run still opening its report is passed over", reportPending: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			cfg := strings.Replace(queueConfig, "POLICY", "queue", 1)
			h.files(sha1, cfg, ciRun)
			h.files(sha2, cfg, ciRun)
			h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
			h.evaluate(pushTrigger(sha2, "main"), EvalOptions{})
			second := h.run("demo-ci-2222222-1")
			if tt.reportPending {
				// The second run's start was cut short before its report was recorded.
				h.runner.Update(second.ID, func(r *ci.Run) { r.ReportID, r.Reported = 0, "" })
			}
			h.finish("demo-ci-1111111-1", ci.Failure)
			if err := h.svc.ReleaseNext(ctx, h.run("demo-ci-1111111-1")); err != nil {
				t.Fatal(err)
			}
			if got := h.run(second.ID.Name).Phase == ci.Released; got != tt.wantReleased {
				t.Fatalf("second run released = %v, want %v", got, tt.wantReleased)
			}
			// Resume opens what is missing and lets the run go.
			if err := h.svc.Resume(ctx, h.run(second.ID.Name)); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if got := h.run(second.ID.Name); got.Phase != ci.Released || got.ReportID == 0 {
				t.Fatalf("after Resume: %+v", got)
			}
		})
	}
}

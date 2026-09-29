package trigger

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"octomaton.dev/internal/adapters/github/githubtest"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/tekton"
)

func TestSupersedeCancelsOlderCommitsRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.files(sha2, ciConfig, ciRun)
	h.gh.SetPullRequest(fullName, githubtest.PullRequest{Number: 5, State: "open", HeadSHA: sha1, HeadRef: "feature", BaseRef: "main"})
	h.svc.Evaluate(ctx, trustedPR(sha1), EvalOptions{})
	first := h.run("demo-ci-1111111-1")
	if tekton.IsPending(first) || tekton.CancelRequested(first) {
		t.Fatalf("the first run must be released")
	}

	h.gh.SetPullRequest(fullName, githubtest.PullRequest{Number: 5, State: "open", HeadSHA: sha2, HeadRef: "feature", BaseRef: "main"})
	h.svc.Evaluate(ctx, trustedPR(sha2), EvalOptions{})
	first, second := h.run("demo-ci-1111111-1"), h.run("demo-ci-2222222-1")
	if !tekton.CancelRequested(first) || first.GetAnnotations()[tekton.AnnotationSupersededBy] != "demo-ci-2222222-1" {
		t.Fatalf("the older commit's run must be superseded: spec %v, annotations %v", first.Object["spec"], first.GetAnnotations())
	}
	if tekton.IsPending(second) || tekton.CancelRequested(second) {
		t.Fatalf("the newest commit's run must be released")
	}

	// A late delivery for the old commit must not stand the head's run down.
	late := trustedPR(sha1)
	late.DeliveryID = "late"
	h.svc.Evaluate(ctx, late, EvalOptions{})
	stale := h.run("demo-ci-1111111-2")
	if !tekton.CancelRequested(stale) || stale.GetAnnotations()[tekton.AnnotationSupersededBy] != SupersededByHead+sha2 {
		t.Fatalf("a run for a commit that is no longer the head must stand itself down: %v %v", stale.Object["spec"], stale.GetAnnotations())
	}
	if second = h.run("demo-ci-2222222-1"); tekton.CancelRequested(second) {
		t.Fatalf("the head's run must keep running")
	}
}

func TestSupersedeOfSameCommitKeepsTheNewestCheck(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.gh.SetPullRequest(fullName, githubtest.PullRequest{Number: 5, State: "open", HeadSHA: sha1})
	h.svc.Evaluate(ctx, trustedPR(sha1), EvalOptions{})
	// A re-run of the same commit is a new attempt with a newer check: it wins.
	gh := h.svc.GitHub.Installation(installationID)
	cfg, _ := h.svc.loadConfig(ctx, gh, trustedPR(sha1), false)
	c := trustedPR(sha1)
	c.Pipeline = "ci"
	if _, _, err := h.svc.start(ctx, gh, c, cfg.Pipeline("ci"), startOptions{Rerun: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	first, second := h.run("demo-ci-1111111-1"), h.run("demo-ci-1111111-2")
	if !tekton.CancelRequested(first) || first.GetAnnotations()[tekton.AnnotationSupersededBy] != second.GetName() {
		t.Fatalf("the older attempt must be superseded by the newer one")
	}
	if tekton.CancelRequested(second) || tekton.IsPending(second) {
		t.Fatalf("the newer attempt must run")
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
	for _, sha := range []string{sha1, sha2} {
		h.files(sha, cfg, ciRun)
	}
	h.svc.Evaluate(ctx, pushCtx(sha1, "main"), EvalOptions{})
	h.svc.Evaluate(ctx, pushCtx(sha2, "main"), EvalOptions{})
	first, second := h.run("demo-ci-1111111-1"), h.run("demo-ci-2222222-1")
	if tekton.IsPending(first) || !tekton.IsPending(second) {
		t.Fatalf("one run at a time: first pending=%v, second pending=%v", tekton.IsPending(first), tekton.IsPending(second))
	}
	if second.GetAnnotations()[tekton.AnnotationConcurrencyGroup] != "deploy" || second.GetAnnotations()[tekton.AnnotationConcurrencyPolicy] != "queue" {
		t.Fatalf("group annotations = %v", second.GetAnnotations())
	}
	// Still running: nothing to release.
	if err := h.svc.ReleaseNext(ctx, first); err != nil {
		t.Fatal(err)
	}
	if !tekton.IsPending(h.run("demo-ci-2222222-1")) {
		t.Fatalf("the second run must wait while the first runs")
	}
	h.finish(first.GetName(), "True", "Succeeded")
	if err := h.svc.ReleaseNext(ctx, h.run(first.GetName())); err != nil {
		t.Fatal(err)
	}
	if tekton.IsPending(h.run("demo-ci-2222222-1")) {
		t.Fatalf("the second run must be released when the first finishes")
	}
}

func TestLatestPolicy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := strings.Replace(queueConfig, "POLICY", "latest", 1)
	for _, sha := range []string{sha1, sha2, sha3} {
		h.files(sha, cfg, ciRun)
	}
	h.gh.SetBranch(fullName, "main", sha3)
	h.svc.Evaluate(ctx, pushCtx(sha1, "main"), EvalOptions{})
	h.svc.Evaluate(ctx, pushCtx(sha2, "main"), EvalOptions{})
	if !tekton.IsPending(h.run("demo-ci-2222222-1")) {
		t.Fatalf("the second run must wait")
	}
	h.svc.Evaluate(ctx, pushCtx(sha3, "main"), EvalOptions{})
	second, third := h.run("demo-ci-2222222-1"), h.run("demo-ci-3333333-1")
	if !tekton.CancelRequested(second) || second.GetAnnotations()[tekton.AnnotationSupersededBy] != third.GetName() {
		t.Fatalf("only the newest waiting run survives: second %v %v", second.Object["spec"], second.GetAnnotations())
	}
	if !tekton.IsPending(third) || tekton.CancelRequested(third) {
		t.Fatalf("the newest run waits for the running one")
	}
	if tekton.CancelRequested(h.run("demo-ci-1111111-1")) {
		t.Fatalf("the running run is not touched")
	}
}

func TestCancelMergeGroup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	c := checkrun.Context{
		Version: checkrun.ContextVersion, Event: checkrun.EventMergeGroup, Action: "checks_requested", InstallationID: installationID,
		Repository: repo(), Revision: sha1, Ref: "refs/heads/gh-readonly-queue/main/pr-5-" + baseSHA, Branch: "gh-readonly-queue/main/pr-5-" + baseSHA,
		MergeGroup: &checkrun.MergeGroup{HeadRef: "refs/heads/gh-readonly-queue/main/pr-5-" + baseSHA, HeadSHA: sha1, BaseRef: "refs/heads/main", BaseSHA: baseSHA},
	}
	h.svc.Evaluate(ctx, c, EvalOptions{})
	run := h.run("demo-ci-1111111-1")
	if tekton.CancelRequested(run) {
		t.Fatalf("the merge group run must start")
	}
	c.Action = "destroyed"
	h.svc.CancelMergeGroup(ctx, c, "dequeued")
	run = h.run("demo-ci-1111111-1")
	if !tekton.CancelRequested(run) || run.GetAnnotations()[tekton.AnnotationCancelReason] != "merge group destroyed (dequeued)" {
		t.Fatalf("the destroyed merge group's run must be cancelled: %v %v", run.Object["spec"], run.GetAnnotations())
	}
}

func TestResumeFinishesAnInterruptedStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	c := pushCtx(sha1, "main")
	c.Pipeline = "ci"
	gh := h.svc.GitHub.Installation(installationID)
	cfg, _ := h.svc.loadConfig(ctx, gh, c, false)
	p := cfg.Pipeline("ci")
	prep, problem := h.svc.prepare(ctx, gh, c, p)
	if problem != "" {
		t.Fatal(problem)
	}
	// What a replica left behind when it stopped right after creating the held run.
	name := tekton.RunName(repoName, "ci", sha1, 1)
	pr, err := tekton.Render(prep.pipelineRun, tekton.RenderInput{
		Namespace: namespace, Name: name, Params: prep.params, TokenWorkspace: p.TokenWorkspace(),
		Labels: runLabels(c, p, prep, startOptions{}), Annotations: runAnnotations(c, p, prep, 1), Held: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := h.runs.Create(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	// GitHub already has the check (the pod stopped before recording it).
	existing := h.gh.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci", HeadSHA: sha1, Status: "queued", ExternalID: namespace + "/" + name})

	if err := h.svc.Resume(ctx, created); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	run := h.run(name)
	if run.GetAnnotations()[tekton.AnnotationCheckRunID] != strconv.FormatInt(existing, 10) || tekton.IsPending(run) {
		t.Fatalf("resumed run: check %q (want %d), pending %v", run.GetAnnotations()[tekton.AnnotationCheckRunID], existing, tekton.IsPending(run))
	}
	if n := len(h.checks("ci")); n != 1 {
		t.Fatalf("the existing check must be taken over, found %d checks", n)
	}
	if s, _ := h.runs.TokenSecret(ctx, namespace, name); s == nil {
		t.Fatalf("the missing token Secret must be minted")
	}
}

func TestHeldRunInQueueIsReleasedByResumeWhenItsTurnComes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := strings.Replace(queueConfig, "POLICY", "queue", 1)
	h.files(sha1, cfg, ciRun)
	h.files(sha2, cfg, ciRun)
	h.svc.Evaluate(ctx, pushCtx(sha1, "main"), EvalOptions{})
	h.svc.Evaluate(ctx, pushCtx(sha2, "main"), EvalOptions{})
	h.finish("demo-ci-1111111-1", "False", "Failed")
	if err := h.svc.Resume(ctx, h.run("demo-ci-2222222-1")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if tekton.IsPending(h.run("demo-ci-2222222-1")) {
		t.Fatalf("Resume must release a queued run whose turn has come")
	}
}

func TestQueueSkipsRunsStillOpeningTheirCheck(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := strings.Replace(queueConfig, "POLICY", "queue", 1)
	h.files(sha1, cfg, ciRun)
	h.files(sha2, cfg, ciRun)
	h.svc.Evaluate(ctx, pushCtx(sha1, "main"), EvalOptions{})
	h.svc.Evaluate(ctx, pushCtx(sha2, "main"), EvalOptions{})
	// The second run's start was cut short before its check was recorded.
	if err := h.runs.Annotate(ctx, namespace, "demo-ci-2222222-1", map[string]*string{tekton.AnnotationCheckRunID: nil, tekton.AnnotationReported: nil}); err != nil {
		t.Fatal(err)
	}
	h.finish("demo-ci-1111111-1", "True", "Succeeded")
	if err := h.svc.ReleaseNext(ctx, h.run("demo-ci-1111111-1")); err != nil {
		t.Fatal(err)
	}
	if !tekton.IsPending(h.run("demo-ci-2222222-1")) {
		t.Fatalf("a run without a check must not be released by the queue")
	}
	if err := h.svc.Resume(ctx, h.run("demo-ci-2222222-1")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	run := h.run("demo-ci-2222222-1")
	if tekton.IsPending(run) || run.GetAnnotations()[tekton.AnnotationCheckRunID] == "" {
		t.Fatalf("Resume must open the check and release the run")
	}
}

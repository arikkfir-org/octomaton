package runs

import (
	"context"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// environmentConfig deploys an environment for each pull request that is not a draft, and tears it
// down when the pull request is closed or becomes a draft.
const environmentConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: environment
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {drafts: false}
  - name: teardown
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {types: [closed, converted_to_draft]}
    params:
      merged: "{{ .PullRequest.Merged }}"
`

// prEvent is pull request #5's action at sha, delivered as delivery.
func prEvent(sha, action, delivery string) ci.Trigger {
	t := branchPR(sha)
	t.Action, t.DeliveryID = action, delivery
	return t
}

// TestEndingsAreRunsOfTheirOwn: closing a pull request or making it a draft runs the pipelines that
// list it at the head commit, each event as a run of its own, even with drafts: false; no other run
// of the pull request supersedes such a run, and it supersedes none. Reopening the pull request, or
// marking it ready again, runs the pipeline that skips drafts again at the same commit.
func TestEndingsAreRunsOfTheirOwn(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, environmentConfig, ciRun)
	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: *branchPR(sha1).PullRequest, State: "open"})
	steps := []struct {
		trigger ci.Trigger
		draft   bool
		// the runs each pipeline has after the step, and which of them were cancelled
		environment, teardown int
		cancelled             []string
	}{
		{trigger: prEvent(sha1, "opened", "d1"), environment: 1},
		{trigger: prEvent(sha1, "converted_to_draft", "d2"), draft: true, environment: 1, teardown: 1},
		{trigger: prEvent(sha1, "converted_to_draft", "d2"), draft: true, environment: 1, teardown: 1}, // its redelivery
		{trigger: prEvent(sha1, "ready_for_review", "d3"), environment: 2, teardown: 1, cancelled: []string{"demo-environment-1111111-1"}},
		{trigger: func() ci.Trigger { t := prEvent(sha1, "closed", "d4"); t.PullRequest.Merged = true; return t }(), environment: 2, teardown: 2,
			cancelled: []string{"demo-environment-1111111-1"}},
		{trigger: prEvent(sha1, "reopened", "d5"), environment: 3, teardown: 2, cancelled: []string{"demo-environment-1111111-1", "demo-environment-1111111-2"}},
		{trigger: prEvent(sha1, "reopened", "d5"), environment: 3, teardown: 2, cancelled: []string{"demo-environment-1111111-1", "demo-environment-1111111-2"}},
	}
	for i, step := range steps {
		h.evaluate(step.trigger, EvalOptions{Draft: step.draft})
		counts := map[string]int{}
		cancelled := map[string]bool{}
		for _, r := range h.runner.Runs() {
			counts[r.Trigger.Pipeline]++
			if r.CancelRequested {
				cancelled[r.ID.Name] = true
			}
			if r.Trigger.Pipeline == "teardown" && r.Group != "" {
				t.Fatalf("step %d: a teardown must be in no concurrency group by default: %+v", i, r)
			}
		}
		if counts["environment"] != step.environment || counts["teardown"] != step.teardown {
			t.Fatalf("step %d (%s): runs = %v, want environment %d, teardown %d", i, step.trigger.Action, counts, step.environment, step.teardown)
		}
		if len(cancelled) != len(step.cancelled) {
			t.Fatalf("step %d (%s): cancelled = %v, want %v", i, step.trigger.Action, cancelled, step.cancelled)
		}
		for _, name := range step.cancelled {
			if !cancelled[name] {
				t.Fatalf("step %d (%s): %s must be cancelled (cancelled: %v)", i, step.trigger.Action, name, cancelled)
			}
		}
	}
	first, second := h.run("demo-teardown-1111111-1"), h.run("demo-teardown-1111111-2")
	if first.Trigger.Action != "converted_to_draft" || second.Trigger.Action != "closed" || second.Phase != ci.Released {
		t.Fatalf("each ending runs: %+v, %+v", first, second)
	}
	if got := h.runner.Spec(second.ID).Params["merged"]; got != "true" {
		t.Fatalf("merged = %q, want true", got)
	}
	if got := h.runner.Spec(first.ID).Params["merged"]; got != "false" {
		t.Fatalf("merged = %q, want false", got)
	}
}

// TestEndingRunsAtTheCommitItEnded: an ending's run is at the commit the event names, even when the
// pull request has moved on since: it does not stand down for the newer commit.
func TestEndingRunsAtTheCommitItEnded(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, environmentConfig, ciRun)
	moved := branchPR(sha2).PullRequest
	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: *moved, State: "open", Draft: true})
	h.evaluate(prEvent(sha1, "converted_to_draft", "d1"), EvalOptions{Draft: true})
	if run := h.run("demo-teardown-1111111-1"); run.CancelRequested || run.Phase != ci.Released {
		t.Fatalf("the teardown must run at the commit it ended: %+v", run)
	}
}

// TestEndingsFollowTheirPipelinesGroup: a pipeline that names a concurrency group gets it for its
// endings too, so a teardown can wait for the deployment its group runs.
func TestEndingsFollowTheirPipelinesGroup(t *testing.T) {
	const cfg = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: environment
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {drafts: false}
    concurrency: {group: "environment-{{ .PullRequest.Number }}", policy: latest}
  - name: teardown
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {types: [closed]}
    concurrency: {group: "environment-{{ .PullRequest.Number }}", policy: latest}
`
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, cfg, ciRun)
	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: *branchPR(sha1).PullRequest, State: "closed"})
	h.evaluate(prEvent(sha1, "opened", "d1"), EvalOptions{})
	deploy := h.run("demo-environment-1111111-1")
	h.runner.Start(deploy.ID, h.clock())
	h.evaluate(prEvent(sha1, "closed", "d2"), EvalOptions{})
	teardown := h.run("demo-teardown-1111111-1")
	if teardown.Phase != ci.Held || teardown.CancelRequested || h.run(deploy.ID.Name).CancelRequested {
		t.Fatalf("the teardown must wait for the running deployment: %+v", teardown)
	}
	h.finish(deploy.ID.Name, ci.Success)
	if err := h.svc.ReleaseNext(ctx, h.run(deploy.ID.Name)); err != nil {
		t.Fatal(err)
	}
	if run := h.run(teardown.ID.Name); run.Phase != ci.Released {
		t.Fatalf("the teardown must go once the deployment finished: %+v", run)
	}
}

// TestDestroyedMergeGroupRunsItsPipelines: a destroyed merge group cancels its runs, then runs the
// pipelines that list destroyed at its head, as a run of its own, which its redelivery finds and
// does not cancel.
func TestDestroyedMergeGroupRunsItsPipelines(t *testing.T) {
	const cfg = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on: {merge_group: {}}
  - name: teardown
    pipelineRun: .tekton/ci.yaml
    on: {merge_group: {types: [destroyed]}}
    params:
      number: "{{ .MergeGroup.PullRequestNumber }}"
      reason: "{{ .MergeGroup.Reason }}"
`
	queue := "gh-readonly-queue/main/pr-5-" + baseSHA
	mg := ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventMergeGroup, Action: "checks_requested", DeliveryID: "q1", InstallationID: installationID,
		Repository: repo, Revision: sha1, Ref: "refs/heads/" + queue, Branch: queue,
		MergeGroup: &ci.MergeGroup{HeadRef: "refs/heads/" + queue, HeadSHA: sha1, BaseRef: "refs/heads/main", BaseSHA: baseSHA, PullRequestNumber: 5},
	}
	destroyed := mg
	destroyed.Action, destroyed.DeliveryID = "destroyed", "q2"
	destroyed.MergeGroup = &ci.MergeGroup{HeadRef: "refs/heads/" + queue, HeadSHA: sha1, BaseRef: "refs/heads/main", BaseSHA: baseSHA, PullRequestNumber: 5, Reason: "dequeued"}
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, cfg, ciRun)
	h.evaluate(mg, EvalOptions{})
	if len(h.runner.Runs()) != 1 {
		t.Fatalf("only ci runs for a merge group's checks: %+v", h.runner.Runs())
	}
	for range 2 { // the event and its redelivery
		if err := h.svc.Handle(ctx, &ci.MergeGroupDestroyed{Trigger: destroyed}); err != nil {
			t.Fatal(err)
		}
	}
	if run := h.run("demo-ci-1111111-1"); !run.CancelRequested || run.Cancellation.Reason != "merge group destroyed (dequeued)" {
		t.Fatalf("the merge group's checks must be cancelled: %+v", run)
	}
	if n := len(h.runner.Runs()); n != 2 {
		t.Fatalf("runs = %d, want ci's and one teardown", n)
	}
	teardown := h.run("demo-teardown-1111111-1")
	if teardown.CancelRequested || teardown.Phase != ci.Released || teardown.Trigger.Action != "destroyed" {
		t.Fatalf("the teardown must run: %+v", teardown)
	}
	if p := h.runner.Spec(teardown.ID).Params; p["number"] != "5" || p["reason"] != "dequeued" {
		t.Fatalf("params = %v", p)
	}
}

// TestEndingsReportNoInvalidConfiguration: an ending, like a label, is no reason to report an invalid
// configuration its commit's checks already reported.
func TestEndingsReportNoInvalidConfiguration(t *testing.T) {
	const invalid = "apiVersion: octomaton.dev/v1\npipelines:\n  - {bogus: 1}\n"
	queue := "gh-readonly-queue/main/pr-5-" + baseSHA
	destroyed := ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventMergeGroup, Action: "destroyed", InstallationID: installationID,
		Repository: repo, Revision: sha1, Ref: "refs/heads/" + queue, Branch: queue,
		MergeGroup: &ci.MergeGroup{HeadRef: "refs/heads/" + queue, HeadSHA: sha1, BaseRef: "refs/heads/main", BaseSHA: baseSHA, Reason: "merged"},
	}
	events := map[string]ci.Event{
		"closed":             &ci.TriggerEvent{Trigger: prEvent(sha1, "closed", "d1")},
		"converted_to_draft": &ci.TriggerEvent{Trigger: prEvent(sha1, "converted_to_draft", "d1"), Draft: true},
		"destroyed":          &ci.MergeGroupDestroyed{Trigger: destroyed},
	}
	for name, ev := range events {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, invalid, "")
			if err := h.svc.Handle(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
			if reports := h.host.Reports(); len(reports) != 0 {
				t.Fatalf("reports = %+v, want none", reports)
			}
		})
	}
}

// TestAnEndingIsNoCommitsRun: an event for a commit and head never takes an ending's run there for
// its own.
func TestAnEndingIsNoCommitsRun(t *testing.T) {
	const cfg = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      push: {}
      pull_request: {types: [converted_to_draft]}
`
	h := newHarness(t)
	h.files(sha1, cfg, ciRun)
	h.evaluate(prEvent(sha1, "converted_to_draft", "d1"), EvalOptions{Draft: true})
	h.evaluate(pushTrigger(sha1, "feature"), EvalOptions{})
	if n := len(h.runner.Runs()); n != 2 {
		t.Fatalf("runs = %d, want the ending's and the push's", n)
	}
}

package runs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/reports"
)

// rerunConfig runs "ci" on pull requests into main, the merge queue and pushes to main, with task
// checks, and reruns a run that lost its pods to disruptions twice at most.
const rerunConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {branches: [main]}
      merge_group: {}
      push: {branches: [main]}
    taskChecks: true
    disruptions: {reruns: 2}
`

var nodeShutdown = ci.Disruption{Node: "spot-node-1", Reason: "TerminationByKubelet", Message: "Pod was terminated in response to imminent node shutdown."}

// reporter is the leader's reports service, wired to h's runs as the server wires it.
func (h *harness) reporter() *reports.Service {
	return &reports.Service{
		Host: h.host, Runner: h.runner, Logger: h.svc.Logger, Now: h.clock,
		Resume: h.svc.Resume, ReleaseNext: h.svc.ReleaseNext, RerunLost: h.svc.RerunLost,
	}
}

// reconcile hands a run to the reports service, as the watch does.
func (h *harness) reconcile(rep *reports.Service, name string) {
	h.t.Helper()
	if _, err := rep.Reconcile(context.Background(), h.run(name)); err != nil {
		h.t.Fatalf("Reconcile %s: %v", name, err)
	}
}

// lose has a running run's build task fail after losing its pod (or by itself, when d is nil) and
// test end as given.
func (h *harness) lose(name string, build *ci.Disruption, test ci.TaskState) {
	h.runner.SetDetails(h.run(name).ID, ci.Details{Tasks: []ci.Task{
		{Name: "build", State: ci.TaskFailed, Started: h.clock(), Finished: h.clock(), Message: `"step-b" exited with code 143`, Disruption: build},
		{Name: "test", State: test, Started: h.clock(), Finished: h.clock()},
	}})
}

func TestARunThatLostItsPodsIsRerun(t *testing.T) {
	h := newHarness(t)
	rep := h.reporter()
	h.files(sha1, rerunConfig, ciRun)
	h.host.SetBranch(repo, "main", sha1)
	h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
	first := h.run("demo-ci-1111111-1")
	if first.Reruns == nil || first.Reruns.Left != 2 || first.Reruns.Of != "" {
		t.Fatalf("a first run reruns as its pipeline says: %+v", first.Reruns)
	}
	h.runner.Start(first.ID, h.clock())
	h.reconcile(rep, first.ID.Name)
	h.lose(first.ID.Name, &nodeShutdown, ci.TaskSucceeded)
	h.reconcile(rep, first.ID.Name)
	build, test := h.onlyReport("ci / build"), h.onlyReport("ci / test")
	if build.Status == ci.StatusCompleted {
		t.Fatalf("the check of a task that lost its pod must stay open while its run may rerun: %+v", build.Report)
	}
	if test.Conclusion != ci.Success {
		t.Fatalf("the check of a task that passed concludes: %+v", test.Report)
	}

	h.finish(first.ID.Name, ci.Failure)
	h.reconcile(rep, first.ID.Name)
	second := h.run("demo-ci-1111111-2")
	if !h.run(first.ID.Name).Done {
		t.Fatalf("the rerun run is let go")
	}
	if second.Reruns == nil || second.Reruns.Left != 1 || second.Reruns.Of != first.ID.Name || len(second.Reruns.Lost) != 1 ||
		second.Reruns.Lost[0].Attempt != 1 || second.Reruns.Lost[0].Tasks[0].Task != "build" {
		t.Fatalf("the rerun records what it reruns: %+v", second.Reruns)
	}
	if second.Phase != ci.Released || second.ReportID != first.ReportID {
		t.Fatalf("the rerun goes, on its first attempt's report: %+v", second)
	}
	check := h.onlyReport("ci")
	if check.Status != ci.StatusInProgress || check.ExternalID != second.ID.String() {
		t.Fatalf("the check stays in progress, now the rerun's: %+v", check.Report)
	}
	mustContain(t, check.Summary, "Attempt 1 (`demo-ci-1111111-1`): `build` on node `spot-node-1` (TerminationByKubelet)")
	if got := h.onlyReport("ci / build"); got.ID != build.ID || got.Status != ci.StatusInProgress || got.ExternalID != second.ID.String() {
		t.Fatalf("an open task check is taken over: %+v", got.Report)
	}
	tests := h.host.ReportsNamed("ci / test")
	if len(tests) != 2 || tests[0].Conclusion != ci.Success || tests[1].Status != ci.StatusQueued || second.TaskReportIDs["test"] != tests[1].ID {
		t.Fatalf("a concluded task check is opened afresh: %+v", tests)
	}

	// The rerun loses its pods too, then its own rerun passes.
	h.runner.Start(second.ID, h.clock())
	h.reconcile(rep, second.ID.Name)
	h.lose(second.ID.Name, &nodeShutdown, ci.TaskSucceeded)
	h.finish(second.ID.Name, ci.Failure)
	h.reconcile(rep, second.ID.Name)
	third := h.run("demo-ci-1111111-3")
	if third.Reruns.Left != 0 || len(third.Reruns.Lost) != 2 {
		t.Fatalf("the second rerun: %+v", third.Reruns)
	}
	h.runner.Start(third.ID, h.clock())
	h.reconcile(rep, third.ID.Name)
	h.runner.SetDetails(third.ID, ci.Details{Tasks: []ci.Task{{Name: "build", State: ci.TaskSucceeded}, {Name: "test", State: ci.TaskSucceeded}}})
	h.finish(third.ID.Name, ci.Success)
	h.reconcile(rep, third.ID.Name)
	if check := h.onlyReport("ci"); check.Conclusion != ci.Success {
		t.Fatalf("the check concludes with the run that ends it: %+v", check.Report)
	}
	mustContain(t, h.onlyReport("ci").Summary, "Attempt 1 (`demo-ci-1111111-1`)", "Attempt 2 (`demo-ci-1111111-2`)")
	if n := len(h.runner.Runs()); n != 3 {
		t.Fatalf("runs = %d, want 3", n)
	}
}

// TestRerunsAreCapped concludes the check of a run that lost its pods once its pipeline's reruns are
// used up.
func TestRerunsAreCapped(t *testing.T) {
	h := newHarness(t)
	rep := h.reporter()
	h.files(sha1, strings.Replace(rerunConfig, "reruns: 2", "reruns: 1", 1), ciRun)
	h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
	for attempt, name := range []string{"demo-ci-1111111-1", "demo-ci-1111111-2"} {
		h.runner.Start(h.run(name).ID, h.clock())
		h.reconcile(rep, name)
		h.lose(name, &nodeShutdown, ci.TaskSucceeded)
		h.reconcile(rep, name)
		if build := h.onlyReport("ci / build"); (build.Status == ci.StatusCompleted) != (attempt == 1) {
			t.Fatalf("attempt %d: the lost task's check: %+v", attempt+1, build.Report)
		}
		h.finish(name, ci.Failure)
		h.reconcile(rep, name)
	}
	if n := len(h.runner.Runs()); n != 2 {
		t.Fatalf("runs = %d, want 2: one rerun at most", n)
	}
	check := h.onlyReport("ci")
	if check.Conclusion != ci.Failure {
		t.Fatalf("the check fails once no rerun is left: %+v", check.Report)
	}
	mustContain(t, check.Summary, "Lost their pods to disruptions: `build` on node `spot-node-1` (TerminationByKubelet)", "Not rerun: its pipeline's `disruptions.reruns` are used up")
	mustContain(t, h.onlyReport("ci / build").Summary, "Its pod was taken away on node `spot-node-1` (TerminationByKubelet)")
}

// TestOnlyARunThatFailedOnlyByLosingPodsIsRerun: a task that failed by itself fails the run, even
// beside one that lost its pod.
func TestOnlyARunThatFailedOnlyByLosingPodsIsRerun(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		build   *ci.Disruption
		test    ci.TaskState
		outcome ci.Conclusion
		tasks   bool
	}{
		{name: "a task failed by itself beside a lost one", config: rerunConfig, build: &nodeShutdown, test: ci.TaskFailed, outcome: ci.Failure, tasks: true},
		{name: "a task failed by itself", config: rerunConfig, test: ci.TaskSucceeded, outcome: ci.Failure, tasks: true},
		{name: "a task timed out beside a lost one", config: rerunConfig, build: &nodeShutdown, test: ci.TaskTimedOut, outcome: ci.Failure, tasks: true},
		{name: "the run failed before any task", config: rerunConfig, outcome: ci.Failure},
		{name: "the run timed out", config: rerunConfig, build: &nodeShutdown, test: ci.TaskSucceeded, outcome: ci.TimedOut, tasks: true},
		{name: "the pipeline does not rerun", config: strings.Replace(rerunConfig, "    disruptions: {reruns: 2}\n", "", 1), build: &nodeShutdown, test: ci.TaskSucceeded, outcome: ci.Failure, tasks: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			rep := h.reporter()
			h.files(sha1, tt.config, ciRun)
			h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
			name := "demo-ci-1111111-1"
			h.runner.Start(h.run(name).ID, h.clock())
			h.reconcile(rep, name)
			if tt.tasks {
				h.lose(name, tt.build, tt.test)
			}
			h.finish(name, tt.outcome)
			h.reconcile(rep, name)
			if n := len(h.runner.Runs()); n != 1 {
				t.Fatalf("runs = %d: the run must not be rerun", n)
			}
			if check := h.onlyReport("ci"); check.Conclusion != tt.outcome {
				t.Fatalf("the check concludes as the run ended: %+v", check.Report)
			}
		})
	}
}

// TestARerunFollowsItsPipelinesConcurrency: under supersede, the rerun of a commit that is no longer
// the head of its pull request stands itself down, so it never acts on the newer commit's behalf.
func TestARerunFollowsItsPipelinesConcurrency(t *testing.T) {
	h := newHarness(t)
	rep := h.reporter()
	h.files(sha1, rerunConfig, ciRun)
	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: *branchPR(sha1).PullRequest, State: "open"})
	h.evaluate(branchPR(sha1), EvalOptions{})
	name := "demo-ci-1111111-1"
	h.runner.Start(h.run(name).ID, h.clock())
	h.reconcile(rep, name)
	h.lose(name, &nodeShutdown, ci.TaskSucceeded)

	h.host.SetPullRequest(repo, ci.PullRequestState{PullRequest: *branchPR(sha2).PullRequest, State: "open"})
	h.finish(name, ci.Failure)
	h.reconcile(rep, name)
	rerun := h.run("demo-ci-1111111-2")
	if !rerun.CancelRequested || rerun.Cancellation.NewerCommit != sha2 {
		t.Fatalf("the rerun of an older commit must stand itself down: %+v", rerun)
	}
	h.runner.Finish(rerun.ID, ci.Outcome{Conclusion: ci.Cancelled}, h.clock())
	h.reconcile(rep, rerun.ID.Name)
	if check := h.onlyReport("ci"); check.Conclusion != ci.Skipped || !strings.Contains(check.Summary, "Superseded by a newer commit") {
		t.Fatalf("its check concludes superseded: %+v", check.Report)
	}
}

// TestRerunLostRefusals: when the run can't be rerun, its check concludes as the run ended, saying
// why.
func TestRerunLostRefusals(t *testing.T) {
	queue := "gh-readonly-queue/main/pr-5-" + baseSHA
	mergeGroup := ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventMergeGroup, Action: "checks_requested", InstallationID: installationID,
		Repository: repo, Revision: sha1, Ref: "refs/heads/" + queue, Branch: queue,
		MergeGroup: &ci.MergeGroup{HeadRef: "refs/heads/" + queue, HeadSHA: sha1, BaseRef: "refs/heads/main", BaseSHA: baseSHA},
	}
	tests := []struct {
		name    string
		trigger ci.Trigger
		change  func(h *harness)
		want    string
		reruns  bool
	}{
		{name: "a live merge group", trigger: mergeGroup, change: func(h *harness) { h.host.SetBranch(repo, queue, sha1) }, reruns: true},
		{name: "a merge group that is gone", trigger: mergeGroup, want: "Not rerun: its merge group is gone."},
		{name: "a pipeline that is gone", trigger: pushTrigger(sha1, "main"), change: func(h *harness) {
			h.files(sha1, strings.Replace(rerunConfig, "name: ci", "name: build", 1), ciRun)
		}, want: "Not rerun: no valid configuration defines pipeline `ci` anymore."},
		{name: "a pipeline that won't start", trigger: pushTrigger(sha1, "main"), change: func(h *harness) {
			h.runner.CheckError = func(ci.RunSpec) error {
				return &ci.Refusal{Title: "Could not start the pipeline", Reason: "repository not onboarded"}
			}
		}, want: "Not rerun: repository not onboarded."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			rep := h.reporter()
			h.files(sha1, rerunConfig, ciRun)
			h.evaluate(tt.trigger, EvalOptions{})
			name := "demo-ci-1111111-1"
			h.runner.Start(h.run(name).ID, h.clock())
			h.reconcile(rep, name)
			h.lose(name, &nodeShutdown, ci.TaskSucceeded)
			h.finish(name, ci.Failure)
			if tt.change != nil {
				tt.change(h)
			}
			h.reconcile(rep, name)
			if rerun := len(h.runner.Runs()) == 2; rerun != tt.reruns {
				t.Fatalf("rerun = %v, want %v", rerun, tt.reruns)
			}
			if tt.reruns {
				return
			}
			check := h.onlyReport("ci")
			if check.Conclusion != ci.Failure {
				t.Fatalf("the check fails: %+v", check.Report)
			}
			mustContain(t, check.Summary, tt.want)
		})
	}
}

// TestRerunLostOnce: a rerun that was created, but whose run was not let go (a restart, a failed
// call), is found again rather than created twice.
func TestRerunLostOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, rerunConfig, ciRun)
	h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
	first := h.run("demo-ci-1111111-1")
	lost := []ci.Disruption{{Task: "build", Reason: "DeletionByPodGC"}}
	a, err := h.svc.RerunLost(ctx, first, lost)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.svc.RerunLost(ctx, first, lost)
	if err != nil || b.ID != a.ID {
		t.Fatalf("a second call finds the rerun: %+v, %v (first %s)", b.ID, err, a.ID)
	}
	if n := len(h.runner.Runs()); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}

	exhausted := h.run("demo-ci-1111111-2")
	exhausted.Reruns = &ci.Reruns{Left: 0, Of: first.ID.Name}
	var refusal *ci.Refusal
	if _, err := h.svc.RerunLost(ctx, exhausted, lost); !errors.As(err, &refusal) {
		t.Fatalf("a run with no reruns left is not rerun: %v", err)
	}
}

// TestARerunThatCannotStartFailsItsChecks: a rerun whose start fails after it exists fails the
// check it took over and the task checks still open, and lets its first attempt go.
func TestARerunThatCannotStartFailsItsChecks(t *testing.T) {
	h := newHarness(t)
	rep := h.reporter()
	h.files(sha1, rerunConfig, ciRun)
	h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
	name := "demo-ci-1111111-1"
	h.runner.Start(h.run(name).ID, h.clock())
	h.reconcile(rep, name)
	h.lose(name, &nodeShutdown, ci.TaskSucceeded)
	h.finish(name, ci.Failure)
	h.runner.Fail("Release", errors.New("connection refused"))
	if _, err := rep.Reconcile(context.Background(), h.run(name)); err == nil {
		t.Fatalf("a rerun that could not start is an error, retried")
	}
	h.runner.Fail("Release", nil)
	h.reconcile(rep, name)
	if !h.run(name).Done || len(h.runner.Runs()) != 2 {
		t.Fatalf("the retry finds the rerun and lets the first attempt go")
	}
	if check := h.onlyReport("ci"); check.Conclusion != ci.Failure || check.Title != "The run could not be started" {
		t.Fatalf("the check: %+v", check.Report)
	}
	if build := h.onlyReport("ci / build"); build.Conclusion != ci.Cancelled || build.Title != "Not run" {
		t.Fatalf("an open task check concludes: %+v", build.Report)
	}
}

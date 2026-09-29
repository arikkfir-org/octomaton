package reports

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/ci/citest"
)

const sha = "1111111111111111111111111111111111111111"

var repo = ci.Repository{ID: 1, Owner: "octo-org", Name: "demo", FullName: "octo-org/demo"}

type harness struct {
	t        *testing.T
	host     *citest.Host
	runner   *citest.Runner
	svc      *Service
	mu       sync.Mutex
	now      time.Time
	resumed  []string
	released []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, now: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	h.host = citest.NewHost(h.clock)
	h.runner = citest.NewRunner(h.clock)
	h.svc = &Service{
		Host: h.host, Runner: h.runner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: h.clock,
		Resume: func(_ context.Context, run ci.Run) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.resumed = append(h.resumed, run.ID.Name)
			return nil
		},
		ReleaseNext: func(_ context.Context, run ci.Run) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.released = append(h.released, run.ID.Name)
			return nil
		},
	}
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func pushTrigger() ci.Trigger {
	return ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventPush, InstallationID: 7, Repository: repo, Revision: sha,
		Ref: "refs/heads/main", Branch: "main", Sender: "alice", Pipeline: "ci",
	}
}

// newRun creates a run whose start opened its report, then lets change adjust it.
func (h *harness) newRun(t ci.Trigger, change func(*ci.Run)) (ci.Run, ci.ReportID) {
	h.t.Helper()
	ctx := context.Background()
	run, err := h.runner.Create(ctx, ci.RunSpec{Trigger: t}, 1)
	for attempt := 2; errors.Is(err, ci.ErrExists); attempt++ {
		run, err = h.runner.Create(ctx, ci.RunSpec{Trigger: t}, attempt)
	}
	if err != nil {
		h.t.Fatal(err)
	}
	id := h.host.AddReport(repo, ci.Report{Name: t.Pipeline, Revision: t.Revision, Status: ci.StatusQueued, Title: "Queued", Trigger: &t})
	if err := h.runner.Record(ctx, run.ID, ci.Record{ReportID: &id, Reported: new(ci.ReportedQueued)}); err != nil {
		h.t.Fatal(err)
	}
	h.runner.Update(run.ID, func(r *ci.Run) {
		r.Tasks = []string{"build", "test"}
		r.Created = h.clock().Add(-time.Minute)
		if change != nil {
			change(r)
		}
	})
	return h.get(run.ID), id
}

func (h *harness) get(id ci.RunID) ci.Run {
	h.t.Helper()
	run, err := h.runner.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return run
}

func (h *harness) reconcile(id ci.RunID) time.Duration {
	h.t.Helper()
	again, err := h.svc.Reconcile(context.Background(), h.get(id))
	if err != nil {
		h.t.Fatalf("Reconcile(%s): %v", id, err)
	}
	return again
}

func (h *harness) report(id ci.ReportID) citest.Report {
	h.t.Helper()
	r, ok := h.host.Report(id)
	if !ok {
		h.t.Fatalf("report %d not found", id)
	}
	return r
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("%q does not contain %q", got, w)
		}
	}
}

func finished(o ci.Outcome, start, end time.Time) func(*ci.Run) {
	return func(r *ci.Run) { r.Phase, r.Outcome, r.Started, r.Finished = ci.Finished, o, start, end }
}

func running(start time.Time) func(*ci.Run) {
	return func(r *ci.Run) { r.Phase, r.Started = ci.Running, start }
}

func TestHeldRuns(t *testing.T) {
	tests := []struct {
		name        string
		heldFor     time.Duration
		deleting    bool
		wantAgain   time.Duration
		wantResumed bool
	}{
		{name: "a run held for a minute is looked at again later", heldFor: time.Minute, wantAgain: 4 * time.Minute},
		{name: "a run held too long is resumed", heldFor: 6 * time.Minute, wantAgain: defaultHeldTooLong, wantResumed: true},
		{name: "a run being deleted is left alone", heldFor: time.Hour, deleting: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			run, _ := h.newRun(pushTrigger(), func(r *ci.Run) { r.Phase, r.Created, r.Deleting = ci.Held, h.clock().Add(-tt.heldFor), tt.deleting })
			if again := h.reconcile(run.ID); again != tt.wantAgain || (len(h.resumed) == 1) != tt.wantResumed {
				t.Fatalf("again = %v, resumed %v; want %v, resumed %v", again, h.resumed, tt.wantAgain, tt.wantResumed)
			}
		})
	}
}

func TestInProgressAndProgress(t *testing.T) {
	h := newHarness(t)
	start := h.clock().Add(-90 * time.Second)
	run, id := h.newRun(pushTrigger(), running(start))

	h.reconcile(run.ID)
	r := h.report(id)
	if r.Status != ci.StatusInProgress || !r.Started.Equal(start) || r.Title != "Running" || r.Trigger == nil {
		t.Fatalf("report after the start = %+v", r)
	}
	if h.get(run.ID).Reported != ci.ReportedInProgress {
		t.Fatalf("what was reported must advance to in_progress")
	}

	h.runner.SetDetails(run.ID, ci.Details{Tasks: []ci.Task{
		{Name: "build", State: ci.TaskSucceeded, Started: start, Finished: start.Add(30 * time.Second)},
		{Name: "test", State: ci.TaskRunning, Started: start.Add(31 * time.Second)},
	}})
	h.reconcile(run.ID)
	r = h.report(id)
	if r.Title != "1 of 2 · test · 1m30s" {
		t.Fatalf("progress title = %q", r.Title)
	}
	mustContain(t, r.Summary, "| `build` | ✅ Succeeded | 30s |", "| `test` | ⏳ Running |  |",
		"**PipelineRun:** [`ci-demo/demo-ci-1111111-1`](https://runs.example/ci-demo/demo-ci-1111111-1)", "**Trigger:** Push to `main`")
	updates := r.Updates
	h.advance(10 * time.Second)
	h.reconcile(run.ID)
	if h.report(id).Updates != updates {
		t.Fatalf("an unchanged table must not be written again")
	}
}

func TestFinishSuccess(t *testing.T) {
	h := newHarness(t)
	start := h.clock().Add(-10 * time.Minute)
	run, id := h.newRun(pushTrigger(), finished(ci.Outcome{Conclusion: ci.Success}, start, start.Add(3*time.Minute+12*time.Second)))
	h.runner.SetDetails(run.ID, ci.Details{Tasks: []ci.Task{
		{Name: "build", State: ci.TaskSucceeded, Started: start, Finished: start.Add(time.Minute)},
		{Name: "test", State: ci.TaskSucceeded, Started: start.Add(time.Minute), Finished: start.Add(3 * time.Minute)},
		{Name: "deploy", State: ci.TaskSkipped, Note: "When Expressions evaluated to false"},
	}})

	h.reconcile(run.ID)
	r := h.report(id)
	if r.Status != ci.StatusCompleted || r.Conclusion != ci.Success || r.Title != "Succeeded in 3m12s" || !r.Completed.Equal(start.Add(3*time.Minute+12*time.Second)) {
		t.Fatalf("report = %+v", r)
	}
	mustContain(t, r.Summary, "**Trigger:** Push to `main`", "| `build` | ✅ Succeeded | 1m0s |", "| `test` | ✅ Succeeded | 2m0s |",
		"| `deploy` | ⬜ Skipped (When Expressions evaluated to false) |")
	if got := h.get(run.ID); !got.Done || got.Reported != ci.ReportedCompleted {
		t.Fatalf("a finished run is let go: %+v", got)
	}
	if !slices.Equal(h.released, []string{run.ID.Name}) {
		t.Fatalf("a finished run releases the next queued run: %v", h.released)
	}
	updates := r.Updates
	h.reconcile(run.ID)
	if h.report(id).Updates != updates {
		t.Fatalf("a finished run is reported once")
	}
}

func TestFinishFailureIncludesLogs(t *testing.T) {
	h := newHarness(t)
	start := h.clock().Add(-time.Hour)
	o := ci.Outcome{Conclusion: ci.Failure, Message: "Tasks Completed: 2 (Failed: 1, Cancelled 0), Skipped: 0"}
	run, id := h.newRun(pushTrigger(), finished(o, start, start.Add(time.Minute)))
	h.runner.SetDetails(run.ID, ci.Details{Tasks: []ci.Task{
		{Name: "build", State: ci.TaskFailed, Started: start, Finished: start.Add(time.Minute), Message: `"step-compile" exited with code 2`,
			FailedSteps: []ci.Step{{Name: "compile", ExitCode: 2, Logs: "pod/step-compile"}, {Name: "lint", ExitCode: 1}}},
		{Name: "test", State: ci.TaskPending},
	}})
	h.runner.SetLogs("pod/step-compile", "fake logs\n")

	h.reconcile(run.ID)
	r := h.report(id)
	if r.Conclusion != ci.Failure || r.Title != "Failed after 1m0s" || r.Trigger == nil {
		t.Fatalf("report = %+v", r)
	}
	mustContain(t, r.Summary, "Failed: `build`.", "> Tasks Completed: 2 (Failed: 1, Cancelled 0), Skipped: 0", "| `test` | ⬜ Pending |")
	mustContain(t, r.Text, "### build › compile (exit code 2)", "```text\nfake logs\n```", "### build › lint (exit code 1)", "_Logs unavailable: no logs for step lint._")
}

func TestFinishOutcomes(t *testing.T) {
	start := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		outcome        ci.Outcome
		end            time.Time
		cancellation   ci.Cancellation
		results        map[string]string
		wantConclusion ci.Conclusion
		wantTitle      string
		wantSummary    []string
	}{
		{
			name: "superseded by a newer commit", outcome: ci.Outcome{Conclusion: ci.Cancelled}, end: start.Add(time.Minute),
			cancellation:   ci.Cancellation{NewerCommit: "2222222222222222222222222222222222222222"},
			wantConclusion: ci.Skipped, wantTitle: "Superseded", wantSummary: []string{"Superseded by a newer commit, `2222222`."},
		},
		{
			name: "superseded by a newer run", outcome: ci.Outcome{Conclusion: ci.Cancelled}, end: start.Add(time.Minute),
			cancellation:   ci.Cancellation{SupersededBy: "demo-ci-2222222-1"},
			wantConclusion: ci.Skipped, wantTitle: "Superseded",
			wantSummary: []string{"Superseded by a newer run, [`demo-ci-2222222-1`](https://runs.example/ci-demo/demo-ci-2222222-1)."},
		},
		{
			name: "a success before the supersede stands", outcome: ci.Outcome{Conclusion: ci.Success}, end: start.Add(time.Minute),
			cancellation:   ci.Cancellation{SupersededBy: "demo-ci-2222222-1"},
			wantConclusion: ci.Success, wantTitle: "Succeeded in 1m0s",
		},
		{
			name: "cancelled by Octomaton", outcome: ci.Outcome{Conclusion: ci.Cancelled}, end: start.Add(time.Minute),
			cancellation:   ci.Cancellation{Reason: "merge group destroyed (dequeued)"},
			wantConclusion: ci.Cancelled, wantTitle: "Cancelled after 1m0s", wantSummary: []string{"Cancelled by Octomaton: merge group destroyed (dequeued)."},
		},
		{
			name: "timed out", outcome: ci.Outcome{Conclusion: ci.TimedOut, Message: "PipelineRun timed out"}, end: start.Add(time.Hour),
			wantConclusion: ci.TimedOut, wantTitle: "Timed out after 1h0m0s", wantSummary: []string{"> PipelineRun timed out"},
		},
		{
			name: "results override title and summary", outcome: ci.Outcome{Conclusion: ci.Success}, end: start.Add(time.Minute),
			results:        map[string]string{ResultTitle: "3 tests passed", ResultSummary: "See **the report**."},
			wantConclusion: ci.Success, wantTitle: "3 tests passed", wantSummary: []string{"See **the report**.\n\n**PipelineRun:**"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			run, id := h.newRun(pushTrigger(), func(r *ci.Run) {
				finished(tt.outcome, start, tt.end)(r)
				r.Cancellation = tt.cancellation
			})
			h.runner.SetDetails(run.ID, ci.Details{Results: tt.results})
			h.reconcile(run.ID)
			r := h.report(id)
			if r.Conclusion != tt.wantConclusion || r.Title != tt.wantTitle {
				t.Fatalf("report = %s / %q, want %s / %q", r.Conclusion, r.Title, tt.wantConclusion, tt.wantTitle)
			}
			mustContain(t, r.Summary, tt.wantSummary...)
		})
	}
}

func TestCommentRunsAreAnswered(t *testing.T) {
	h := newHarness(t)
	tr := pushTrigger()
	tr.Event = ci.EventComment
	tr.PullRequest = &ci.PullRequest{Number: 5}
	tr.Comment = &ci.Comment{ID: 9, Author: "maintainer", Command: "/deploy", Arguments: "staging"}
	start := h.clock().Add(-time.Hour)
	run, _ := h.newRun(tr, finished(ci.Outcome{Conclusion: ci.Success}, start, start.Add(time.Minute)))
	h.reconcile(run.ID)
	comments := h.host.Comments()
	if len(comments) != 1 || comments[0].Number != 5 || !strings.HasPrefix(comments[0].Body, "@maintainer ✅ `/deploy staging`: Succeeded in 1m0s") {
		t.Fatalf("comments = %+v", comments)
	}
	h.reconcile(run.ID)
	if len(h.host.Comments()) != 1 {
		t.Fatalf("a comment is answered once")
	}
}

func TestTaskReports(t *testing.T) {
	h := newHarness(t)
	start := h.clock().Add(-time.Hour)
	build := h.host.AddReport(repo, ci.Report{Name: "ci / build", Revision: sha})
	test := h.host.AddReport(repo, ci.Report{Name: "ci / test", Revision: sha})
	run, _ := h.newRun(pushTrigger(), func(r *ci.Run) {
		running(start)(r)
		r.Reported = ci.ReportedInProgress
		r.TaskReportIDs = map[string]ci.ReportID{"build": build, "test": test}
	})
	h.runner.SetDetails(run.ID, ci.Details{Tasks: []ci.Task{{Name: "build", State: ci.TaskRunning, Started: start}, {Name: "test", State: ci.TaskPending}}})
	h.reconcile(run.ID)
	if r := h.report(build); r.Status != ci.StatusInProgress || !r.Started.Equal(start) {
		t.Fatalf("a started task's report is in progress: %+v", r)
	}
	if r := h.report(test); r.Status != ci.StatusQueued || r.Updates != 0 {
		t.Fatalf("a task not started stays queued: %+v", r)
	}

	// The run fails in build; test never starts.
	h.runner.Update(run.ID, finished(ci.Outcome{Conclusion: ci.Failure}, start, start.Add(time.Minute)))
	h.runner.SetDetails(run.ID, ci.Details{Tasks: []ci.Task{
		{Name: "build", State: ci.TaskFailed, Started: start, Finished: start.Add(time.Minute), Message: "boom", Results: map[string]string{ResultTitle: "Compilation failed"}},
		{Name: "test", State: ci.TaskPending},
	}})
	h.reconcile(run.ID)
	if r := h.report(build); r.Status != ci.StatusCompleted || r.Conclusion != ci.Failure || r.Title != "Compilation failed" {
		t.Fatalf("the failed task's report = %+v", r)
	}
	mustContain(t, h.report(build).Summary, "boom.", "[The task on the Dashboard](https://runs.example/ci-demo/demo-ci-1111111-1/build)")
	if r := h.report(test); r.Conclusion != ci.Cancelled || r.Title != "Not run" {
		t.Fatalf("a task that never ran in a failed run = %+v", r)
	}
	if states := h.get(run.ID).TaskReportStates; states["build"] != ci.StatusCompleted || states["test"] != ci.StatusCompleted {
		t.Fatalf("task report states = %v", states)
	}
}

func TestTaskReportsWhenTheRunEnds(t *testing.T) {
	tests := []struct {
		name           string
		conclusion     ci.Conclusion
		cancellation   ci.Cancellation
		started        bool
		wantConclusion ci.Conclusion
		wantTitle      string
	}{
		{name: "a passing run skipped it", conclusion: ci.Success, wantConclusion: ci.Skipped, wantTitle: "Skipped"},
		{name: "a failing run stopped it", conclusion: ci.Failure, started: true, wantConclusion: ci.Cancelled, wantTitle: "Stopped"},
		{name: "a failing run never got to it", conclusion: ci.Failure, wantConclusion: ci.Cancelled, wantTitle: "Not run"},
		{name: "a newer run superseded it", conclusion: ci.Cancelled, cancellation: ci.Cancellation{SupersededBy: "r2"}, started: true, wantConclusion: ci.Skipped, wantTitle: "Superseded"},
		{name: "a timeout stopped it", conclusion: ci.TimedOut, started: true, wantConclusion: ci.TimedOut, wantTitle: "Stopped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			start := h.clock().Add(-time.Hour)
			test := h.host.AddReport(repo, ci.Report{Name: "ci / test", Revision: sha})
			run, _ := h.newRun(pushTrigger(), func(r *ci.Run) {
				finished(ci.Outcome{Conclusion: tt.conclusion}, start, start.Add(time.Minute))(r)
				r.Cancellation = tt.cancellation
				r.TaskReportIDs = map[string]ci.ReportID{"test": test}
			})
			task := ci.Task{Name: "test", State: ci.TaskPending}
			if tt.started {
				task = ci.Task{Name: "test", State: ci.TaskCancelled, Started: start}
			}
			h.runner.SetDetails(run.ID, ci.Details{Tasks: []ci.Task{task}})
			h.reconcile(run.ID)
			if r := h.report(test); r.Conclusion != tt.wantConclusion || r.Title != tt.wantTitle {
				t.Fatalf("task report = %s / %q, want %s / %q", r.Conclusion, r.Title, tt.wantConclusion, tt.wantTitle)
			}
		})
	}
}

func TestDeleted(t *testing.T) {
	h := newHarness(t)
	run, id := h.newRun(pushTrigger(), running(h.clock()))
	if err := h.svc.Deleted(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if r := h.report(id); r.Conclusion != ci.Cancelled || !strings.Contains(r.Summary, "The PipelineRun was deleted before it finished.") {
		t.Fatalf("a run deleted before it finished is cancelled: %+v", r)
	}
	done, doneID := h.newRun(pushTrigger(), func(r *ci.Run) { r.Reported = ci.ReportedCompleted })
	_ = h.svc.Deleted(context.Background(), done)
	if r := h.report(doneID); r.Updates != 0 {
		t.Fatalf("a reported run's deletion changes nothing")
	}
}

func TestRunsWithoutReportsAreLetGo(t *testing.T) {
	start := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		change func(*ci.Run)
	}{
		{"never opened its report", func(r *ci.Run) { r.ReportID = 0 }},
		{"lost its repository", func(r *ci.Run) { r.Trigger = ci.Trigger{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			run, _ := h.newRun(pushTrigger(), func(r *ci.Run) {
				finished(ci.Outcome{Conclusion: ci.Cancelled}, start, start)(r)
				tt.change(r)
			})
			h.reconcile(run.ID)
			if !h.get(run.ID).Done {
				t.Fatalf("a finished run whose report cannot be written is let go")
			}
		})
	}
}

func TestHeaderAndTitle(t *testing.T) {
	link := ci.RunLink{Kind: "PipelineRun", Name: "ci-demo/r1", URL: "https://runs.example/r1"}
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"a header with a link and a trigger", Header(link, pushTrigger()), "**PipelineRun:** [`ci-demo/r1`](https://runs.example/r1)\n\n**Trigger:** Push to `main` at `1111111` by @alice\n"},
		{"a header without a dashboard or a readable trigger", Header(ci.RunLink{Kind: "PipelineRun", Name: "ci-demo/r1"}, ci.Trigger{Pipeline: "ci"}), "**PipelineRun:** `ci-demo/r1`\n"},
		{"success", Title(ci.Success, 192*time.Second), "Succeeded in 3m12s"},
		{"failure", Title(ci.Failure, 500*time.Millisecond), "Failed after <1s"},
		{"supersede", Title(ci.Skipped, time.Minute), "Superseded"},
		{"task report name", TaskName("ci", "build"), "ci / build"},
		{"fence without backticks", codeFence("no backticks"), "```"},
		{"fence around backticks", codeFence("a ```` b"), "`````"},
		{"the tail of a log", truncateHead("abcdef", 3), "def"},
		{"a tail that starts mid-rune", truncateHead("ééé", 3), "é"},
		{"a table cell", cell("a | b\nc"), "a \\| b c"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestWatchedRunsAreReported(t *testing.T) {
	h := newHarness(t)
	start := h.clock().Add(-time.Hour)
	run, id := h.newRun(pushTrigger(), finished(ci.Outcome{Conclusion: ci.Success}, start, start.Add(time.Minute)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.runner.Watch(ctx, h.svc) }()
	for deadline := time.Now().Add(5 * time.Second); h.report(id).Status != ci.StatusCompleted; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the watched run was not reported")
		}
	}
	cancel()
	if err := <-done; err != nil || !h.get(run.ID).Done {
		t.Fatalf("Watch = %v; run done %v", err, h.get(run.ID).Done)
	}
}

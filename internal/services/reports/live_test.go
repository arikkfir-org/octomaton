package reports

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/ci/citest"
)

const stepLogs = "e2e-pod/step-run"

// liveRun creates a running run whose task e2e has a live report, and returns the run and that report.
func (h *harness) liveRun(live bool) (ci.Run, ci.ReportID) {
	h.t.Helper()
	e2e := h.host.AddReport(repo, ci.Report{Name: "ci / e2e", Revision: sha})
	run, _ := h.newRun(pushTrigger(), func(r *ci.Run) {
		running(h.clock().Add(-time.Hour))(r)
		r.Reported = ci.ReportedInProgress
		r.Tasks, r.TaskReportIDs = []string{"e2e"}, map[string]ci.ReportID{"e2e": e2e}
		r.LiveTaskReports = live
	})
	h.setTask(run.ID, ci.Task{Name: "e2e", State: ci.TaskRunning, Started: h.clock().Add(-time.Hour), Steps: []ci.Step{{Name: "run", Logs: stepLogs}}})
	return run, e2e
}

func (h *harness) setTask(id ci.RunID, t ci.Task) {
	h.runner.SetDetails(id, ci.Details{Tasks: []ci.Task{t}})
}

func (h *harness) print(name, value string) {
	h.runner.Print(stepLogs, citest.Output{At: h.clock(), Name: name, Value: value})
}

func TestLiveTaskReports(t *testing.T) {
	h := newHarness(t)
	run, e2e := h.liveRun(true)
	first := h.clock()
	h.print(ResultTitle, "3 of 40 passed")
	h.print(ResultSummary, "**Failing:** `login`")

	if again := h.reconcile(run.ID); again != liveInterval {
		t.Fatalf("again = %v, want %v", again, liveInterval)
	}
	r := h.report(e2e)
	if r.Status != ci.StatusInProgress || r.Title != "3 of 40 passed" || r.Trigger == nil {
		t.Fatalf("live report = %+v", r)
	}
	if want := "**Failing:** `login`\n\n[The task on the Dashboard](https://runs.example/ci-demo/demo-ci-1111111-1/e2e)."; r.Summary != want {
		t.Fatalf("live summary = %q, want %q", r.Summary, want)
	}
	if reads := h.runner.OutputReads(); len(reads) != 1 || !reflect.DeepEqual(reads[0], []ci.OutputRead{{Step: ci.Step{Name: "run", Logs: stepLogs}}}) {
		t.Fatalf("the first look reads the step's whole output: %+v", reads)
	}

	// Looked at again before the interval: nothing is read.
	h.advance(5 * time.Second)
	if again := h.reconcile(run.ID); again != 10*time.Second || len(h.runner.OutputReads()) != 1 {
		t.Fatalf("again = %v after %d reads; want 10s after 1", again, len(h.runner.OutputReads()))
	}

	// A new title; the summary printed before stays.
	h.advance(10 * time.Second)
	h.print(ResultTitle, "7 of 40 passed")
	h.reconcile(run.ID)
	if reads := h.runner.OutputReads(); len(reads) != 2 || !reads[1][0].Since.Equal(first.Add(-liveSlack)) {
		t.Fatalf("the next look reads from a little before the last: %+v", reads)
	}
	if r := h.report(e2e); r.Title != "7 of 40 passed" || r.Summary[:len("**Failing:**")] != "**Failing:**" {
		t.Fatalf("live report = %q / %q", r.Title, r.Summary)
	}

	// Nothing new is not written again.
	updates := h.report(e2e).Updates
	h.advance(liveInterval)
	h.reconcile(run.ID)
	if h.report(e2e).Updates != updates {
		t.Fatalf("an unchanged live report must not be written again")
	}

	// The task ends: its results replace the live text, as without live reports.
	h.setTask(run.ID, ci.Task{Name: "e2e", State: ci.TaskSucceeded, Started: h.clock().Add(-time.Hour), Finished: h.clock(), Results: map[string]string{ResultTitle: "40 passed"}})
	h.advance(liveInterval)
	if again := h.reconcile(run.ID); again != 0 {
		t.Fatalf("again = %v once no task runs", again)
	}
	if r := h.report(e2e); r.Status != ci.StatusCompleted || r.Conclusion != ci.Success || r.Title != "40 passed" {
		t.Fatalf("the ended task's report = %+v", r)
	}
	if len(h.svc.live) != 0 {
		t.Fatalf("live states of tasks that ended are kept: %v", h.svc.live)
	}
}

func TestLiveTaskReportsBestEffort(t *testing.T) {
	failure := errors.New("unavailable")
	tests := []struct {
		name string
		live bool
		// first runs before the first look, then.
		first func(h *harness)
		then  func(h *harness)
		// wantFirst and wantThen are the report's title after each look.
		wantFirst, wantThen string
	}{
		{
			name: "a run without live task reports reads nothing", live: false,
			first: func(h *harness) { h.print(ResultTitle, "3 of 40") }, then: func(*harness) {},
			wantFirst: "Running", wantThen: "Running",
		},
		{
			name: "a failed read writes nothing and is tried again",
			live: true, first: func(h *harness) { h.print(ResultTitle, "3 of 40"); h.runner.Fail("TaskOutput", failure) },
			then:      func(h *harness) { h.runner.Fail("TaskOutput", nil) },
			wantFirst: "Running", wantThen: "3 of 40",
		},
		{
			name: "a failed write is written at the next look, with nothing new printed",
			live: true, first: func(h *harness) { h.print(ResultTitle, "3 of 40"); h.host.Fail("UpdateReport", failure) },
			then:      func(h *harness) { h.host.Fail("UpdateReport", nil) },
			wantFirst: "Running", wantThen: "3 of 40",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			run, e2e := h.liveRun(tt.live)
			// The task's report goes in progress before anything is printed.
			h.reconcile(run.ID)
			h.advance(liveInterval)
			tt.first(h)
			h.reconcile(run.ID) // a live report's failure is not the run's
			if got := h.report(e2e).Title; got != tt.wantFirst {
				t.Fatalf("title after the first look = %q, want %q", got, tt.wantFirst)
			}
			h.advance(liveInterval)
			tt.then(h)
			h.reconcile(run.ID)
			if got := h.report(e2e).Title; got != tt.wantThen {
				t.Fatalf("title after the next look = %q, want %q", got, tt.wantThen)
			}
			if !tt.live && len(h.runner.OutputReads()) != 0 {
				t.Fatalf("output read: %+v", h.runner.OutputReads())
			}
		})
	}
}

func TestLiveReads(t *testing.T) {
	looked := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	since := looked.Add(-liveSlack)
	running := ci.Step{Name: "run", Logs: "pod/step-run"}
	endedBefore := ci.Step{Name: "setup", Logs: "pod/step-setup", Finished: since.Add(-time.Second)}
	endedSince := ci.Step{Name: "setup", Logs: "pod/step-setup", Finished: since}
	tests := []struct {
		name  string
		l     live
		steps []ci.Step
		want  []ci.OutputRead
	}{
		{name: "never looked: everything of every step", steps: []ci.Step{endedBefore, running},
			want: []ci.OutputRead{{Step: endedBefore}, {Step: running}}},
		{name: "a running step read before: since the last look, less the slack",
			l: live{looked: looked, read: map[string]bool{"run": true}}, steps: []ci.Step{running},
			want: []ci.OutputRead{{Step: running, Since: since}}},
		{name: "a step that ended before the last look is not read again",
			l: live{looked: looked, read: map[string]bool{"setup": true, "run": true}}, steps: []ci.Step{endedBefore, running},
			want: []ci.OutputRead{{Step: running, Since: since}}},
		{name: "a step that ended since the last look is read to its end",
			l: live{looked: looked, read: map[string]bool{"setup": true}}, steps: []ci.Step{endedSince},
			want: []ci.OutputRead{{Step: endedSince, Since: since}}},
		{name: "a step that started since the last look: everything it printed",
			l: live{looked: looked, read: map[string]bool{"setup": true}}, steps: []ci.Step{endedBefore, running},
			want: []ci.OutputRead{{Step: running}}},
		{name: "no step started", l: live{looked: looked}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := liveReads(tt.l, tt.steps); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("liveReads = %+v, want %+v", got, tt.want)
			}
		})
	}
}

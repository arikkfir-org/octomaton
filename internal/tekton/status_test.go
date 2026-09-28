package tekton

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestOutcomeOf(t *testing.T) {
	tests := []struct {
		name           string
		conditions     []Condition
		wantStatus     string
		wantConclusion string
	}{
		{"no status yet (pending)", nil, StatusInProgress, ""},
		{"started", []Condition{{Type: "Succeeded", Status: "Unknown", Reason: "Started"}}, StatusInProgress, ""},
		{"running", []Condition{{Type: "Succeeded", Status: "Unknown", Reason: "Running"}}, StatusInProgress, ""},
		{"pending", []Condition{{Type: "Succeeded", Status: "Unknown", Reason: "PipelineRunPending"}}, StatusInProgress, ""},
		{"succeeded", []Condition{{Type: "Succeeded", Status: "True", Reason: "Succeeded"}}, StatusCompleted, ConclusionSuccess},
		{"completed with skips", []Condition{{Type: "Succeeded", Status: "True", Reason: "Completed"}}, StatusCompleted, ConclusionSuccess},
		{"cancelled", []Condition{{Type: "Succeeded", Status: "False", Reason: "Cancelled"}}, StatusCompleted, ConclusionCancelled},
		{"cancelled, finally ran", []Condition{{Type: "Succeeded", Status: "False", Reason: "CancelledRunFinally"}}, StatusCompleted, ConclusionCancelled},
		{"stopped, finally ran", []Condition{{Type: "Succeeded", Status: "False", Reason: "StoppedRunFinally"}}, StatusCompleted, ConclusionCancelled},
		{"timed out", []Condition{{Type: "Succeeded", Status: "False", Reason: "PipelineRunTimeout"}}, StatusCompleted, ConclusionTimedOut},
		{"failed", []Condition{{Type: "Succeeded", Status: "False", Reason: "Failed"}}, StatusCompleted, ConclusionFailure},
		{"could not get pipeline", []Condition{{Type: "Succeeded", Status: "False", Reason: "CouldntGetPipeline"}}, StatusCompleted, ConclusionFailure},
		{"other condition types are ignored", []Condition{{Type: "Ready", Status: "True"}}, StatusInProgress, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := OutcomeOf(tt.conditions)
			if o.Status != tt.wantStatus || o.Conclusion != tt.wantConclusion {
				t.Fatalf("OutcomeOf = %+v, want %s/%s", o, tt.wantStatus, tt.wantConclusion)
			}
		})
	}
}

func run(status map[string]any, spec map[string]any, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": APIVersion, "kind": KindPipelineRun, "metadata": map[string]any{"name": "r", "namespace": "ns"}}}
	if status != nil {
		u.Object["status"] = status
	}
	if spec != nil {
		u.Object["spec"] = spec
	}
	u.SetAnnotations(annotations)
	return u
}

func TestRunOutcomeSuperseded(t *testing.T) {
	cancelled := map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": "False", "reason": "CancelledRunFinally"}}}
	pr := run(cancelled, nil, map[string]string{AnnotationSupersededBy: "head:abc"})
	st, _ := GetPipelineRunStatus(pr)
	if o := RunOutcome(pr, st); o.Conclusion != ConclusionSkipped || o.Title != "Superseded" {
		t.Fatalf("superseded run = %+v, want skipped", o)
	}
	succeeded := run(map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": "True"}}}, nil, map[string]string{AnnotationSupersededBy: "x"})
	st, _ = GetPipelineRunStatus(succeeded)
	if o := RunOutcome(succeeded, st); o.Conclusion != ConclusionSuccess {
		t.Fatalf("a run that succeeded before being superseded stays successful: %+v", o)
	}
}

func TestRunPredicates(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	tests := []struct {
		name                                    string
		pr                                      *unstructured.Unstructured
		done, pending, started, cancelRequested bool
	}{
		{"fresh held run", run(nil, map[string]any{"status": SpecStatusPending}, nil), false, true, false, false},
		{"released, not started", run(nil, map[string]any{}, nil), false, false, false, false},
		{"running", run(map[string]any{"startTime": now}, map[string]any{}, nil), false, false, true, false},
		{"cancelling", run(map[string]any{"startTime": now}, map[string]any{"status": SpecStatusCancelled}, nil), false, false, true, true},
		{"finished by condition", run(map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": "False"}}}, nil, nil), true, false, false, false},
		{"finished by completion time", run(map[string]any{"completionTime": now}, nil, nil), true, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if IsDone(tt.pr) != tt.done || IsPending(tt.pr) != tt.pending || Started(tt.pr) != tt.started || CancelRequested(tt.pr) != tt.cancelRequested {
				t.Fatalf("done=%v pending=%v started=%v cancel=%v", IsDone(tt.pr), IsPending(tt.pr), Started(tt.pr), CancelRequested(tt.pr))
			}
		})
	}
}

func TestStatusDecoding(t *testing.T) {
	pr := run(map[string]any{
		"startTime":       "2026-01-01T00:00:00Z",
		"completionTime":  "2026-01-01T00:02:30Z",
		"conditions":      []any{map[string]any{"type": "Succeeded", "status": "True", "reason": "Succeeded", "message": "Tasks Completed: 2"}},
		"childReferences": []any{map[string]any{"apiVersion": "tekton.dev/v1", "kind": "TaskRun", "name": "r-build", "pipelineTaskName": "build"}},
		"skippedTasks":    []any{map[string]any{"name": "deploy", "reason": "When Expressions evaluated to false"}},
		"results":         []any{map[string]any{"name": "check-title", "value": "Done"}, map[string]any{"name": "list", "value": []any{"a"}}},
	}, nil, nil)
	st, err := GetPipelineRunStatus(pr)
	if err != nil {
		t.Fatalf("GetPipelineRunStatus: %v", err)
	}
	if len(st.ChildReferences) != 1 || st.ChildReferences[0].PipelineTaskName != "build" || len(st.SkippedTasks) != 1 {
		t.Fatalf("status = %+v", st)
	}
	if d := Duration(st.StartTime, st.CompletionTime, time.Now()); d != 150*time.Second {
		t.Fatalf("Duration = %v", d)
	}
	if Duration(nil, nil, time.Now()) != 0 {
		t.Fatalf("no start time, no duration")
	}
	start := metav1.NewTime(time.Now().Add(-time.Minute))
	if d := Duration(&start, nil, time.Now()); d < time.Minute {
		t.Fatalf("running duration = %v", d)
	}
	if got := Results(pr); got["check-title"] != "Done" || len(got) != 1 {
		t.Fatalf("Results = %v (string results only)", got)
	}
	tr := run(map[string]any{"podName": "p", "steps": []any{map[string]any{"name": "s", "container": "step-s", "terminated": map[string]any{"exitCode": int64(2)}}}}, nil, nil)
	ts, err := GetTaskRunStatus(tr)
	if err != nil || ts.PodName != "p" || ts.Steps[0].Terminated.ExitCode != 2 {
		t.Fatalf("GetTaskRunStatus = %+v, %v", ts, err)
	}
}

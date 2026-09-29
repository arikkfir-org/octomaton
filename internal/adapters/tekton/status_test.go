package tekton

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"octomaton.dev/internal/services/ci"
)

func TestConclusionOf(t *testing.T) {
	tests := []struct {
		name           string
		conditions     []condition
		wantDone       bool
		wantConclusion ci.Conclusion
	}{
		{name: "no status yet (pending)"},
		{name: "started", conditions: []condition{{Type: "Succeeded", Status: "Unknown", Reason: "Started"}}},
		{name: "running", conditions: []condition{{Type: "Succeeded", Status: "Unknown", Reason: "Running"}}},
		{name: "pending", conditions: []condition{{Type: "Succeeded", Status: "Unknown", Reason: "PipelineRunPending"}}},
		{name: "succeeded", conditions: []condition{{Type: "Succeeded", Status: "True", Reason: "Succeeded"}}, wantDone: true, wantConclusion: ci.Success},
		{name: "completed with skips", conditions: []condition{{Type: "Succeeded", Status: "True", Reason: "Completed"}}, wantDone: true, wantConclusion: ci.Success},
		{name: "cancelled", conditions: []condition{{Type: "Succeeded", Status: "False", Reason: "Cancelled"}}, wantDone: true, wantConclusion: ci.Cancelled},
		{name: "cancelled, finally ran", conditions: []condition{{Type: "Succeeded", Status: "False", Reason: "CancelledRunFinally"}}, wantDone: true, wantConclusion: ci.Cancelled},
		{name: "stopped, finally ran", conditions: []condition{{Type: "Succeeded", Status: "False", Reason: "StoppedRunFinally"}}, wantDone: true, wantConclusion: ci.Cancelled},
		{name: "timed out", conditions: []condition{{Type: "Succeeded", Status: "False", Reason: "PipelineRunTimeout"}}, wantDone: true, wantConclusion: ci.TimedOut},
		{name: "task timed out", conditions: []condition{{Type: "Succeeded", Status: "False", Reason: "TaskRunTimeout"}}, wantDone: true, wantConclusion: ci.TimedOut},
		{name: "failed", conditions: []condition{{Type: "Succeeded", Status: "False", Reason: "Failed", Message: "boom"}}, wantDone: true, wantConclusion: ci.Failure},
		{name: "could not get pipeline", conditions: []condition{{Type: "Succeeded", Status: "False", Reason: "CouldntGetPipeline"}}, wantDone: true, wantConclusion: ci.Failure},
		{name: "other condition types are ignored", conditions: []condition{{Type: "Ready", Status: "True"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conclusion, message, done := conclusionOf(tt.conditions)
			if done != tt.wantDone || conclusion != tt.wantConclusion {
				t.Fatalf("conclusionOf = %q, %v; want %q, %v", conclusion, done, tt.wantConclusion, tt.wantDone)
			}
			if c, ok := succeeded(tt.conditions); ok && done && message != c.Message {
				t.Fatalf("message = %q, want the condition's %q", message, c.Message)
			}
		})
	}
}

func run(status map[string]any, spec map[string]any, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kindPipelineRun, "metadata": map[string]any{"name": "r", "namespace": "ns"}}}
	if status != nil {
		u.Object["status"] = status
	}
	if spec != nil {
		u.Object["spec"] = spec
	}
	u.SetAnnotations(annotations)
	return u
}

func TestRunPredicates(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	tests := []struct {
		name                                    string
		pr                                      *unstructured.Unstructured
		done, pending, started, cancelRequested bool
	}{
		{"fresh held run", run(nil, map[string]any{"status": specStatusPending}, nil), false, true, false, false},
		{"released, not started", run(nil, map[string]any{}, nil), false, false, false, false},
		{"running", run(map[string]any{"startTime": now}, map[string]any{}, nil), false, false, true, false},
		{"cancelling", run(map[string]any{"startTime": now}, map[string]any{"status": specStatusCancelled}, nil), false, false, true, true},
		{"finished by condition", run(map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": "False"}}}, nil, nil), true, false, false, false},
		{"finished by completion time", run(map[string]any{"completionTime": now}, nil, nil), true, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if isDone(tt.pr) != tt.done || isPending(tt.pr) != tt.pending || started(tt.pr) != tt.started || cancelRequested(tt.pr) != tt.cancelRequested {
				t.Fatalf("done=%v pending=%v started=%v cancel=%v", isDone(tt.pr), isPending(tt.pr), started(tt.pr), cancelRequested(tt.pr))
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
	st, err := getPipelineRunStatus(pr)
	if err != nil {
		t.Fatalf("GetPipelineRunStatus: %v", err)
	}
	if len(st.ChildReferences) != 1 || st.ChildReferences[0].PipelineTaskName != "build" || len(st.SkippedTasks) != 1 {
		t.Fatalf("status = %+v", st)
	}
	if st.StartTime == nil || st.CompletionTime == nil || st.CompletionTime.Sub(st.StartTime.Time) != 150*time.Second {
		t.Fatalf("start and completion times = %v, %v", st.StartTime, st.CompletionTime)
	}
	if got := resultsOf(pr); got["check-title"] != "Done" || len(got) != 1 {
		t.Fatalf("Results = %v (string results only)", got)
	}
	tr := run(map[string]any{"podName": "p", "steps": []any{map[string]any{"name": "s", "container": "step-s", "terminated": map[string]any{"exitCode": int64(2)}}}}, nil, nil)
	ts, err := getTaskRunStatus(tr)
	if err != nil || ts.PodName != "p" || ts.Steps[0].Terminated.ExitCode != 2 {
		t.Fatalf("GetTaskRunStatus = %+v, %v", ts, err)
	}
}

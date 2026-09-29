package tekton

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition is a knative-style status condition.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ChildReference points from a PipelineRun to one of its TaskRuns or CustomRuns.
type ChildReference struct {
	APIVersion       string `json:"apiVersion,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Name             string `json:"name,omitempty"`
	PipelineTaskName string `json:"pipelineTaskName,omitempty"`
	DisplayName      string `json:"displayName,omitempty"`
}

// SkippedTask is a pipeline task that did not run.
type SkippedTask struct {
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

// PipelineRunStatus is the subset of a PipelineRun's status Octomaton reads.
type PipelineRunStatus struct {
	Conditions      []Condition      `json:"conditions,omitempty"`
	StartTime       *metav1.Time     `json:"startTime,omitempty"`
	CompletionTime  *metav1.Time     `json:"completionTime,omitempty"`
	ChildReferences []ChildReference `json:"childReferences,omitempty"`
	SkippedTasks    []SkippedTask    `json:"skippedTasks,omitempty"`
}

// StepTerminated describes a finished step container.
type StepTerminated struct {
	ExitCode int32  `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
}

// StepState is the state of one TaskRun step.
type StepState struct {
	Name       string          `json:"name,omitempty"`
	Container  string          `json:"container,omitempty"`
	Terminated *StepTerminated `json:"terminated,omitempty"`
}

// TaskRunStatus is the subset of a TaskRun's status Octomaton reads.
type TaskRunStatus struct {
	Conditions     []Condition  `json:"conditions,omitempty"`
	PodName        string       `json:"podName,omitempty"`
	StartTime      *metav1.Time `json:"startTime,omitempty"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	Steps          []StepState  `json:"steps,omitempty"`
}

// GetPipelineRunStatus decodes a PipelineRun's status.
func GetPipelineRunStatus(obj *unstructured.Unstructured) (PipelineRunStatus, error) {
	var st PipelineRunStatus
	return st, decodeStatus(obj, &st)
}

// GetTaskRunStatus decodes a TaskRun's status.
func GetTaskRunStatus(obj *unstructured.Unstructured) (TaskRunStatus, error) {
	var st TaskRunStatus
	return st, decodeStatus(obj, &st)
}

func decodeStatus(obj *unstructured.Unstructured, into any) error {
	raw, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil {
		return fmt.Errorf("reading status of %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	if !found {
		return nil
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, into); err != nil {
		return fmt.Errorf("decoding status of %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// Succeeded returns the Succeeded condition, if any.
func Succeeded(conditions []Condition) (Condition, bool) {
	for _, c := range conditions {
		if c.Type == "Succeeded" {
			return c, true
		}
	}
	return Condition{}, false
}

// Check-run statuses and conclusions.
const (
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"

	ConclusionSuccess   = "success"
	ConclusionFailure   = "failure"
	ConclusionCancelled = "cancelled"
	ConclusionTimedOut  = "timed_out"
	ConclusionSkipped   = "skipped"
)

// Outcome is a PipelineRun state expressed as a GitHub check-run status and conclusion.
type Outcome struct {
	Status     string
	Conclusion string
	Reason     string
	Message    string
	// Title is a short description of the conclusion ("Succeeded", "Superseded", ...).
	Title string
}

// Done reports whether the outcome is final.
func (o Outcome) Done() bool { return o.Status == StatusCompleted }

// OutcomeOf maps a Succeeded condition to a check-run status and conclusion:
// no condition or Unknown → in_progress; True → success; False with reason
// Cancelled, CancelledRunFinally or StoppedRunFinally → cancelled; False with
// reason PipelineRunTimeout → timed_out; any other False → failure.
func OutcomeOf(conditions []Condition) Outcome {
	c, ok := Succeeded(conditions)
	if !ok {
		return Outcome{Status: StatusInProgress}
	}
	o := Outcome{Reason: c.Reason, Message: c.Message}
	switch c.Status {
	case "True":
		o.Status, o.Conclusion, o.Title = StatusCompleted, ConclusionSuccess, "Succeeded"
	case "False":
		o.Status = StatusCompleted
		switch {
		case c.Reason == "Cancelled" || c.Reason == "CancelledRunFinally" || c.Reason == "StoppedRunFinally" ||
			strings.Contains(c.Reason, "Cancel") || strings.Contains(c.Reason, "Stopped"):
			o.Conclusion, o.Title = ConclusionCancelled, "Cancelled"
		case c.Reason == "PipelineRunTimeout" || strings.Contains(c.Reason, "Timeout"):
			o.Conclusion, o.Title = ConclusionTimedOut, "Timed out"
		default:
			o.Conclusion, o.Title = ConclusionFailure, "Failed"
		}
	default:
		o.Status = StatusInProgress
	}
	return o
}

// RunOutcome is OutcomeOf for a PipelineRun, where a finished run that
// Octomaton superseded concludes as skipped ("Superseded").
func RunOutcome(pr *unstructured.Unstructured, st PipelineRunStatus) Outcome {
	o := OutcomeOf(st.Conditions)
	if o.Done() && o.Conclusion != ConclusionSuccess && pr.GetAnnotations()[AnnotationSupersededBy] != "" {
		o.Conclusion, o.Title = ConclusionSkipped, "Superseded"
	}
	return o
}

// IsDone reports whether a PipelineRun (or TaskRun) has finished.
func IsDone(obj *unstructured.Unstructured) bool {
	if t, _, _ := unstructured.NestedString(obj.Object, "status", "completionTime"); t != "" {
		return true
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		if c["type"] == "Succeeded" {
			return c["status"] == "True" || c["status"] == "False"
		}
	}
	return false
}

// CancelRequested reports whether spec.status requests cancellation.
func CancelRequested(obj *unstructured.Unstructured) bool {
	s, _, _ := unstructured.NestedString(obj.Object, "spec", "status")
	switch s {
	case "Cancelled", "CancelledRunFinally", "StoppedRunFinally":
		return true
	}
	return false
}

// IsPending reports whether a run is held (spec.status PipelineRunPending).
func IsPending(obj *unstructured.Unstructured) bool {
	s, _, _ := unstructured.NestedString(obj.Object, "spec", "status")
	return s == SpecStatusPending
}

// Started reports whether Tekton started a released run.
func Started(obj *unstructured.Unstructured) bool {
	t, _, _ := unstructured.NestedString(obj.Object, "status", "startTime")
	return t != "" && !IsPending(obj)
}

// Results returns a run's (or TaskRun's) results by name.
func Results(obj *unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	list, _, _ := unstructured.NestedSlice(obj.Object, "status", "results")
	for _, r := range list {
		m, _ := r.(map[string]any)
		name, _ := m["name"].(string)
		if v, ok := m["value"].(string); ok && name != "" {
			out[name] = v
		}
	}
	return out
}

// Duration returns the time between start and end (or now when end is nil).
func Duration(start, end *metav1.Time, now time.Time) time.Duration {
	if start == nil {
		return 0
	}
	if end != nil {
		return end.Sub(start.Time)
	}
	return now.Sub(start.Time)
}

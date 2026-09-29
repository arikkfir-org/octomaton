package tekton

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"octomaton.dev/internal/services/ci"
)

// condition is a knative-style status condition.
type condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// childReference points from a PipelineRun to one of its TaskRuns or CustomRuns.
type childReference struct {
	APIVersion       string `json:"apiVersion,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Name             string `json:"name,omitempty"`
	PipelineTaskName string `json:"pipelineTaskName,omitempty"`
	DisplayName      string `json:"displayName,omitempty"`
}

// skippedTask is a pipeline task that did not run.
type skippedTask struct {
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

// pipelineRunStatus is the subset of a PipelineRun's status Octomaton reads.
type pipelineRunStatus struct {
	Conditions      []condition      `json:"conditions,omitempty"`
	StartTime       *metav1.Time     `json:"startTime,omitempty"`
	CompletionTime  *metav1.Time     `json:"completionTime,omitempty"`
	ChildReferences []childReference `json:"childReferences,omitempty"`
	SkippedTasks    []skippedTask    `json:"skippedTasks,omitempty"`
}

// stepTerminated describes a finished step container.
type stepTerminated struct {
	ExitCode int32  `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
}

// stepState is the state of one TaskRun step.
type stepState struct {
	Name       string          `json:"name,omitempty"`
	Container  string          `json:"container,omitempty"`
	Terminated *stepTerminated `json:"terminated,omitempty"`
}

// taskRunStatus is the subset of a TaskRun's status Octomaton reads.
type taskRunStatus struct {
	Conditions     []condition  `json:"conditions,omitempty"`
	PodName        string       `json:"podName,omitempty"`
	StartTime      *metav1.Time `json:"startTime,omitempty"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	Steps          []stepState  `json:"steps,omitempty"`
}

// getPipelineRunStatus decodes a PipelineRun's status.
func getPipelineRunStatus(obj *unstructured.Unstructured) (pipelineRunStatus, error) {
	var st pipelineRunStatus
	return st, decodeStatus(obj, &st)
}

// getTaskRunStatus decodes a TaskRun's status.
func getTaskRunStatus(obj *unstructured.Unstructured) (taskRunStatus, error) {
	var st taskRunStatus
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

// succeeded returns the Succeeded condition, if any.
func succeeded(conditions []condition) (condition, bool) {
	for _, c := range conditions {
		if c.Type == "Succeeded" {
			return c, true
		}
	}
	return condition{}, false
}

// conclusionOf maps a Succeeded condition to how a run (or TaskRun) ended: True is success; False
// is cancelled for a cancelled or stopped reason (Cancelled, CancelledRunFinally,
// StoppedRunFinally), timed out for a timeout reason (PipelineRunTimeout, TaskRunTimeout), and
// failure otherwise. done is false while it has not ended: no condition, or Unknown.
func conclusionOf(conditions []condition) (conclusion ci.Conclusion, message string, done bool) {
	c, ok := succeeded(conditions)
	if !ok {
		return "", "", false
	}
	switch c.Status {
	case "True":
		return ci.Success, c.Message, true
	case "False":
		switch {
		case strings.Contains(c.Reason, "Cancel") || strings.Contains(c.Reason, "Stopped"):
			return ci.Cancelled, c.Message, true
		case strings.Contains(c.Reason, "Timeout"):
			return ci.TimedOut, c.Message, true
		default:
			return ci.Failure, c.Message, true
		}
	}
	return "", "", false
}

// isDone reports whether a PipelineRun (or TaskRun) has finished.
func isDone(obj *unstructured.Unstructured) bool {
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

// cancelRequested reports whether spec.status requests cancellation.
func cancelRequested(obj *unstructured.Unstructured) bool {
	s, _, _ := unstructured.NestedString(obj.Object, "spec", "status")
	switch s {
	case "Cancelled", "CancelledRunFinally", "StoppedRunFinally":
		return true
	}
	return false
}

// isPending reports whether a run is held (spec.status PipelineRunPending).
func isPending(obj *unstructured.Unstructured) bool {
	s, _, _ := unstructured.NestedString(obj.Object, "spec", "status")
	return s == specStatusPending
}

// started reports whether Tekton started a released run.
func started(obj *unstructured.Unstructured) bool {
	t, _, _ := unstructured.NestedString(obj.Object, "status", "startTime")
	return t != "" && !isPending(obj)
}

// resultsOf returns a run's (or TaskRun's) results by name.
func resultsOf(obj *unstructured.Unstructured) map[string]string {
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

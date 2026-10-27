package tekton

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"octomaton.dev/internal/services/ci"
)

func outputOf(name, value string) string {
	return ci.OutputPrefix + name + " " + base64.StdEncoding.EncodeToString([]byte(value))
}

func TestScanOutput(t *testing.T) {
	names := []string{"check-title", "check-summary"}
	tests := []struct {
		name string
		logs string
		want map[string]string
	}{
		{name: "no output lines", logs: "building\nok\n", want: map[string]string{}},
		{name: "a title and a multi-line summary", logs: "x\n" + outputOf("check-title", "3 of 9 passed") + "\n" + outputOf("check-summary", "| a | b |\n| --- | --- |") + "\ny\n",
			want: map[string]string{"check-title": "3 of 9 passed", "check-summary": "| a | b |\n| --- | --- |"}},
		{name: "the newest line wins", logs: outputOf("check-title", "1 of 9") + "\n" + outputOf("check-title", "2 of 9") + "\n",
			want: map[string]string{"check-title": "2 of 9"}},
		{name: "carriage returns and spaces around the value", logs: ci.OutputPrefix + "check-title  " + base64.StdEncoding.EncodeToString([]byte("ok")) + " \r\n",
			want: map[string]string{"check-title": "ok"}},
		{name: "a name it doesn't read", logs: outputOf("check-text", "no") + "\n", want: map[string]string{}},
		{name: "a value that isn't base64", logs: ci.OutputPrefix + "check-title 3 of 9\n", want: map[string]string{}},
		{name: "a value that isn't text", logs: ci.OutputPrefix + "check-title " + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) + "\n", want: map[string]string{}},
		{name: "an empty value", logs: outputOf("check-title", "1 of 9") + "\n" + outputOf("check-title", "  ") + "\n", want: map[string]string{"check-title": "1 of 9"}},
		{name: "a line that doesn't start with the prefix", logs: "echo " + outputOf("check-title", "no") + "\n", want: map[string]string{}},
		{name: "a last line without its newline", logs: outputOf("check-title", "1 of 9") + "\n" + outputOf("check-title", "2 of 9"), want: map[string]string{"check-title": "1 of 9"}},
		{name: "a line too long, then a short one", logs: outputOf("check-summary", strings.Repeat("x", maxOutputLine)) + "\n" + outputOf("check-title", "after") + "\n",
			want: map[string]string{"check-title": "after"}},
		{name: "a summary as long as a check takes", logs: outputOf("check-summary", strings.Repeat("x", 65535)) + "\n",
			want: map[string]string{"check-summary": strings.Repeat("x", 65535)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := map[string]string{}
			if err := scanOutput(strings.NewReader(tt.logs), names, got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("scanOutput = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartedSteps(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 5, 1, 9, 3, 0, 0, time.UTC))
	tests := []struct {
		name string
		ts   taskRunStatus
		want []ci.Step
	}{
		{name: "no pod yet", ts: taskRunStatus{Steps: []stepState{{Name: "a", Container: "step-a"}}}},
		{name: "ended, running and waiting steps", ts: taskRunStatus{PodName: "pod", Steps: []stepState{
			{Name: "a", Container: "step-a", Terminated: &stepTerminated{FinishedAt: at}},
			{Name: "b", Container: "step-b", Running: &stepRunning{}},
			{Name: "c", Container: "step-c"},
		}}, want: []ci.Step{{Name: "a", Logs: "pod/step-a", Finished: at.Time}, {Name: "b", Logs: "pod/step-b"}}},
		{name: "a step without a container", ts: taskRunStatus{PodName: "pod", Steps: []stepState{{Name: "a", Running: &stepRunning{}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startedSteps(tt.ts); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("startedSteps = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTaskOutput(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	id := h.create(demoSpec(pushTrigger(shaA)), 1).ID
	since := time.Date(2026, 5, 1, 9, 5, 0, 0, time.UTC)
	reads := []ci.OutputRead{
		{Step: ci.Step{Name: "a", Logs: "build-pod/step-a", Finished: since}},
		{Step: ci.Step{Name: "b", Logs: "build-pod/step-b"}, Since: since},
	}
	// The fake clientset's logs are always "fake logs", which hold no output line.
	out, err := h.r.TaskOutput(ctx, id, reads, []string{"check-title"})
	if err != nil || len(out) != 0 {
		t.Fatalf("TaskOutput = %v, %v", out, err)
	}
	var got []corev1.PodLogOptions
	for _, a := range h.kube.Actions() {
		if a.GetSubresource() == "log" {
			got = append(got, *a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions))
		}
	}
	want := []corev1.PodLogOptions{{Container: "step-a"}, {Container: "step-b", SinceTime: &metav1.Time{Time: since}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("log reads = %+v, want %+v", got, want)
	}

	if _, err := h.r.TaskOutput(ctx, id, []ci.OutputRead{{Step: ci.Step{Name: "a"}}}, []string{"check-title"}); err == nil || !strings.Contains(err.Error(), "no pod") {
		t.Fatalf("TaskOutput of a step without a pod: %v", err)
	}
	if _, err := h.r.TaskOutput(ctx, ci.RunID{Tenant: demoNS, Name: "missing"}, reads, []string{"check-title"}); !errors.Is(err, ci.ErrNotFound) {
		t.Fatalf("TaskOutput of a missing run: %v", err)
	}
	// A Secret that can't be read reads nothing, rather than passing output on unredacted.
	h.kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "x", errors.New("denied"))
	})
	if _, err := h.r.TaskOutput(ctx, id, reads, []string{"check-title"}); err == nil {
		t.Fatal("TaskOutput with an unreadable Secret must fail")
	}
}

func TestDetailsListARunningTasksSteps(t *testing.T) {
	h := newRunnerHarness(t)
	id := h.create(demoSpec(pushTrigger(shaA)), 1).ID
	h.taskRun(id, "build", "build", time.Date(2026, 5, 1, 9, 2, 0, 0, time.UTC), map[string]any{
		"podName": "build-pod", "startTime": "2026-05-01T09:02:00Z",
		"conditions": []any{map[string]any{"type": "Succeeded", "status": "Unknown", "reason": "Running"}},
		"steps": []any{
			map[string]any{"name": "a", "container": "step-a", "terminated": map[string]any{"exitCode": int64(0), "finishedAt": "2026-05-01T09:03:00Z"}},
			map[string]any{"name": "b", "container": "step-b", "running": map[string]any{"startedAt": "2026-05-01T09:03:00Z"}},
		},
	})
	details, err := h.r.Details(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := []ci.Step{{Name: "a", Logs: "build-pod/step-a", Finished: time.Date(2026, 5, 1, 9, 3, 0, 0, time.UTC)}, {Name: "b", Logs: "build-pod/step-b"}}
	if task := details.Tasks[0]; task.State != ci.TaskRunning || !reflect.DeepEqual(task.Steps, want) {
		t.Fatalf("build = %+v, want running with steps %+v", task, want)
	}
}

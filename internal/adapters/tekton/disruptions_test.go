package tekton

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/cache"
	"octomaton.dev/internal/services/ci"
)

// disruptedPod is a pod of a run's task, with a DisruptionTarget condition when reason is set.
func disruptedPod(name, run, task, node, status, reason string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: demoNS, Labels: map[string]string{labelPipelineRun: run, labelPipelineTask: task}},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}},
	}
	if reason != "" {
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type: corev1.DisruptionTarget, Status: corev1.ConditionStatus(status), Reason: reason, Message: "Pod was terminated in response to imminent node shutdown.",
		})
	}
	return pod
}

func TestDisruptionOf(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want *ci.Disruption
	}{
		{name: "node shutdown", pod: disruptedPod("p", "r", "build", "node-1", "True", "TerminationByKubelet"),
			want: &ci.Disruption{Task: "build", Node: "node-1", Reason: "TerminationByKubelet", Message: "Pod was terminated in response to imminent node shutdown."}},
		{name: "node gone", pod: disruptedPod("p", "r", "test", "node-2", "True", "DeletionByPodGC"),
			want: &ci.Disruption{Task: "test", Node: "node-2", Reason: "DeletionByPodGC", Message: "Pod was terminated in response to imminent node shutdown."}},
		{name: "preempted", pod: disruptedPod("p", "r", "test", "", "True", "PreemptionByScheduler"),
			want: &ci.Disruption{Task: "test", Reason: "PreemptionByScheduler", Message: "Pod was terminated in response to imminent node shutdown."}},
		{name: "a condition that is not true", pod: disruptedPod("p", "r", "build", "node-1", "False", "TerminationByKubelet")},
		{name: "failed by itself", pod: disruptedPod("p", "r", "build", "node-1", "", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := disruptionOf(tt.pod)
			if ok != (tt.want != nil) || ok && !reflect.DeepEqual(got, *tt.want) {
				t.Fatalf("disruptionOf = %+v, %v; want %+v", got, ok, tt.want)
			}
		})
	}
}

func TestDisruptedKey(t *testing.T) {
	longest := strings.Repeat("p", 63)
	tests := []struct {
		name string
		pod  string
	}{
		{name: "a pod", pod: "demo-ci-1111111-1-build-pod"},
		{name: "a pod whose name takes 63 characters", pod: longest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := disruptedKey(tt.pod)
			if errs := validation.IsQualifiedName(key); len(errs) > 0 || !strings.HasPrefix(key, "octomaton.dev/") {
				t.Fatalf("%q: %v", key, errs)
			}
		})
	}
	if disruptedKey("build-pod") == disruptedKey("test-pod") || disruptedKey("build-pod") != disruptedKey("build-pod") {
		t.Fatalf("one key per pod")
	}
}

func TestCreateRecordsReruns(t *testing.T) {
	h := newRunnerHarness(t)
	spec := demoSpec(pushTrigger(shaA))
	spec.Reruns = &ci.Reruns{Left: 1, Of: "demo-ci-1111111-1", Lost: []ci.LostAttempt{{Attempt: 1, Run: "demo-ci-1111111-1",
		Tasks: []ci.Disruption{{Task: "build", Node: "node-1", Reason: "TerminationByKubelet"}}}}}
	spec.ReportID, spec.TaskReportIDs = 41, map[string]ci.ReportID{"build": 42}
	run := h.get(h.create(spec, 2).ID)
	if !reflect.DeepEqual(run.Reruns, spec.Reruns) || run.ReportID != 41 || !reflect.DeepEqual(run.TaskReportIDs, spec.TaskReportIDs) {
		t.Fatalf("a rerun must record how it reruns and the reports it takes over: %+v, report %d, task reports %v", run.Reruns, run.ReportID, run.TaskReportIDs)
	}
	if run.Reported != "" {
		t.Fatalf("a report taken over is not reported on yet: %q", run.Reported)
	}
	if first := h.get(h.create(demoSpec(pushTrigger(shaB)), 1).ID); first.Reruns != nil || first.ReportID != 0 {
		t.Fatalf("a run without reruns records none: %+v", first)
	}
}

// TestDetailsTellsLostTasks covers the ways a task that lost its pod to a disruption fails: Tekton
// records the failure, not why, so only the run's record of the pod tells a lost task from one that
// failed by itself.
func TestDetailsTellsLostTasks(t *testing.T) {
	failure := func(reason, message string, exit int64) map[string]any {
		st := map[string]any{
			"podName":    "build-pod",
			"conditions": []any{map[string]any{"type": "Succeeded", "status": "False", "reason": reason, "message": message}},
		}
		if exit != 0 {
			st["steps"] = []any{map[string]any{"name": "b", "container": "step-b", "terminated": map[string]any{"exitCode": exit}}}
		}
		return st
	}
	forms := []struct {
		name   string
		status map[string]any
	}{
		{name: "pod failed on node shutdown", status: failure("Failed", "Pod was terminated in response to imminent node shutdown.", 0)},
		{name: "step terminated", status: failure("StepFailed", `"step-b" exited with code 143`, 143)},
		{name: "step lost its runtime", status: failure("StepFailed", `"step-b" exited with code 255`, 255)},
		{name: "init container terminated", status: failure("InitContainerFailed", `init container "prepare" exited with code 143`, 0)},
		{name: "sidecar failed", status: failure("SidecarFailed", `sidecar "docker" exited with code 1`, 0)},
	}
	lost := ci.Disruption{Task: "build", Node: "node-1", Reason: "TerminationByKubelet", Message: "Pod was terminated in response to imminent node shutdown."}
	recorded, _ := json.Marshal(disruptionRecord{Pod: "build-pod", Disruption: lost})
	other, _ := json.Marshal(disruptionRecord{Pod: "other-pod", Disruption: lost})
	for _, form := range forms {
		for _, tt := range []struct {
			name     string
			reruns   bool
			recorded bool
			want     *ci.Disruption
		}{
			{name: "recorded", reruns: true, recorded: true, want: &lost},
			{name: "not recorded", reruns: true},
			{name: "recorded on a run without reruns", recorded: true},
		} {
			t.Run(form.name+"/"+tt.name, func(t *testing.T) {
				h := newRunnerHarness(t)
				spec := demoSpec(pushTrigger(shaA))
				if tt.reruns {
					spec.Reruns = &ci.Reruns{Left: 2}
				}
				id := h.create(spec, 1).ID
				if tt.recorded {
					pr := h.object(id)
					a := pr.GetAnnotations()
					a[disruptedKey("build-pod")] = string(recorded)
					a[disruptedKey("other-pod")] = string(other)
					pr.SetAnnotations(a)
					h.update(pr)
				}
				h.taskRun(id, "build", "build", time.Date(2026, 5, 1, 9, 1, 0, 0, time.UTC), form.status)
				details, err := h.r.Details(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				build := details.Tasks[0]
				if build.State != ci.TaskFailed || !reflect.DeepEqual(build.Disruption, tt.want) {
					t.Fatalf("build: state %s, disruption %+v; want %+v", build.State, build.Disruption, tt.want)
				}
			})
		}
	}
}

func TestWatchRecordsDisruptedPods(t *testing.T) {
	h := newRunnerHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	spec := demoSpec(pushTrigger(shaA))
	spec.Reruns = &ci.Reruns{Left: 2}
	reruns := h.create(spec, 1)
	plain := h.create(demoSpec(pushTrigger(shaB)), 1)
	pods := h.kube.CoreV1().Pods(demoNS)
	for _, pod := range []*corev1.Pod{
		disruptedPod("build-pod", reruns.ID.Name, "build", "node-1", "True", "TerminationByKubelet"),
		disruptedPod("test-pod", reruns.ID.Name, "test", "node-1", "", ""),
		disruptedPod("plain-pod", plain.ID.Name, "build", "node-1", "True", "TerminationByKubelet"),
	} {
		if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	w := &watcher{reconciled: map[string]int{}}
	stopped := make(chan error, 1)
	go func() { stopped <- h.r.Watch(ctx, w) }()
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting until %s", what)
			}
		}
	}
	recorded := func(id ci.RunID, pod string) ci.Disruption {
		var rec disruptionRecord
		if raw := h.object(id).GetAnnotations()[disruptedKey(pod)]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &rec)
		}
		if rec.Pod != pod {
			return ci.Disruption{}
		}
		return rec.Disruption
	}
	eventually("a pod disrupted before the watch is recorded on its run", func() bool {
		return recorded(reruns.ID, "build-pod").Reason == "TerminationByKubelet"
	})
	if d := recorded(reruns.ID, "build-pod"); d.Task != "build" || d.Node != "node-1" {
		t.Fatalf("recorded %+v", d)
	}

	// A pod disrupted while the run runs is recorded too, and stays known once it is gone.
	test, err := pods.Get(ctx, "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	test.Status.Conditions = append(test.Status.Conditions, corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "DeletionByPodGC"})
	if _, err := pods.UpdateStatus(ctx, test, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually("a pod disrupted later is recorded", func() bool { return recorded(reruns.ID, "test-pod").Reason == "DeletionByPodGC" })
	if err := pods.Delete(ctx, "test-pod", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	lookup := h.r.disruptionLookup(h.object(reruns.ID))
	if d := lookup("test-pod"); d == nil || d.Reason != "DeletionByPodGC" {
		t.Fatalf("a recorded pod that is gone: %+v", d)
	}

	if a := h.object(plain.ID).GetAnnotations(); a[disruptedKey("plain-pod")] != "" {
		t.Fatalf("a run without reruns records no pod: %v", a)
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if h.r.podWatch.Load() != nil {
		t.Fatalf("a stopped Watch watches no pods")
	}
}

// TestPodWatchRemembers keeps a disrupted pod known from the moment it is seen, even when its record
// on the run fails or is not read yet.
func TestPodWatchRemembers(t *testing.T) {
	h := newRunnerHarness(t)
	spec := demoSpec(pushTrigger(shaA))
	spec.Reruns = &ci.Reruns{Left: 1}
	run := h.create(spec, 1)
	pw := newPodWatch(context.Background(), h.r, nil)
	pod := disruptedPod("build-pod", run.ID.Name, "build", "node-1", "True", "EvictionByEvictionAPI")
	d, _ := disruptionOf(pod)
	pw.remember(pod, d)
	h.r.podWatch.Store(pw)
	defer h.r.podWatch.Store(nil)
	if got := h.r.disruptionLookup(h.object(run.ID))("build-pod"); got == nil || got.Reason != "EvictionByEvictionAPI" {
		t.Fatalf("a pod seen disrupted: %+v", got)
	}
	if got := h.r.disruptionLookup(h.object(run.ID))("test-pod"); got != nil {
		t.Fatalf("a pod not seen: %+v", got)
	}
	if got := h.r.disruptionLookup(h.object(run.ID))(""); got != nil {
		t.Fatalf("no pod: %+v", got)
	}
}

// heldRuns holds the first lookup of a run until released, as a pod's handler would be held when
// the watch stops.
type heldRuns struct {
	cache.Indexer
	once              sync.Once
	entered, released chan struct{}
}

func (r *heldRuns) GetByKey(key string) (any, bool, error) {
	r.once.Do(func() {
		close(r.entered)
		<-r.released
	})
	return r.Indexer.GetByKey(key)
}

// TestPodWatchStopsWhileHandlingAPod stops the watch while a pod's handler runs: the handler goes on
// to take the watch's lock, so stopping must not hold it.
func TestPodWatchStopsWhileHandlingAPod(t *testing.T) {
	h := newRunnerHarness(t)
	spec := demoSpec(pushTrigger(shaA))
	spec.Reruns = &ci.Reruns{Left: 1}
	run := h.create(spec, 1)
	runs := &heldRuns{Indexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}), entered: make(chan struct{}), released: make(chan struct{})}
	if err := runs.Add(h.object(run.ID)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pw := newPodWatch(ctx, h.r, runs)
	pw.watch(demoNS)
	pod := disruptedPod("build-pod", run.ID.Name, "build", "node-1", "True", "TerminationByKubelet")
	if _, err := h.kube.CoreV1().Pods(demoNS).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	<-runs.entered
	cancel()
	stopped := make(chan struct{})
	go func() { pw.stop(); close(stopped) }()
	time.Sleep(50 * time.Millisecond)
	close(runs.released)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatalf("stop waits on a pod's handler that waits on stop")
	}
	if _, ok := pw.pod(demoNS, "build-pod"); !ok {
		t.Fatalf("the handler ran to its end")
	}
	pw.watch(demoNS + "-other")
	if pw.factories[demoNS+"-other"] != nil {
		t.Fatalf("a stopped watch watches no new namespace")
	}
}

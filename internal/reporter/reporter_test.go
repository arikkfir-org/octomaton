package reporter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/githubapp/githubtest"
	"octomaton.dev/internal/system/metrics/metricstest"
	"octomaton.dev/internal/tekton"
)

const (
	appID          = 42
	installationID = 7
	fullName       = "octo-org/demo"
	ns             = "ci-demo"
	sha            = "1111111111111111111111111111111111111111"
)

type harness struct {
	t        *testing.T
	gh       *githubtest.Server
	dyn      *dynamicfake.FakeDynamicClient
	runs     *tekton.Client
	rep      *Reporter
	now      time.Time
	mu       sync.Mutex
	resumed  []string
	released []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, now: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	h.gh = githubtest.NewServer(t, appID)
	app, err := githubapp.New(appID, githubtest.Key(), githubapp.WithBaseURL(h.gh.URL))
	if err != nil {
		t.Fatal(err)
	}
	h.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		tekton.PipelineRuns: "PipelineRunList",
		tekton.TaskRuns:     "TaskRunList",
	})
	h.runs = &tekton.Client{Dynamic: h.dyn, Kube: kubefake.NewSimpleClientset()}
	h.rep = &Reporter{
		Dynamic:      h.dyn,
		Runs:         h.runs,
		GitHub:       app,
		DashboardURL: "https://tekton.example",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      metricstest.New(t).Metrics,
		Now:          func() time.Time { return h.now },
		Resume: func(_ context.Context, run *unstructured.Unstructured) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.resumed = append(h.resumed, run.GetName())
			return nil
		},
		ReleaseNext: func(_ context.Context, run *unstructured.Unstructured) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.released = append(h.released, run.GetName())
			return nil
		},
	}
	return h
}

func testContext() checkrun.Context {
	return checkrun.Context{
		Version: checkrun.ContextVersion, Event: checkrun.EventPush, InstallationID: installationID, Revision: sha,
		Repository: checkrun.Repository{ID: 1, Owner: "octo-org", Name: "demo", FullName: fullName},
		Ref:        "refs/heads/main", Branch: "main", Sender: "alice", Pipeline: "ci",
	}
}

// newRun creates a PipelineRun with a check run; opts adjust it before creation.
func (h *harness) newRun(name string, c checkrun.Context, opts func(*unstructured.Unstructured)) (*unstructured.Unstructured, int64) {
	h.t.Helper()
	checkID := h.gh.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: c.Pipeline, HeadSHA: sha, Status: "queued"})
	ctxJSON, _ := json.Marshal(c)
	pr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": tekton.APIVersion, "kind": tekton.KindPipelineRun,
		"metadata": map[string]any{
			"name": name, "namespace": ns, "creationTimestamp": h.now.Add(-time.Minute).Format(time.RFC3339),
			"labels": map[string]any{tekton.LabelManagedBy: tekton.ManagedByValue, tekton.LabelPipeline: c.Pipeline, tekton.LabelEvent: c.Event},
			"annotations": map[string]any{
				tekton.AnnotationRepository: fullName, tekton.AnnotationSHA: sha, tekton.AnnotationInstallationID: strconv.Itoa(installationID),
				tekton.AnnotationCheckRunID: strconv.FormatInt(checkID, 10), tekton.AnnotationReported: tekton.ReportedQueued,
				tekton.AnnotationContext: string(ctxJSON),
			},
		},
		"spec": map[string]any{"pipelineSpec": map[string]any{"tasks": []any{
			map[string]any{"name": "build"}, map[string]any{"name": "test"},
		}}},
	}}
	if opts != nil {
		opts(pr)
	}
	created, err := h.dyn.Resource(tekton.PipelineRuns).Namespace(ns).Create(context.Background(), pr, metav1.CreateOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return created, checkID
}

func (h *harness) taskRun(run, task string, status map[string]any) {
	h.t.Helper()
	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": tekton.APIVersion, "kind": "TaskRun",
		"metadata": map[string]any{"name": run + "-" + task, "namespace": ns,
			"labels": map[string]any{"tekton.dev/pipelineRun": run, "tekton.dev/pipelineTask": task}},
		"status": status,
	}}
	if _, err := h.dyn.Resource(tekton.TaskRuns).Namespace(ns).Create(context.Background(), tr, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) get(name string) *unstructured.Unstructured {
	h.t.Helper()
	pr, err := h.runs.Get(context.Background(), ns, name)
	if err != nil || pr == nil {
		h.t.Fatalf("get %s: %v", name, err)
	}
	return pr
}

func (h *harness) reconcile(name string) time.Duration {
	h.t.Helper()
	after, err := h.rep.Reconcile(context.Background(), h.get(name))
	if err != nil {
		h.t.Fatalf("Reconcile(%s): %v", name, err)
	}
	return after
}

func (h *harness) check(id int64) githubtest.CheckRun {
	h.t.Helper()
	cr, ok := h.gh.CheckRun(id)
	if !ok {
		h.t.Fatalf("check run %d not found", id)
	}
	return cr
}

func condition(status, reason, message string) []any {
	return []any{map[string]any{"type": "Succeeded", "status": status, "reason": reason, "message": message}}
}

func started(at time.Time) map[string]any {
	return map[string]any{"startTime": at.Format(time.RFC3339), "conditions": condition("Unknown", "Running", "")}
}

func finished(status, reason, message string, start, end time.Time) map[string]any {
	return map[string]any{"startTime": start.Format(time.RFC3339), "completionTime": end.Format(time.RFC3339), "conditions": condition(status, reason, message)}
}

func setStatus(pr *unstructured.Unstructured, status map[string]any) { pr.Object["status"] = status }

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("%q does not contain %q", got, w)
		}
	}
}

func TestHeldRuns(t *testing.T) {
	h := newHarness(t)
	h.newRun("young", testContext(), func(pr *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(pr.Object, tekton.SpecStatusPending, "spec", "status")
	})
	if after := h.reconcile("young"); after != 4*time.Minute || len(h.resumed) != 0 {
		t.Fatalf("a run held for a minute is looked at again in 4 minutes (got %v), not resumed (%v)", after, h.resumed)
	}
	h.newRun("old", testContext(), func(pr *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(pr.Object, tekton.SpecStatusPending, "spec", "status")
		pr.SetCreationTimestamp(metav1.NewTime(h.now.Add(-6 * time.Minute)))
	})
	if after := h.reconcile("old"); after != defaultHeldTooLong || len(h.resumed) != 1 || h.resumed[0] != "old" {
		t.Fatalf("a run held too long is resumed and polled again: after %v, resumed %v", after, h.resumed)
	}
}

func TestInProgressAndProgress(t *testing.T) {
	h := newHarness(t)
	start := h.now.Add(-90 * time.Second)
	_, checkID := h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) { setStatus(pr, started(start)) })

	h.reconcile("r1")
	cr := h.check(checkID)
	if cr.Status != "in_progress" || cr.StartedAt != start.Format(time.RFC3339) || cr.Title != "Running" {
		t.Fatalf("check after start = %+v", cr)
	}
	if h.get("r1").GetAnnotations()[tekton.AnnotationReported] != tekton.ReportedInProgress {
		t.Fatalf("reported state must advance to in_progress")
	}

	h.taskRun("r1", "build", finished("True", "Succeeded", "", start, start.Add(30*time.Second)))
	h.taskRun("r1", "test", map[string]any{"startTime": start.Add(31 * time.Second).Format(time.RFC3339), "conditions": condition("Unknown", "Running", "")})
	h.reconcile("r1")
	cr = h.check(checkID)
	if cr.Title != "1 of 2 · test · 1m30s" {
		t.Fatalf("progress title = %q", cr.Title)
	}
	mustContain(t, cr.Summary, "| `build` | ✅ Succeeded | 30s |", "| `test` | ⏳ Running |", "[`ci-demo/r1`](https://tekton.example/#/namespaces/ci-demo/pipelineruns/r1)")
	updates := cr.Updates
	h.now = h.now.Add(10 * time.Second)
	h.reconcile("r1")
	if h.check(checkID).Updates != updates {
		t.Fatalf("an unchanged table must not be written again")
	}
	if _, found, _ := checkrun.DecodeMarker(h.check(checkID).Text); !found {
		t.Fatalf("the check keeps its trigger context")
	}
}

func TestFinishSuccess(t *testing.T) {
	h := newHarness(t)
	start := h.now.Add(-10 * time.Minute)
	_, checkID := h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) {
		setStatus(pr, finished("True", "Succeeded", "Tasks Completed: 2", start, start.Add(3*time.Minute+12*time.Second)))
		pr.Object["status"].(map[string]any)["skippedTasks"] = []any{map[string]any{"name": "deploy", "reason": "When Expressions evaluated to false"}}
	})
	h.taskRun("r1", "build", finished("True", "Succeeded", "", start, start.Add(time.Minute)))
	h.taskRun("r1", "test", finished("True", "Succeeded", "", start.Add(time.Minute), start.Add(3*time.Minute)))

	h.reconcile("r1")
	cr := h.check(checkID)
	if cr.Status != "completed" || cr.Conclusion != "success" || cr.Title != "Succeeded in 3m12s" || cr.CompletedAt != start.Add(3*time.Minute+12*time.Second).Format(time.RFC3339) {
		t.Fatalf("check = %+v", cr)
	}
	mustContain(t, cr.Summary, "**Trigger:** Push to `main`", "| `build` | ✅ Succeeded | 1m0s |", "| `test` | ✅ Succeeded | 2m0s |", "| `deploy` | ⬜ Skipped (When Expressions evaluated to false) |")
	run := h.get("r1")
	if run.GetLabels()[tekton.LabelDone] != "true" || run.GetAnnotations()[tekton.AnnotationReported] != tekton.ReportedCompleted {
		t.Fatalf("a finished run is let go: %v %v", run.GetLabels(), run.GetAnnotations())
	}
	if len(h.released) != 1 || h.released[0] != "r1" {
		t.Fatalf("a finished run releases the next queued run: %v", h.released)
	}
	updates := cr.Updates
	h.reconcile("r1")
	if h.check(checkID).Updates != updates {
		t.Fatalf("a completed run is reported once")
	}
}

func TestFinishFailureIncludesLogs(t *testing.T) {
	h := newHarness(t)
	start := h.now.Add(-time.Hour)
	_, checkID := h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) {
		setStatus(pr, finished("False", "Failed", "Tasks Completed: 2 (Failed: 1, Cancelled 0), Skipped: 0", start, start.Add(time.Minute)))
	})
	h.taskRun("r1", "build", map[string]any{
		"startTime": start.Format(time.RFC3339), "completionTime": start.Add(time.Minute).Format(time.RFC3339), "podName": "r1-build-pod",
		"conditions": condition("False", "Failed", "\"step-compile\" exited with code 2"),
		"steps":      []any{map[string]any{"name": "compile", "container": "step-compile", "terminated": map[string]any{"exitCode": int64(2)}}},
	})
	h.reconcile("r1")
	cr := h.check(checkID)
	if cr.Conclusion != "failure" || cr.Title != "Failed after 1m0s" {
		t.Fatalf("check = %+v", cr)
	}
	mustContain(t, cr.Summary, "Failed: `build`.", "> Tasks Completed: 2 (Failed: 1, Cancelled 0), Skipped: 0", "| `test` | ⬜ Pending |")
	mustContain(t, cr.Text, "### build › compile (exit code 2)", "```text\nfake logs\n```")
	if _, found, _ := checkrun.DecodeMarker(cr.Text); !found {
		t.Fatalf("the log text must keep the trigger context marker")
	}
}

func TestFinishOutcomes(t *testing.T) {
	start := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		status         map[string]any
		annotations    map[string]string
		results        []any
		wantConclusion string
		wantTitle      string
		wantSummary    []string
	}{
		{
			name: "superseded by a newer commit", status: finished("False", "CancelledRunFinally", "", start, start.Add(time.Minute)),
			annotations:    map[string]string{tekton.AnnotationSupersededBy: "head:2222222222222222222222222222222222222222"},
			wantConclusion: "skipped", wantTitle: "Superseded", wantSummary: []string{"Superseded by a newer commit, `2222222`."},
		},
		{
			name: "superseded by a newer run", status: finished("False", "Cancelled", "", start, start.Add(time.Minute)),
			annotations:    map[string]string{tekton.AnnotationSupersededBy: "demo-ci-2222222-1"},
			wantConclusion: "skipped", wantTitle: "Superseded", wantSummary: []string{"Superseded by a newer run, [`demo-ci-2222222-1`]"},
		},
		{
			name: "cancelled by Octomaton", status: finished("False", "Cancelled", "", start, start.Add(time.Minute)),
			annotations:    map[string]string{tekton.AnnotationCancelReason: "merge group destroyed (dequeued)"},
			wantConclusion: "cancelled", wantTitle: "Cancelled after 1m0s", wantSummary: []string{"Cancelled by Octomaton: merge group destroyed (dequeued)."},
		},
		{
			name: "timed out", status: finished("False", "PipelineRunTimeout", "PipelineRun timed out", start, start.Add(time.Hour)),
			wantConclusion: "timed_out", wantTitle: "Timed out after 1h0m0s", wantSummary: []string{"> PipelineRun timed out"},
		},
		{
			name: "results override title and summary", status: finished("True", "Succeeded", "", start, start.Add(time.Minute)),
			results:        []any{map[string]any{"name": "check-title", "value": "3 tests passed"}, map[string]any{"name": "check-summary", "value": "See **the report**."}},
			wantConclusion: "success", wantTitle: "3 tests passed", wantSummary: []string{"See **the report**.\n\n**PipelineRun:**"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			_, checkID := h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) {
				setStatus(pr, tt.status)
				if tt.results != nil {
					pr.Object["status"].(map[string]any)["results"] = tt.results
				}
				ann := pr.GetAnnotations()
				for k, v := range tt.annotations {
					ann[k] = v
				}
				pr.SetAnnotations(ann)
			})
			h.reconcile("r1")
			cr := h.check(checkID)
			if cr.Conclusion != tt.wantConclusion || cr.Title != tt.wantTitle {
				t.Fatalf("check = %s / %q, want %s / %q", cr.Conclusion, cr.Title, tt.wantConclusion, tt.wantTitle)
			}
			mustContain(t, cr.Summary, tt.wantSummary...)
		})
	}
}

func TestFailedTaskResultsAreRecovered(t *testing.T) {
	h := newHarness(t)
	start := h.now.Add(-time.Hour)
	_, checkID := h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) {
		setStatus(pr, finished("False", "Failed", "", start, start.Add(time.Minute)))
		_ = unstructured.SetNestedSlice(pr.Object, []any{map[string]any{"name": "check-summary", "value": "$(tasks.test.results.summary)"}}, "spec", "pipelineSpec", "results")
	})
	h.taskRun("r1", "test", map[string]any{
		"conditions": condition("False", "Failed", ""), "results": []any{map[string]any{"name": "summary", "value": "2 of 10 tests failed"}},
	})
	h.reconcile("r1")
	mustContain(t, h.check(checkID).Summary, "2 of 10 tests failed")
}

func TestCommentRunsAreAnswered(t *testing.T) {
	h := newHarness(t)
	c := testContext()
	c.Event = checkrun.EventComment
	c.PullRequest = &checkrun.PullRequest{Number: 5}
	c.Comment = &checkrun.Comment{ID: 9, Author: "maintainer", Command: "/deploy", Arguments: "staging"}
	start := h.now.Add(-time.Hour)
	h.newRun("r1", c, func(pr *unstructured.Unstructured) {
		setStatus(pr, finished("True", "Succeeded", "", start, start.Add(time.Minute)))
	})
	h.reconcile("r1")
	comments := h.gh.Comments()
	if len(comments) != 1 || comments[0].Number != 5 || !strings.HasPrefix(comments[0].Body, "@maintainer ✅ `/deploy staging`: Succeeded in 1m0s") {
		t.Fatalf("comments = %+v", comments)
	}
	h.reconcile("r1")
	if len(h.gh.Comments()) != 1 {
		t.Fatalf("a comment is answered once")
	}
}

func TestTaskChecks(t *testing.T) {
	h := newHarness(t)
	start := h.now.Add(-time.Hour)
	build := h.gh.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci / build", HeadSHA: sha, Status: "queued"})
	test := h.gh.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci / test", HeadSHA: sha, Status: "queued"})
	ids, _ := json.Marshal(map[string]int64{"build": build, "test": test})
	h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) {
		setStatus(pr, started(start))
		ann := pr.GetAnnotations()
		ann[tekton.AnnotationTaskCheckIDs] = string(ids)
		pr.SetAnnotations(ann)
	})
	h.taskRun("r1", "build", map[string]any{"startTime": start.Format(time.RFC3339), "conditions": condition("Unknown", "Running", "")})
	h.reconcile("r1")
	if cr := h.check(build); cr.Status != "in_progress" {
		t.Fatalf("a started task's check is in progress: %+v", cr)
	}
	if cr := h.check(test); cr.Status != "queued" {
		t.Fatalf("a task not started stays queued: %+v", cr)
	}

	// The run fails in build; test never starts.
	pr := h.get("r1")
	setStatus(pr, finished("False", "Failed", "", start, start.Add(time.Minute)))
	if _, err := h.dyn.Resource(tekton.PipelineRuns).Namespace(ns).Update(context.Background(), pr, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	tr, _ := h.dyn.Resource(tekton.TaskRuns).Namespace(ns).Get(context.Background(), "r1-build", metav1.GetOptions{})
	tr.Object["status"] = map[string]any{
		"startTime": start.Format(time.RFC3339), "completionTime": start.Add(time.Minute).Format(time.RFC3339),
		"conditions": condition("False", "Failed", "boom"),
		"results":    []any{map[string]any{"name": "check-title", "value": "Compilation failed"}},
	}
	if _, err := h.dyn.Resource(tekton.TaskRuns).Namespace(ns).Update(context.Background(), tr, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.reconcile("r1")
	if cr := h.check(build); cr.Status != "completed" || cr.Conclusion != "failure" || cr.Title != "Compilation failed" {
		t.Fatalf("the failed task's check = %+v", cr)
	}
	mustContain(t, h.check(build).Summary, "boom.", "[The task on the Dashboard](https://tekton.example/#/namespaces/ci-demo/pipelineruns/r1?pipelineTask=build)")
	if cr := h.check(test); cr.Status != "completed" || cr.Conclusion != "cancelled" || cr.Title != "Not run" {
		t.Fatalf("a task that never ran in a failed run = %+v", cr)
	}
	states := h.get("r1").GetAnnotations()[tekton.AnnotationTaskCheckStates]
	mustContain(t, states, `"build":"completed"`, `"test":"completed"`)
}

func TestTaskChecksOfAPassingRun(t *testing.T) {
	h := newHarness(t)
	start := h.now.Add(-time.Hour)
	test := h.gh.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci / test", HeadSHA: sha, Status: "queued"})
	ids, _ := json.Marshal(map[string]int64{"test": test})
	h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) {
		setStatus(pr, finished("True", "Completed", "", start, start.Add(time.Minute)))
		ann := pr.GetAnnotations()
		ann[tekton.AnnotationTaskCheckIDs] = string(ids)
		pr.SetAnnotations(ann)
	})
	h.reconcile("r1")
	if cr := h.check(test); cr.Conclusion != "skipped" || cr.Title != "Skipped" {
		t.Fatalf("a task a passing run skipped = %+v", cr)
	}
}

func TestDeletedRuns(t *testing.T) {
	h := newHarness(t)
	run, checkID := h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) { setStatus(pr, started(h.now)) })
	if err := h.rep.ReportDeleted(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if cr := h.check(checkID); cr.Conclusion != "cancelled" {
		t.Fatalf("a run deleted before it finished is cancelled: %+v", cr)
	}
	done, doneID := h.newRun("r2", testContext(), func(pr *unstructured.Unstructured) {
		ann := pr.GetAnnotations()
		ann[tekton.AnnotationReported] = tekton.ReportedCompleted
		pr.SetAnnotations(ann)
	})
	_ = h.rep.ReportDeleted(context.Background(), done)
	if cr := h.check(doneID); cr.Updates != 0 {
		t.Fatalf("a reported run's deletion changes nothing")
	}
}

func TestRunWithoutCheckIsLetGo(t *testing.T) {
	h := newHarness(t)
	start := h.now.Add(-time.Hour)
	h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) {
		setStatus(pr, finished("False", "Cancelled", "", start, start))
		ann := pr.GetAnnotations()
		delete(ann, tekton.AnnotationCheckRunID)
		pr.SetAnnotations(ann)
	})
	h.reconcile("r1")
	if h.get("r1").GetLabels()[tekton.LabelDone] != "true" {
		t.Fatalf("a finished run without a check is let go")
	}
}

func TestTitle(t *testing.T) {
	tests := []struct {
		o    tekton.Outcome
		d    time.Duration
		want string
	}{
		{tekton.Outcome{Status: tekton.StatusInProgress}, time.Minute, "Running"},
		{tekton.Outcome{Status: tekton.StatusCompleted, Conclusion: tekton.ConclusionSuccess, Title: "Succeeded"}, 192 * time.Second, "Succeeded in 3m12s"},
		{tekton.Outcome{Status: tekton.StatusCompleted, Conclusion: tekton.ConclusionFailure, Title: "Failed"}, 500 * time.Millisecond, "Failed after <1s"},
		{tekton.Outcome{Status: tekton.StatusCompleted, Conclusion: tekton.ConclusionSkipped, Title: "Superseded"}, time.Minute, "Superseded"},
	}
	for _, tt := range tests {
		if got := Title(tt.o, tt.d); got != tt.want {
			t.Errorf("Title(%+v, %v) = %q, want %q", tt.o, tt.d, got, tt.want)
		}
	}
	if codeFence("no backticks") != "```" || codeFence("a ```` b") != "`````" {
		t.Fatalf("codeFence must be longer than any backtick run")
	}
}

func TestRunWatchesAndReports(t *testing.T) {
	h := newHarness(t)
	h.rep.Resync = time.Minute
	start := h.now.Add(-time.Hour)
	_, checkID := h.newRun("r1", testContext(), func(pr *unstructured.Unstructured) {
		setStatus(pr, finished("True", "Succeeded", "", start, start.Add(time.Minute)))
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.rep.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for h.check(checkID).Status != "completed" {
		if time.Now().After(deadline) {
			t.Fatalf("the reporter did not report the finished run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !h.rep.Synced() {
		t.Fatalf("the reporter must report its informer as synced while running")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.rep.Synced() {
		t.Fatalf("a stopped reporter is not synced")
	}
}

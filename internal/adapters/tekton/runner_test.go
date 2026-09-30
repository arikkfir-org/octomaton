package tekton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"octomaton.dev/internal/services/ci"
)

const (
	demoNS  = "ci-demo"
	shaA    = "1111111111111111111111111111111111111111"
	shaB    = "2222222222222222222222222222222222222222"
	demoDef = `
apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  generateName: ci-
spec:
  params:
    - {name: revision, value: ""}
  pipelineSpec:
    tasks:
      - {name: build, taskSpec: {steps: [{name: b, image: alpine, script: "true"}]}}
      - {name: test, taskSpec: {steps: [{name: t, image: alpine, script: "true"}]}}
`
)

var demo = ci.Repository{ID: 1001, Owner: "octo-org", Name: "demo", FullName: "octo-org/demo"}

type runnerHarness struct {
	t     *testing.T
	dyn   *dynamicfake.FakeDynamicClient
	kube  *kubefake.Clientset
	r     *Runner
	mu    sync.Mutex
	clock time.Time
	uid   int
}

func newRunnerHarness(t *testing.T) *runnerHarness {
	t.Helper()
	h := &runnerHarness{t: t, clock: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	h.kube = kubefake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: demoNS}})
	h.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		PipelineRuns: "PipelineRunList",
		TaskRuns:     "TaskRunList",
	})
	// Give created objects what the API server would: a UID and a creation time.
	h.dyn.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.uid++
		h.clock = h.clock.Add(time.Second)
		obj.SetUID(types.UID(fmt.Sprintf("uid-%d", h.uid)))
		obj.SetCreationTimestamp(metav1.NewTime(h.clock))
		return false, nil, nil
	})
	namespaces, err := NewNamespaces("ci-{{ .Repository.Name }}", nil)
	if err != nil {
		t.Fatal(err)
	}
	h.r = &Runner{
		Dynamic: h.dyn, Kube: h.kube, Namespaces: namespaces, DashboardURL: "https://tekton.example",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Resync: time.Minute,
	}
	return h
}

func pushTrigger(sha string) ci.Trigger {
	return ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventPush, DeliveryID: "d-" + sha[:4], InstallationID: 7, Repository: demo,
		Revision: sha, Ref: "refs/heads/main", Branch: "main", Sender: "alice", Pipeline: "ci",
		Push: &ci.Push{Before: shaB, After: sha},
	}
}

func demoSpec(t ci.Trigger) ci.RunSpec {
	return ci.RunSpec{
		Trigger: t, Definition: []byte(demoDef), Path: ".tekton/ci.yaml",
		Params: map[string]string{"revision": t.Revision}, Timeout: time.Hour,
		Token:       &ci.TokenSettings{Workspace: "github-token", Permissions: map[string]string{"contents": "read"}},
		TaskReports: true,
		Concurrency: ci.Concurrency{Group: "main", Key: "main", Policy: ci.Queue},
	}
}

func (h *runnerHarness) create(spec ci.RunSpec, attempt int) ci.Run {
	h.t.Helper()
	run, err := h.r.Create(context.Background(), spec, attempt)
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	return run
}

func (h *runnerHarness) object(id ci.RunID) *unstructured.Unstructured {
	h.t.Helper()
	pr, err := h.dyn.Resource(PipelineRuns).Namespace(id.Tenant).Get(context.Background(), id.Name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return pr
}

func (h *runnerHarness) update(pr *unstructured.Unstructured) {
	h.t.Helper()
	if _, err := h.dyn.Resource(PipelineRuns).Namespace(pr.GetNamespace()).Update(context.Background(), pr, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *runnerHarness) get(id ci.RunID) ci.Run {
	h.t.Helper()
	run, err := h.r.Get(context.Background(), id)
	if err != nil {
		h.t.Fatalf("Get(%s): %v", id, err)
	}
	return run
}

func TestCreateRecordsTheRun(t *testing.T) {
	h := newRunnerHarness(t)
	spec := demoSpec(pushTrigger(shaA))
	run := h.create(spec, 1)
	pr := h.object(run.ID)

	wantLabels := map[string]string{
		labelManagedBy: managedByValue, labelPipeline: "ci", labelEvent: "push", labelRepositoryID: "1001", labelSHA: shaA,
		labelConcurrencyGroup: groupLabel("octo-org/demo", "main"),
	}
	if !reflect.DeepEqual(pr.GetLabels(), wantLabels) {
		t.Errorf("labels = %v\nwant %v", pr.GetLabels(), wantLabels)
	}
	a := pr.GetAnnotations()
	for key, want := range map[string]string{
		annotationRepository: "octo-org/demo", annotationSHA: shaA, annotationInstallationID: "7", annotationDeliveryID: "d-1111",
		annotationHead: "main", annotationAttempt: "1", annotationConcurrencyGroup: "main", annotationConcurrencyPolicy: "queue",
		annotationToken: `{"workspace":"github-token","permissions":{"contents":"read"}}`, annotationTaskChecks: "true",
	} {
		if a[key] != want {
			t.Errorf("annotation %s = %q, want %q", key, a[key], want)
		}
	}
	if !isPending(pr) || pr.GetName() != "demo-ci-1111111-1" || pr.GetNamespace() != demoNS {
		t.Errorf("created %s/%s, held %v", pr.GetNamespace(), pr.GetName(), isPending(pr))
	}
	workspaces, _, _ := unstructured.NestedSlice(pr.Object, "spec", "workspaces")
	if len(workspaces) != 1 || workspaces[0].(map[string]any)["secret"].(map[string]any)["secretName"] != "demo-ci-1111111-1-github-token" {
		t.Errorf("token workspace = %v", workspaces)
	}

	want := ci.Run{
		ID: ci.RunID{Tenant: demoNS, Name: "demo-ci-1111111-1"}, Trigger: spec.Trigger, Attempt: 1,
		Group: groupLabel("octo-org/demo", "main"), Policy: ci.Queue, Token: spec.Token, TaskReports: true,
		Tasks: []string{"build", "test"}, Phase: ci.Held, Created: pr.GetCreationTimestamp().Time.UTC(),
	}
	if !reflect.DeepEqual(run, want) {
		t.Fatalf("Create returned %+v\nwant %+v", run, want)
	}
	if got := h.get(run.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("Get returned %+v\nwant %+v", got, want)
	}
}

func TestCreateTakenAttempt(t *testing.T) {
	h := newRunnerHarness(t)
	first := h.create(demoSpec(pushTrigger(shaA)), 1)
	got, err := h.r.Create(context.Background(), demoSpec(pushTrigger(shaA)), 1)
	if !errors.Is(err, ci.ErrExists) || got.ID != first.ID {
		t.Fatalf("Create of a taken attempt = %+v, %v; want ErrExists with the run holding the name", got.ID, err)
	}
	second := h.create(demoSpec(pushTrigger(shaA)), 2)
	if second.ID.Name != "demo-ci-1111111-2" || second.Attempt != 2 {
		t.Fatalf("attempt 2 = %+v", second)
	}
}

func TestOccasionLabels(t *testing.T) {
	comment := pushTrigger(shaA)
	comment.Event, comment.Comment = ci.EventComment, &ci.Comment{ID: 99, Author: "alice", Command: "/deploy"}
	schedule := pushTrigger(shaA)
	schedule.Event, schedule.Schedule = ci.EventSchedule, &ci.Schedule{Cron: "0 3 * * *", Slot: "2026-01-01T03:00:00Z"}
	tests := []struct {
		name    string
		trigger ci.Trigger
		label   string
		want    string
	}{
		{"a comment command", comment, labelComment, "99"},
		{"a schedule slot", schedule, labelSlot, "1767236400"},
		{"a push has neither", pushTrigger(shaA), labelComment, ""},
	}
	for _, tt := range tests {
		if got := runLabels(ci.RunSpec{Trigger: tt.trigger})[tt.label]; got != tt.want {
			t.Errorf("%s: label %s = %q, want %q", tt.name, tt.label, got, tt.want)
		}
	}
}

func TestRefusals(t *testing.T) {
	other := pushTrigger(shaA)
	other.Repository = ci.Repository{ID: 2, Owner: "octo-org", Name: "other", FullName: "octo-org/other"}
	withDefinition := func(def string) ci.RunSpec {
		spec := demoSpec(pushTrigger(shaA))
		spec.Definition = []byte(def)
		return spec
	}
	noTasks := withDefinition("apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: {pipelineRef: {name: p}}\n")
	noTasks.Token = nil
	tests := []struct {
		name      string
		spec      ci.RunSpec
		setup     func(h *runnerHarness)
		create    bool
		wantTitle string
		wantText  string
	}{
		{name: "a repository without its namespace", spec: demoSpec(other), wantTitle: "Could not start the pipeline", wantText: "repository not onboarded: namespace ci-other not found"},
		{name: "namespaces that cannot be read", spec: demoSpec(pushTrigger(shaA)), wantTitle: "Could not start the pipeline", wantText: "Could not verify that namespace `ci-demo` exists",
			setup: func(h *runnerHarness) {
				h.kube.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("boom") })
			}},
		{name: "an invalid file", spec: withDefinition("kind: TaskRun\n"), wantTitle: "Could not start the pipeline", wantText: "`.tekton/ci.yaml` is not a valid PipelineRun file"},
		{name: "another namespace", spec: withDefinition(strings.Replace(demoDef, "generateName: ci-", "namespace: prod", 1)), wantTitle: "Could not start the pipeline", wantText: "sets namespace `prod`"},
		{name: "task reports without tasks", spec: func() ci.RunSpec { s := noTasks; s.TaskReports = true; return s }(), wantTitle: "Could not start the pipeline", wantText: "sets `taskChecks`"},
		{name: "a mounted Secret", spec: func() ci.RunSpec {
			s := withDefinition(demoDef + "  workspaces:\n    - {name: creds, secret: {secretName: prod-db}}\n")
			return s
		}(), create: true, wantTitle: "Refused", wantText: `references Secret "prod-db"`},
		{name: "an API server that refuses the run", spec: demoSpec(pushTrigger(shaA)), create: true, wantTitle: "Could not create the PipelineRun", wantText: "Kubernetes refused PipelineRun `ci-demo/demo-ci-1111111-1`",
			setup: func(h *runnerHarness) {
				h.dyn.PrependReactor("create", "pipelineruns", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pipelineruns"}, "x", errors.New("quota"))
				})
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			var err error
			if tt.create {
				if err = h.r.Check(context.Background(), tt.spec); err != nil {
					t.Fatalf("Check: %v", err)
				}
				_, err = h.r.Create(context.Background(), tt.spec, 1)
			} else {
				err = h.r.Check(context.Background(), tt.spec)
			}
			var refusal *ci.Refusal
			if !errors.As(err, &refusal) || refusal.Title != tt.wantTitle || !strings.Contains(refusal.Reason, tt.wantText) {
				t.Fatalf("error = %v, want a refusal %q mentioning %q", err, tt.wantTitle, tt.wantText)
			}
		})
	}
	h := newRunnerHarness(t)
	if err := h.r.Check(context.Background(), demoSpec(pushTrigger(shaA))); err != nil {
		t.Fatalf("Check of a sound run: %v", err)
	}
}

func TestRunStates(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	id := h.create(demoSpec(pushTrigger(shaA)), 1).ID
	setStatus := func(status map[string]any) {
		pr := h.object(id)
		pr.Object["status"] = status
		h.update(pr)
	}
	start, end := "2026-05-01T09:00:00Z", "2026-05-01T09:10:00Z"
	finished := func(status, reason string) map[string]any {
		return map[string]any{"startTime": start, "completionTime": end,
			"conditions": []any{map[string]any{"type": "Succeeded", "status": status, "reason": reason, "message": "m"}}}
	}
	steps := []struct {
		name  string
		act   func()
		check func(ci.Run) bool
	}{
		{"created held", func() {}, func(r ci.Run) bool { return r.Phase == ci.Held && !r.CancelRequested }},
		{"released", func() { _ = h.r.Release(ctx, id) }, func(r ci.Run) bool { return r.Phase == ci.Released }},
		{"running", func() { setStatus(map[string]any{"startTime": start}) }, func(r ci.Run) bool {
			return r.Phase == ci.Running && r.Started.Format(time.RFC3339) == start && r.Finished.IsZero()
		}},
		{"cancelled for a newer commit", func() { _ = h.r.Cancel(ctx, id, ci.Cancellation{NewerCommit: shaB}) }, func(r ci.Run) bool {
			return r.CancelRequested && r.Cancellation == ci.Cancellation{NewerCommit: shaB} && h.object(id).GetAnnotations()[annotationSupersededBy] == "head:"+shaB
		}},
		{"finished cancelled", func() { setStatus(finished("False", "CancelledRunFinally")) }, func(r ci.Run) bool {
			return r.Phase == ci.Finished && r.Outcome == ci.Outcome{Conclusion: ci.Cancelled, Message: "m"} && r.Finished.Format(time.RFC3339) == end
		}},
	}
	for _, step := range steps {
		step.act()
		if got := h.get(id); !step.check(got) {
			t.Fatalf("%s: run = %+v", step.name, got)
		}
	}
	for _, tt := range []struct {
		reason string
		status string
		want   ci.Conclusion
	}{{"Succeeded", "True", ci.Success}, {"Failed", "False", ci.Failure}, {"PipelineRunTimeout", "False", ci.TimedOut}} {
		setStatus(finished(tt.status, tt.reason))
		if got := h.get(id).Outcome.Conclusion; got != tt.want {
			t.Errorf("%s: conclusion = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

func TestCancelAndRecord(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	id := h.create(demoSpec(pushTrigger(shaA)), 1).ID
	if err := h.r.Cancel(ctx, id, ci.Cancellation{Reason: "merge group destroyed", SupersededBy: "demo-ci-1111111-2"}); err != nil {
		t.Fatal(err)
	}
	report, reported, progress, waiting := ci.ReportID(55), ci.ReportedInProgress, "1/2 build", "demo-ci-1111111-3"
	if err := h.r.Record(ctx, id, ci.Record{
		ReportID: &report, Reported: &reported, Progress: &progress, WaitingFor: &waiting,
		TaskReportIDs: map[string]ci.ReportID{"build": 56}, TaskReportStates: map[string]ci.Status{"build": ci.StatusInProgress},
	}); err != nil {
		t.Fatal(err)
	}
	run := h.get(id)
	want := ci.Cancellation{Reason: "merge group destroyed", SupersededBy: "demo-ci-1111111-2"}
	if run.Cancellation != want || run.ReportID != 55 || run.Reported != ci.ReportedInProgress || run.Progress != progress || run.WaitingFor != waiting ||
		!reflect.DeepEqual(run.TaskReportIDs, map[string]ci.ReportID{"build": 56}) || !reflect.DeepEqual(run.TaskReportStates, map[string]ci.Status{"build": ci.StatusInProgress}) || run.Done {
		t.Fatalf("run = %+v", run)
	}
	if a := h.object(id).GetAnnotations(); a[annotationCheckRunID] != "55" || a[annotationTaskCheckIDs] != `{"build":56}` || a[annotationTaskCheckStates] != `{"build":"in_progress"}` {
		t.Fatalf("annotations = %v", a)
	}
	completed := ci.ReportedCompleted
	if err := h.r.Record(ctx, id, ci.Record{Reported: &completed, Done: true}); err != nil {
		t.Fatal(err)
	}
	if run := h.get(id); !run.Done || run.Reported != ci.ReportedCompleted || run.ReportID != 55 || h.object(id).GetLabels()[labelDone] != "true" {
		t.Fatalf("a run let go = %+v", run)
	}
}

func TestList(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	a1 := h.create(demoSpec(pushTrigger(shaA)), 1)
	a2 := h.create(demoSpec(pushTrigger(shaA)), 2)
	pr := pushTrigger(shaB)
	pr.Event, pr.PullRequest = ci.EventPullRequest, &ci.PullRequest{Number: 5}
	prSpec := demoSpec(pr)
	prSpec.Concurrency = ci.Concurrency{Group: "pr-5", Key: "ci/pr-5", Policy: ci.Supersede}
	b := h.create(prSpec, 1)
	sched := pushTrigger(shaB)
	sched.Event, sched.Pipeline, sched.Schedule = ci.EventSchedule, "nightly", &ci.Schedule{Cron: "0 3 * * *", Slot: "2026-01-01T03:00:00Z"}
	nightly := h.create(demoSpec(sched), 1)
	done := ci.ReportedCompleted
	if err := h.r.Record(ctx, a1.ID, ci.Record{Reported: &done, Done: true}); err != nil {
		t.Fatal(err)
	}
	slot, _ := time.Parse(time.RFC3339, "2026-01-01T03:00:00Z")
	repo := demo
	tests := []struct {
		name  string
		query ci.RunQuery
		want  []ci.RunID
	}{
		{"everything", ci.RunQuery{}, []ci.RunID{a1.ID, a2.ID, b.ID, nightly.ID}},
		{"one repository", ci.RunQuery{Repository: &repo}, []ci.RunID{a1.ID, a2.ID, b.ID, nightly.ID}},
		{"one commit of a pipeline", ci.RunQuery{Repository: &repo, Pipeline: "ci", Revision: shaA}, []ci.RunID{a1.ID, a2.ID}},
		{"live only", ci.RunQuery{Repository: &repo, Pipeline: "ci", Revision: shaA, Live: true}, []ci.RunID{a2.ID}},
		{"an event", ci.RunQuery{Repository: &repo, Event: ci.EventPullRequest}, []ci.RunID{b.ID}},
		{"a concurrency group", ci.RunQuery{Repository: &repo, Group: b.Group, Live: true}, []ci.RunID{b.ID}},
		{"a schedule slot", ci.RunQuery{Repository: &repo, Pipeline: "nightly", Slot: slot}, []ci.RunID{nightly.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs, err := h.r.List(ctx, tt.query)
			var got []ci.RunID
			for _, r := range runs {
				got = append(got, r.ID)
			}
			slices.SortFunc(got, func(a, b ci.RunID) int { return strings.Compare(a.Name, b.Name) })
			slices.SortFunc(tt.want, func(a, b ci.RunID) int { return strings.Compare(a.Name, b.Name) })
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("List = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestTriggerFallback(t *testing.T) {
	pr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kindPipelineRun,
		"metadata": map[string]any{"name": "r", "namespace": "ns",
			"labels":      map[string]any{labelRepositoryID: "1001", labelPipeline: "ci", labelEvent: "push"},
			"annotations": map[string]any{annotationRepository: "octo-org/demo", annotationInstallationID: "7", annotationSHA: shaA, annotationContext: `{"v":99}`},
		},
	}}
	want := ci.Trigger{Event: "push", InstallationID: 7, Repository: ci.Repository{ID: 1001, Owner: "octo-org", Name: "demo", FullName: "octo-org/demo"}, Revision: shaA, Pipeline: "ci"}
	if got := triggerOf(pr); !reflect.DeepEqual(got, want) {
		t.Fatalf("triggerOf = %+v\nwant %+v", got, want)
	}
}

func TestTokens(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	run := h.create(demoSpec(pushTrigger(shaA)), 1)
	if _, ok, err := h.r.TokenExpiry(ctx, run.ID); ok || err != nil {
		t.Fatalf("TokenExpiry before a token = %v, %v", ok, err)
	}
	first := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	if err := h.r.SetToken(ctx, run, ci.Token{Value: "t1", ExpiresAt: first, Permissions: map[string]string{"contents": "read"}}); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	secret, err := h.kube.CoreV1().Secrets(demoNS).Get(ctx, "demo-ci-1111111-1-github-token", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[tokenSecretKey]) != "t1" || len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != h.object(run.ID).GetUID() ||
		secret.Annotations[annotationRepository] != "octo-org/demo" || secret.Annotations[annotationInstallationID] != "7" || secret.Annotations[annotationPermissions] != `{"contents":"read"}` {
		t.Fatalf("token Secret = %+v", secret)
	}
	second := first.Add(time.Hour)
	if err := h.r.SetToken(ctx, run, ci.Token{Value: "t2", ExpiresAt: second}); err != nil {
		t.Fatalf("SetToken again: %v", err)
	}
	secret, _ = h.kube.CoreV1().Secrets(demoNS).Get(ctx, "demo-ci-1111111-1-github-token", metav1.GetOptions{})
	if expires, ok, err := h.r.TokenExpiry(ctx, run.ID); string(secret.Data[tokenSecretKey]) != "t2" || !ok || err != nil || !expires.Equal(second) {
		t.Fatalf("after a refresh: token %q, expiry %v, %v, %v", secret.Data[tokenSecretKey], expires, ok, err)
	}
	gone := run
	gone.ID.Name = "missing"
	if err := h.r.SetToken(ctx, gone, ci.Token{Value: "t"}); !errors.Is(err, ci.ErrNotFound) {
		t.Fatalf("SetToken of a missing run: %v", err)
	}
}

func (h *runnerHarness) taskRun(run ci.RunID, name, task string, created time.Time, status map[string]any) {
	h.t.Helper()
	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": "TaskRun",
		"metadata": map[string]any{"name": name, "namespace": run.Tenant,
			"labels": map[string]any{"tekton.dev/pipelineRun": run.Name, labelPipelineTask: task}},
		"status": status,
	}}
	h.dyn.PrependReactor("create", "taskruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured); obj.GetName() == name {
			obj.SetCreationTimestamp(metav1.NewTime(created))
		}
		return false, nil, nil
	})
	if _, err := h.dyn.Resource(TaskRuns).Namespace(run.Tenant).Create(context.Background(), tr, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func TestDetails(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	spec := demoSpec(pushTrigger(shaA))
	spec.Definition = []byte(demoDef + "    results:\n      - {name: check-summary, value: $(tasks.build.results.check-summary)}\n")
	id := h.create(spec, 1).ID
	pr := h.object(id)
	pr.Object["status"] = map[string]any{"skippedTasks": []any{map[string]any{"name": "deploy", "reason": "When Expressions evaluated to false"}}}
	h.update(pr)
	at := func(minute int) time.Time { return time.Date(2026, 5, 1, 9, minute, 0, 0, time.UTC) }
	condition := func(status, reason, message string) []any {
		return []any{map[string]any{"type": "Succeeded", "status": status, "reason": reason, "message": message}}
	}
	h.taskRun(id, "notify", "notify", at(9), map[string]any{"startTime": "2026-05-01T09:09:00Z"})
	h.taskRun(id, "build-old", "build", at(1), map[string]any{"conditions": condition("True", "Succeeded", "")})
	h.taskRun(id, "build", "build", at(2), map[string]any{
		"podName": "build-pod", "startTime": "2026-05-01T09:02:00Z", "completionTime": "2026-05-01T09:04:00Z",
		"conditions": condition("False", "Failed", "exit 2"),
		"steps":      []any{map[string]any{"name": "b", "container": "step-b", "terminated": map[string]any{"exitCode": int64(2)}}},
		"results":    []any{map[string]any{"name": "check-summary", "value": "2 tests failed"}},
	})

	details, err := h.r.Details(ctx, id)
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	want := []ci.Task{
		{Name: "build", State: ci.TaskFailed, Started: at(2), Finished: at(4), Message: "exit 2",
			FailedSteps: []ci.Step{{Name: "b", ExitCode: 2, Logs: "build-pod/step-b"}}, Results: map[string]string{"check-summary": "2 tests failed"}},
		{Name: "test", State: ci.TaskPending},
		{Name: "notify", State: ci.TaskRunning, Started: at(9), Results: map[string]string{}},
		{Name: "deploy", State: ci.TaskSkipped, Note: "When Expressions evaluated to false"},
	}
	if !reflect.DeepEqual(details.Tasks, want) {
		got, _ := json.MarshalIndent(details.Tasks, "", " ")
		t.Fatalf("tasks = %s", got)
	}
	if details.Results["check-summary"] != "2 tests failed" {
		t.Fatalf("results = %v: a failed task's result must fill the pipeline result it names", details.Results)
	}
	if _, err := h.r.Details(ctx, ci.RunID{Tenant: demoNS, Name: "missing"}); !errors.Is(err, ci.ErrNotFound) {
		t.Fatalf("Details of a missing run: %v", err)
	}
}

func TestStepLogs(t *testing.T) {
	h := newRunnerHarness(t)
	id := ci.RunID{Tenant: demoNS, Name: "r"}
	if logs, err := h.r.StepLogs(context.Background(), id, ci.Step{Name: "b", Logs: "build-pod/step-b"}, 50, 1024); err != nil || logs != "fake logs" {
		t.Fatalf("StepLogs = %q, %v", logs, err)
	}
	if _, err := h.r.StepLogs(context.Background(), id, ci.Step{Name: "b"}, 50, 1024); err == nil || !strings.Contains(err.Error(), "no pod") {
		t.Fatalf("StepLogs without a pod: %v", err)
	}
}

func TestLinks(t *testing.T) {
	h := newRunnerHarness(t)
	id := ci.RunID{Tenant: demoNS, Name: "demo-ci-1111111-1"}
	link := h.r.Link(id)
	if link != (ci.RunLink{Kind: "PipelineRun", Name: "ci-demo/demo-ci-1111111-1", URL: "https://tekton.example/#/namespaces/ci-demo/pipelineruns/demo-ci-1111111-1"}) {
		t.Fatalf("Link = %+v", link)
	}
	if got := h.r.TaskURL(id, "unit tests"); got != link.URL+"?pipelineTask=unit+tests" {
		t.Fatalf("TaskURL = %q", got)
	}
	h.r.DashboardURL = ""
	if h.r.Link(id).URL != "" || h.r.TaskURL(id, "build") != "" {
		t.Fatalf("without a dashboard, runs link nowhere")
	}
}

func TestFreeResources(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	old := h.create(demoSpec(pushTrigger(shaA)), 1)
	recent := h.create(demoSpec(pushTrigger(shaB)), 1)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	finishAt := func(id ci.RunID, at time.Time) *unstructured.Unstructured {
		pr := h.object(id)
		pr.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": "True"}}, "completionTime": at.Format(time.RFC3339)}
		labels := pr.GetLabels()
		labels[labelDone] = "true"
		pr.SetLabels(labels)
		h.update(pr)
		return pr
	}
	oldPR, recentPR := finishAt(old.ID, now.Add(-2*time.Hour)), finishAt(recent.ID, now.Add(-10*time.Minute))
	pvc := func(name string, owner *unstructured.Unstructured) {
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: demoNS}}
		if owner != nil {
			claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kindPipelineRun, Name: owner.GetName(), UID: owner.GetUID()}}
		}
		if _, err := h.kube.CoreV1().PersistentVolumeClaims(demoNS).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pvc("pvc-old", oldPR)
	pvc("pvc-recent", recentPR)
	pvc("pvc-unrelated", nil)

	n, err := h.r.FreeResources(ctx, now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("FreeResources = %d, %v; want 1", n, err)
	}
	claims, _ := h.kube.CoreV1().PersistentVolumeClaims(demoNS).List(ctx, metav1.ListOptions{})
	var names []string
	for _, c := range claims.Items {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"pvc-recent", "pvc-unrelated"}) {
		t.Fatalf("remaining PVCs = %v", names)
	}
	if h.object(old.ID).GetLabels()[labelPVCsFreed] != "true" {
		t.Fatalf("a freed run must be marked so it is not examined again")
	}
	if n, _ := h.r.FreeResources(ctx, now.Add(-time.Hour)); n != 0 {
		t.Fatalf("nothing is left to free")
	}
}

// watcher records what Watch hands it.
type watcher struct {
	mu         sync.Mutex
	reconciled map[string]int
	deleted    []string
}

func (w *watcher) Reconcile(_ context.Context, run ci.Run) (time.Duration, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reconciled[run.ID.Name]++
	return 0, nil
}

func (w *watcher) Deleted(_ context.Context, run ci.Run) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deleted = append(w.deleted, run.ID.Name)
	return nil
}

func (w *watcher) seen(name string) (reconciled int, deleted bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reconciled[name], slices.Contains(w.deleted, name)
}

func TestWatch(t *testing.T) {
	h := newRunnerHarness(t)
	live := h.create(demoSpec(pushTrigger(shaA)), 1)
	doomed := h.create(demoSpec(pushTrigger(shaB)), 1)
	w := &watcher{reconciled: map[string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
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
	eventually("every live run is handed over", func() bool {
		a, _ := w.seen(live.ID.Name)
		b, _ := w.seen(doomed.ID.Name)
		return a > 0 && b > 0
	})
	if !h.r.Synced() {
		t.Fatalf("Watch must report its informer as synced while it runs")
	}
	if err := h.dyn.Resource(PipelineRuns).Namespace(demoNS).Delete(context.Background(), doomed.ID.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually("the deleted run is reported", func() bool { _, deleted := w.seen(doomed.ID.Name); return deleted })
	cancel()
	if err := <-stopped; err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if h.r.Synced() {
		t.Fatalf("a stopped Watch is not synced")
	}
}

// TestHandOverOfARunThatLeftTheWatch hands over runs the informer reported as deleted. A run that is
// labelled done leaves the watch as a deletion carrying its state from before the label; only a run
// that is gone from the API is reported as deleted.
func TestHandOverOfARunThatLeftTheWatch(t *testing.T) {
	tests := []struct {
		name        string
		exists      bool
		getErr      error
		wantDeleted bool
		wantErr     bool
	}{
		{name: "labelled done, still there", exists: true},
		{name: "deleted", wantDeleted: true},
		{name: "cannot tell", getErr: errors.New("connection refused"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			run := h.create(demoSpec(pushTrigger(shaA)), 1)
			pr, err := h.dyn.Resource(PipelineRuns).Namespace(demoNS).Get(context.Background(), run.ID.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !tt.exists {
				if err := h.dyn.Resource(PipelineRuns).Namespace(demoNS).Delete(context.Background(), run.ID.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.getErr != nil {
				h.dyn.PrependReactor("get", "pipelineruns", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}
			key := demoNS + "/" + run.ID.Name
			h.r.rememberDeleted(pr)
			w := &watcher{reconciled: map[string]int{}}
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			_, err = h.r.handOver(context.Background(), key, indexer, w)
			if (err != nil) != tt.wantErr {
				t.Fatalf("handOver error = %v, want an error: %v", err, tt.wantErr)
			}
			if _, deleted := w.seen(run.ID.Name); deleted != tt.wantDeleted {
				t.Fatalf("reported deleted = %v, want %v", deleted, tt.wantDeleted)
			}
			if kept := h.r.takeDeleted(key) != nil; kept != tt.wantErr {
				t.Fatalf("kept for a retry = %v, want %v", kept, tt.wantErr)
			}
		})
	}
}

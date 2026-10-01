package tekton

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"octomaton.dev/internal/services/ci"
)

func TestServiceAccountBranches(t *testing.T) {
	asTemplate := demoDef + "  taskRunTemplate:\n    serviceAccountName: deployer\n"
	asTask := demoDef + "  taskRunSpecs:\n    - {pipelineTaskName: test, serviceAccountName: deployer}\n"
	onBranch := func(branch string) ci.Trigger {
		tr := pushTrigger(shaA)
		tr.Ref, tr.Branch = "refs/heads/"+branch, branch
		return tr
	}
	tagPush := pushTrigger(shaA)
	tagPush.Ref, tagPush.Branch, tagPush.Tag = "refs/tags/v1", "", "v1"
	annotated := func(value string) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: demoNS, Annotations: map[string]string{annotationBranches: value}}}
	}
	plain := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: demoNS}}
	tests := []struct {
		name     string
		def      string
		trigger  ci.Trigger
		sa       *corev1.ServiceAccount
		failGet  bool
		wantText string // empty: the run is created
	}{
		{name: "no ServiceAccount named", def: demoDef, trigger: onBranch("feature"), sa: annotated("main")},
		{name: "a ServiceAccount without the annotation", def: asTemplate, trigger: onBranch("feature"), sa: plain},
		{name: "a ServiceAccount that does not exist", def: asTemplate, trigger: onBranch("feature")},
		{name: "main on main", def: asTemplate, trigger: onBranch("main"), sa: annotated("main")},
		{name: "main on a pull request's branch", def: asTemplate, trigger: onBranch("feature"), sa: annotated("main"),
			wantText: "names ServiceAccount `deployer`, which only runs on branches matching \"main\" may use (its `octomaton.dev/branches` annotation), and this run is on branch \"feature\""},
		{name: "main in the merge queue", def: asTemplate, trigger: onBranch("gh-readonly-queue/main/pr-7-1111111"), sa: annotated("main"),
			wantText: "this run is on branch \"gh-readonly-queue/main/pr-7-1111111\""},
		{name: "a task's ServiceAccount", def: asTask, trigger: onBranch("feature"), sa: annotated("main"), wantText: "names ServiceAccount `deployer`"},
		{name: "a matching glob among several", def: asTemplate, trigger: onBranch("release/1.2"), sa: annotated("main, release/*")},
		{name: "a glob does not cross a slash", def: asTemplate, trigger: onBranch("release/1/2"), sa: annotated("main,release/*"),
			wantText: "branches matching \"main\", \"release/*\""},
		{name: "a tag push", def: asTemplate, trigger: tagPush, sa: annotated("*"), wantText: "this run has no branch"},
		{name: "an empty annotation", def: asTemplate, trigger: onBranch("main"), sa: annotated(" , "), wantText: "annotation lists no branch, so no run may use it"},
		{name: "an invalid glob", def: asTemplate, trigger: onBranch("main"), sa: annotated("main,["), wantText: "has an invalid `octomaton.dev/branches` annotation"},
		{name: "a ServiceAccount that cannot be read", def: asTemplate, trigger: onBranch("main"), failGet: true, wantText: "Could not read ServiceAccount `ci-demo/deployer`"},
		{name: "a ServiceAccount name that is not a string", def: demoDef + "  taskRunTemplate:\n    serviceAccountName: 7\n", trigger: onBranch("main"),
			wantText: "spec.taskRunTemplate.serviceAccountName must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			if tt.sa != nil {
				if _, err := h.kube.CoreV1().ServiceAccounts(demoNS).Create(context.Background(), tt.sa, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.failGet {
				h.kube.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("forbidden") })
			}
			spec := demoSpec(tt.trigger)
			spec.Definition = []byte(tt.def)
			_, err := h.r.Create(context.Background(), spec, 1)
			runs, lerr := h.dyn.Resource(PipelineRuns).Namespace(demoNS).List(context.Background(), metav1.ListOptions{})
			if lerr != nil {
				t.Fatal(lerr)
			}
			if tt.wantText == "" {
				if err != nil || len(runs.Items) != 1 {
					t.Fatalf("Create = %v with %d runs, want the run created", err, len(runs.Items))
				}
				return
			}
			var refusal *ci.Refusal
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, tt.wantText) {
				t.Fatalf("Create = %v, want a refusal mentioning %q", err, tt.wantText)
			}
			if len(runs.Items) != 0 {
				t.Fatalf("a refused run created %d PipelineRuns", len(runs.Items))
			}
		})
	}
}

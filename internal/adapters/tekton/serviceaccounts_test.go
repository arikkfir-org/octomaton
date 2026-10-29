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
	withTags := func(branches, tags string) *corev1.ServiceAccount {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: demoNS, Annotations: map[string]string{annotationTags: tags}}}
		if branches != "" {
			sa.Annotations[annotationBranches] = branches
		}
		return sa
	}
	onTag := func(tag string) ci.Trigger {
		tr := pushTrigger(shaA)
		tr.Ref, tr.Branch, tr.Tag = "refs/tags/"+tag, "", tag
		return tr
	}
	plain := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: demoNS}}
	withAnnotations := func(annotations map[string]string) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: demoNS, Annotations: annotations}}
	}
	mainOrPublish := withAnnotations(map[string]string{annotationBranches: "main", annotationOrganizationPipelines: "lint, publish"})
	of := func(tr ci.Trigger, pipeline string) ci.Trigger {
		tr.Pipeline = pipeline
		return tr
	}
	tests := []struct {
		name         string
		def          string
		trigger      ci.Trigger
		organization bool // the run is of an organization pipeline
		sa           *corev1.ServiceAccount
		failGet      bool
		wantText     string // empty: the run is created
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
		{name: "a tag push", def: asTemplate, trigger: tagPush, sa: annotated("*"), wantText: "this run is of tag \"v1\""},
		{name: "a tag push on a ServiceAccount restricted to branches", def: asTemplate, trigger: onTag("v1.2.0"), sa: annotated("main"),
			wantText: "names ServiceAccount `deployer`, which only runs on branches matching \"main\" may use (its `octomaton.dev/branches` annotation), and this run is of tag \"v1.2.0\""},
		{name: "a matching tag", def: asTemplate, trigger: onTag("v1.2.0"), sa: withTags("", "v*")},
		{name: "a matching tag beside branches", def: asTemplate, trigger: onTag("v1.2.0"), sa: withTags("main", "v*")},
		{name: "main beside tags", def: asTemplate, trigger: onBranch("main"), sa: withTags("main", "v*")},
		{name: "a tag that matches no glob", def: asTemplate, trigger: onTag("nightly"), sa: withTags("main", "v*"),
			wantText: "which only runs on branches matching \"main\" or of tags matching \"v*\" may use (its `octomaton.dev/branches` and `octomaton.dev/tags` annotations), and this run is of tag \"nightly\""},
		{name: "a branch on a ServiceAccount restricted to tags", def: asTemplate, trigger: onBranch("main"), sa: withTags("", "v*"),
			wantText: "which only runs of tags matching \"v*\" may use (its `octomaton.dev/tags` annotation), and this run is on branch \"main\""},
		{name: "a tag glob does not cross a slash", def: asTemplate, trigger: onTag("v1/2"), sa: withTags("", "v*"), wantText: "this run is of tag \"v1/2\""},
		{name: "an empty tags annotation", def: asTemplate, trigger: onTag("v1.0.0"), sa: withTags("", " "), wantText: "`octomaton.dev/tags` annotation lists no tag, so no run may use it"},
		{name: "empty branches and tags annotations", def: asTemplate, trigger: onTag("v1.0.0"), sa: withTags(" ", ""), wantText: "`octomaton.dev/branches` and `octomaton.dev/tags` annotations list no branch or tag, so no run may use it"},
		{name: "an invalid tag glob", def: asTemplate, trigger: onTag("v1.0.0"), sa: withTags("", "v["), wantText: "has an invalid `octomaton.dev/tags` annotation"},
		{name: "an empty annotation", def: asTemplate, trigger: onBranch("main"), sa: annotated(" , "), wantText: "annotation lists no branch, so no run may use it"},
		{name: "an invalid glob", def: asTemplate, trigger: onBranch("main"), sa: annotated("main,["), wantText: "has an invalid `octomaton.dev/branches` annotation"},
		{name: "a listed organization pipeline on a branch", def: asTemplate, trigger: of(onBranch("feat/x"), "publish"), organization: true, sa: mainOrPublish},
		{name: "a listed organization pipeline on a tag", def: asTemplate, trigger: of(onTag("v1.2.0"), "publish"), organization: true, sa: mainOrPublish},
		{name: "a listed organization pipeline on a ServiceAccount restricted to it alone", def: asTemplate, trigger: of(onBranch("feat/x"), "publish"), organization: true,
			sa: withAnnotations(map[string]string{annotationOrganizationPipelines: "publish"})},
		{name: "an organization pipeline not listed", def: asTemplate, trigger: of(onBranch("feat/x"), "docs"), organization: true, sa: mainOrPublish,
			wantText: "names ServiceAccount `deployer`, which only runs on branches matching \"main\" or of organization pipelines `lint`, `publish` may use (its `octomaton.dev/branches` and `octomaton.dev/organization-pipelines` annotations), and this run of organization pipeline `docs` is on branch \"feat/x\""},
		{name: "a repository pipeline with a listed name", def: asTemplate, trigger: of(onBranch("feat/x"), "publish"), sa: mainOrPublish,
			wantText: "and this run of repository pipeline `publish` is on branch \"feat/x\""},
		{name: "a repository pipeline with a listed name on main", def: asTemplate, trigger: of(onBranch("main"), "publish"), sa: mainOrPublish},
		{name: "main on a ServiceAccount restricted to organization pipelines", def: asTemplate, trigger: onBranch("main"),
			sa:       withAnnotations(map[string]string{annotationOrganizationPipelines: "publish"}),
			wantText: "which only runs of organization pipeline `publish` may use (its `octomaton.dev/organization-pipelines` annotation), and this run of repository pipeline `ci` is on branch \"main\""},
		{name: "an organization pipeline on all three annotations", def: asTemplate, trigger: of(onTag("nightly"), "docs"), organization: true,
			sa:       withAnnotations(map[string]string{annotationBranches: "main", annotationTags: "v*", annotationOrganizationPipelines: "publish"}),
			wantText: "which only runs on branches matching \"main\" or of tags matching \"v*\" or of organization pipeline `publish` may use (its `octomaton.dev/branches`, `octomaton.dev/tags` and `octomaton.dev/organization-pipelines` annotations), and this run of organization pipeline `docs` is of tag \"nightly\""},
		{name: "an empty organization pipelines annotation", def: asTemplate, trigger: of(onBranch("main"), "publish"), organization: true,
			sa:       withAnnotations(map[string]string{annotationOrganizationPipelines: " , "}),
			wantText: "whose `octomaton.dev/organization-pipelines` annotation lists no organization pipeline, so no run may use it"},
		{name: "empty annotations beside organization pipelines", def: asTemplate, trigger: of(onBranch("main"), "publish"), organization: true,
			sa:       withAnnotations(map[string]string{annotationBranches: "", annotationTags: "", annotationOrganizationPipelines: ""}),
			wantText: "whose `octomaton.dev/branches`, `octomaton.dev/tags` and `octomaton.dev/organization-pipelines` annotations list no branch, tag or organization pipeline, so no run may use it"},
		{name: "an invalid glob beside a listed organization pipeline", def: asTemplate, trigger: of(onBranch("feat/x"), "publish"), organization: true,
			sa:       withAnnotations(map[string]string{annotationBranches: "main,[", annotationOrganizationPipelines: "publish"}),
			wantText: "has an invalid `octomaton.dev/branches` annotation"},
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
			spec.Definition, spec.Organization = []byte(tt.def), tt.organization
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
			if (refusal.Cause != nil) != tt.failGet {
				t.Fatalf("cause = %v: only a ServiceAccount that cannot be read is a failed call", refusal.Cause)
			}
			if len(runs.Items) != 0 {
				t.Fatalf("a refused run created %d PipelineRuns", len(runs.Items))
			}
		})
	}
}

package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/config"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/githubapp/githubtest"
	"octomaton.dev/internal/metrics"
	"octomaton.dev/internal/tekton"
)

const (
	appID          = 42
	installationID = 7
	repoID         = 1001
	owner          = "octo-org"
	repoName       = "demo"
	fullName       = owner + "/" + repoName
	namespace      = "ci-demo"
	sha1           = "1111111111111111111111111111111111111111"
	sha2           = "2222222222222222222222222222222222222222"
	sha3           = "3333333333333333333333333333333333333333"
	baseSHA        = "9999999999999999999999999999999999999999"
	dashboard      = "https://tekton.example"
)

const serverConfig = `
github: {appIDFile: a, privateKeyFile: b, webhookSecretFile: c, allowedOwners: [octo-org]}
tekton: {dashboardURL: "https://tekton.example"}
namespaces: {template: "ci-{{ .Repository.Name }}"}
`

// ciConfig runs "ci" on pull requests, pushes to main and the merge queue.
const ciConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {branches: [main]}
      push: {branches: [main]}
      merge_group: {}
    params:
      repo-url: "{{ .Repository.CloneURL }}"
      revision: "{{ .Revision }}"
    githubToken: {workspace: github-token}
`

const ciRun = `
apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  generateName: ci-
spec:
  params:
    - {name: revision, value: ""}
  workspaces:
    - {name: source, emptyDir: {}}
  pipelineSpec:
    tasks:
      - {name: build, taskSpec: {steps: [{name: b, image: alpine, script: "true"}]}}
      - {name: test, taskSpec: {steps: [{name: t, image: alpine, script: "true"}]}}
`

type harness struct {
	t    *testing.T
	gh   *githubtest.Server
	app  *githubapp.App
	kube *kubefake.Clientset
	dyn  *dynamicfake.FakeDynamicClient
	runs *tekton.Client
	svc  *Service
	m    *metrics.Metrics

	mu    sync.Mutex
	now   time.Time
	clock time.Time
	uid   int
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, now: time.Date(2026, 5, 1, 10, 2, 0, 0, time.UTC), clock: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	h.gh = githubtest.NewServer(t, appID)
	app, err := githubapp.New(appID, githubtest.Key(), githubapp.WithBaseURL(h.gh.URL))
	if err != nil {
		t.Fatal(err)
	}
	h.app = app
	h.kube = kubefake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	h.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		tekton.PipelineRuns: "PipelineRunList",
		tekton.TaskRuns:     "TaskRunList",
	})
	// Give created objects what the API server would: a UID and a creation time.
	h.dyn.PrependReactor("create", "pipelineruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		h.mu.Lock()
		h.uid++
		h.clock = h.clock.Add(time.Second)
		obj.SetUID(types.UID(fmt.Sprintf("uid-%d", h.uid)))
		obj.SetCreationTimestamp(metav1.NewTime(h.clock))
		h.mu.Unlock()
		return false, nil, nil
	})
	h.runs = &tekton.Client{Dynamic: h.dyn, Kube: h.kube}
	cfg, err := config.Parse([]byte(serverConfig))
	if err != nil {
		t.Fatal(err)
	}
	h.m = metrics.New()
	h.svc = &Service{
		GitHub:       app,
		Runs:         h.runs,
		Namespaces:   &cfg.Namespaces,
		DashboardURL: dashboard,
		OwnerAllowed: cfg.GitHub.OwnerAllowed,
		Logger:       discard(),
		Metrics:      h.m,
		Now:          h.clockNow,
	}
	h.gh.SetPermission(fullName, "maintainer", "write")
	h.gh.SetPermission(fullName, "reader", "read")
	return h
}

func (h *harness) clockNow() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) setNow(t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = t
}

// files stores .octomaton.yaml and the ci PipelineRun at a ref.
func (h *harness) files(ref, cfg, run string) {
	h.gh.AddFile(fullName, ref, ".octomaton.yaml", cfg)
	if run != "" {
		h.gh.AddFile(fullName, ref, ".tekton/ci.yaml", run)
	}
}

func repo() checkrun.Repository {
	return checkrun.Repository{
		ID: repoID, Owner: owner, Name: repoName, FullName: fullName,
		CloneURL: "https://github.com/" + fullName + ".git", HTMLURL: "https://github.com/" + fullName, DefaultBranch: "main",
	}
}

func prContext(sha string, number int, association, headRepo string) checkrun.Context {
	return checkrun.Context{
		Version: checkrun.ContextVersion, Event: checkrun.EventPullRequest, Action: "synchronize", DeliveryID: "delivery-" + sha[:4],
		InstallationID: installationID, Repository: repo(), Revision: sha, Ref: fmt.Sprintf("refs/pull/%d/head", number), Branch: "feature",
		Sender: "alice",
		PullRequest: &checkrun.PullRequest{Number: number, HeadRef: "feature", HeadSHA: sha, BaseRef: "main", BaseSHA: baseSHA,
			HeadRepo: headRepo, Author: "alice", AuthorAssociation: association, HTMLURL: fmt.Sprintf("https://github.com/%s/pull/%d", fullName, number)},
	}
}

func trustedPR(sha string) checkrun.Context { return prContext(sha, 5, "MEMBER", fullName) }

func pushCtx(sha, branch string) checkrun.Context {
	return checkrun.Context{
		Version: checkrun.ContextVersion, Event: checkrun.EventPush, DeliveryID: "push-" + sha[:4], InstallationID: installationID,
		Repository: repo(), Revision: sha, Ref: "refs/heads/" + branch, Branch: branch, Sender: "alice",
		Push: &checkrun.Push{Before: baseSHA, After: sha},
	}
}

func (h *harness) allRuns() []unstructured.Unstructured {
	h.t.Helper()
	runs, err := h.runs.List(context.Background(), namespace, "")
	if err != nil {
		h.t.Fatal(err)
	}
	return runs
}

func (h *harness) run(name string) *unstructured.Unstructured {
	h.t.Helper()
	pr, err := h.runs.Get(context.Background(), namespace, name)
	if err != nil || pr == nil {
		h.t.Fatalf("PipelineRun %s: %v, %v", name, pr, err)
	}
	return pr
}

func (h *harness) checks(name string) []githubtest.CheckRun {
	var out []githubtest.CheckRun
	for _, cr := range h.gh.CheckRuns() {
		if cr.Name == name {
			out = append(out, cr)
		}
	}
	return out
}

func (h *harness) onlyCheck(name string) githubtest.CheckRun {
	h.t.Helper()
	crs := h.checks(name)
	if len(crs) != 1 {
		h.t.Fatalf("check runs named %q = %d, want 1 (all: %+v)", name, len(crs), h.gh.CheckRuns())
	}
	return crs[0]
}

// finish marks a run as finished by Tekton.
func (h *harness) finish(name, status, reason string) {
	h.t.Helper()
	pr := h.run(name)
	_ = unstructured.SetNestedField(pr.Object, map[string]any{
		"conditions":     []any{map[string]any{"type": "Succeeded", "status": status, "reason": reason}},
		"startTime":      "2026-05-01T09:00:00Z",
		"completionTime": "2026-05-01T09:10:00Z",
	}, "status")
	if _, err := h.dyn.Resource(tekton.PipelineRuns).Namespace(namespace).Update(context.Background(), pr, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func contextAnnotation(t *testing.T, pr *unstructured.Unstructured) checkrun.Context {
	t.Helper()
	var c checkrun.Context
	if err := json.Unmarshal([]byte(pr.GetAnnotations()[tekton.AnnotationContext]), &c); err != nil {
		t.Fatalf("context annotation: %v", err)
	}
	return c
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("%q does not contain %q", got, w)
		}
	}
}

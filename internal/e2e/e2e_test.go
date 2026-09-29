// Package e2e drives Octomaton end to end: signed webhooks go through the
// HTTP handler, worker pool and trigger service into a fake cluster and a fake
// GitHub, and the reporter reports the runs back.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	webhook "octomaton.dev/internal/adapters/http"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/githubapp/githubtest"
	"octomaton.dev/internal/reporter"
	"octomaton.dev/internal/system/config"
	"octomaton.dev/internal/system/metrics/metricstest"
	"octomaton.dev/internal/tekton"
	"octomaton.dev/internal/trigger"
)

const (
	appID    = 4242
	secret   = "webhook-secret"
	owner    = "arikkfir-org"
	repoName = "octomaton"
	fullName = owner + "/" + repoName
	ns       = "ci-octomaton"
	headSHA  = "abcdef0123456789abcdef0123456789abcdef01"
	baseSHA  = "0123456789abcdef0123456789abcdef01234567"
)

const octomatonYAML = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {branches: [main]}
      merge_group:
    params:
      repo-url: "{{ .Repository.CloneURL }}"
      revision: "{{ .Revision }}"
    githubToken: {workspace: github-token}
  - name: preview
    pipelineRun: .tekton/ci.yaml
    on:
      comment: {pattern: "^/preview\\b"}
    params:
      revision: "{{ .Revision }}"
      target: "{{ .Comment.Arguments }}"
`

const ciYAML = `
apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  generateName: ci-
spec:
  taskRunTemplate:
    serviceAccountName: pipeline
  workspaces:
    - name: source
      emptyDir: {}
  pipelineSpec:
    params:
      - {name: repo-url, type: string}
      - {name: revision, type: string}
    workspaces:
      - name: source
      - name: github-token
        optional: true
    tasks:
      - name: ci
        taskSpec:
          steps:
            - name: test
              image: golang:1.27.1
              script: go test ./...
`

type env struct {
	t        *testing.T
	gh       *githubtest.Server
	dyn      *dynamicfake.FakeDynamicClient
	runs     *tekton.Client
	handler  http.Handler
	reporter *reporter.Reporter
}

func setup(t *testing.T) *env {
	t.Helper()
	gh := githubtest.NewServer(t, appID)
	gh.AddFile(fullName, headSHA, ".octomaton.yaml", octomatonYAML)
	gh.AddFile(fullName, headSHA, ".tekton/ci.yaml", ciYAML)
	gh.AddFile(fullName, "main", ".octomaton.yaml", octomatonYAML)
	gh.AddFile(fullName, "main", ".tekton/ci.yaml", ciYAML)
	gh.SetPullRequest(fullName, githubtest.PullRequest{Number: 12, State: "open", HeadSHA: headSHA, HeadRef: "feature", BaseRef: "main", BaseSHA: baseSHA, HeadRepo: fullName, Author: "arikkfir", AuthorAssociation: "OWNER"})
	gh.SetPermission(fullName, "arikkfir", "admin")

	app, err := githubapp.New(appID, githubtest.Key(), githubapp.WithBaseURL(gh.URL))
	if err != nil {
		t.Fatal(err)
	}
	kube := kubefake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		tekton.PipelineRuns: "PipelineRunList",
		tekton.TaskRuns:     "TaskRunList",
	})
	runs := &tekton.Client{Dynamic: dyn, Kube: kube}
	namespaces, err := config.NewNamespaces("ci-{{ .Repository.Name }}", map[string]string{"arikkfir-org/.github": "ci-github"})
	if err != nil {
		t.Fatal(err)
	}
	owners := &config.GitHub{AllowedOwners: []string{"arikkfir-org"}}
	const dashboard = "https://tekton.dev.kfirs.com"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metricstest.New(t).Metrics
	svc := &trigger.Service{
		GitHub: app, Runs: runs, Namespaces: namespaces, DashboardURL: dashboard,
		OwnerAllowed: owners.OwnerAllowed, Logger: logger, Metrics: m,
	}
	pool := webhook.NewPool(2, 16, time.Minute, logger, m)
	t.Cleanup(func() { _ = pool.Shutdown(context.Background()) })
	handler := &webhook.Handler{Secret: []byte(secret), Router: svc, Pool: pool, Dedupe: webhook.NewDedupe(100, time.Hour), Metrics: m, Logger: logger}
	rep := &reporter.Reporter{Dynamic: dyn, Runs: runs, GitHub: app, DashboardURL: dashboard, Logger: logger, Metrics: m, Resume: svc.Resume, ReleaseNext: svc.ReleaseNext}
	return &env{t: t, gh: gh, dyn: dyn, runs: runs, handler: handler, reporter: rep}
}

func (e *env) deliver(event, delivery string, payload any, sign bool) *httptest.ResponseRecorder {
	e.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/github/hooks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sign {
		req.Header.Set(webhook.SignatureHeader, webhook.Sign([]byte(secret), body))
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) waitForRun(name string) *unstructured.Unstructured {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		pr, err := e.runs.Get(context.Background(), ns, name)
		if err == nil && pr != nil && pr.GetAnnotations()[tekton.AnnotationCheckRunID] != "" && !tekton.IsPending(pr) {
			return pr
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("PipelineRun %s was not created and released (last: %v, %v)", name, pr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var repository = map[string]any{
	"id": 99, "name": repoName, "full_name": fullName, "private": false, "default_branch": "main",
	"owner":     map[string]any{"login": owner},
	"clone_url": "https://github.com/" + fullName + ".git", "html_url": "https://github.com/" + fullName,
}

func pullRequestPayload(action string) map[string]any {
	return map[string]any{
		"action": action, "number": 12,
		"pull_request": map[string]any{
			"number": 12, "draft": false, "author_association": "OWNER", "html_url": "https://github.com/" + fullName + "/pull/12",
			"user": map[string]any{"login": "arikkfir"},
			"head": map[string]any{"ref": "feature", "sha": headSHA, "repo": map[string]any{"full_name": fullName}},
			"base": map[string]any{"ref": "main", "sha": baseSHA},
		},
		"repository":   repository,
		"installation": map[string]any{"id": 5},
		"sender":       map[string]any{"login": "arikkfir"},
	}
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	if rec := e.deliver("ping", "p-1", map[string]any{"zen": "Design for failure."}, true); rec.Code != http.StatusOK {
		t.Fatalf("ping: %d", rec.Code)
	}
	if rec := e.deliver("pull_request", "forged", pullRequestPayload("opened"), false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned delivery: %d, want 401", rec.Code)
	}

	// A pull request opens: the ci pipeline runs.
	if rec := e.deliver("pull_request", "d-1", pullRequestPayload("opened"), true); rec.Code != http.StatusAccepted {
		t.Fatalf("pull_request: %d %s", rec.Code, rec.Body.String())
	}
	name := "octomaton-ci-abcdef0-1"
	pr := e.waitForRun(name)
	if rec := e.deliver("pull_request", "d-1", pullRequestPayload("opened"), true); !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("redelivery must be deduplicated: %s", rec.Body.String())
	}
	params, _, _ := unstructured.NestedSlice(pr.Object, "spec", "params")
	if len(params) != 2 || params[1].(map[string]any)["value"] != headSHA {
		t.Fatalf("params = %v", params)
	}
	if sa, _, _ := unstructured.NestedString(pr.Object, "spec", "taskRunTemplate", "serviceAccountName"); sa != "pipeline" {
		t.Fatalf("the PipelineRun file is used as is: serviceAccountName %q", sa)
	}
	checkID, _ := strconv.ParseInt(pr.GetAnnotations()[tekton.AnnotationCheckRunID], 10, 64)
	check, _ := e.gh.CheckRun(checkID)
	if check.Name != "ci" || check.Status != "queued" || check.DetailsURL != "https://tekton.dev.kfirs.com/#/namespaces/ci-octomaton/pipelineruns/"+name {
		t.Fatalf("check run = %+v", check)
	}

	// Tekton runs it; the reporter reports it.
	now := time.Now().UTC().Truncate(time.Second)
	pr.Object["status"] = map[string]any{
		"startTime": now.Add(-2 * time.Minute).Format(time.RFC3339), "completionTime": now.Format(time.RFC3339),
		"conditions": []any{map[string]any{"type": "Succeeded", "status": "True", "reason": "Succeeded"}},
	}
	if _, err := e.dyn.Resource(tekton.PipelineRuns).Namespace(ns).Update(ctx, pr, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pr, _ = e.runs.Get(ctx, ns, name)
	if _, err := e.reporter.Reconcile(ctx, pr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	check, _ = e.gh.CheckRun(checkID)
	if check.Status != "completed" || check.Conclusion != "success" || check.Title != "Succeeded in 2m0s" {
		t.Fatalf("reported check = %+v", check)
	}
	if c, found, _ := checkrun.DecodeMarker(check.Text); !found || c.PullRequest == nil || c.PullRequest.Number != 12 {
		t.Fatalf("the completed check keeps its trigger context: %+v", c)
	}

	// Re-run from GitHub, after the run is gone.
	if err := e.dyn.Resource(tekton.PipelineRuns).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	rerun := map[string]any{
		"action":       "rerequested",
		"check_run":    map[string]any{"id": checkID, "name": "ci", "head_sha": headSHA, "app": map[string]any{"id": appID}},
		"repository":   repository,
		"installation": map[string]any{"id": 5},
		"sender":       map[string]any{"login": "arikkfir"},
	}
	if rec := e.deliver("check_run", "d-2", rerun, true); rec.Code != http.StatusAccepted {
		t.Fatalf("check_run: %d %s", rec.Code, rec.Body.String())
	}
	// Attempts follow the highest one still there; the first run was pruned.
	second := e.waitForRun("octomaton-ci-abcdef0-1")
	if c := second.GetAnnotations()[tekton.AnnotationContext]; !strings.Contains(c, `"rerunBy":"arikkfir"`) {
		t.Fatalf("re-run context = %s", c)
	}
	if id := second.GetAnnotations()[tekton.AnnotationCheckRunID]; id == strconv.FormatInt(checkID, 10) {
		t.Fatalf("a re-run gets a new check run")
	}

	// A comment command on the pull request.
	comment := map[string]any{
		"action":       "created",
		"issue":        map[string]any{"number": 12, "pull_request": map[string]any{"url": "https://api.github.com/repos/" + fullName + "/pulls/12"}},
		"comment":      map[string]any{"id": 555, "body": "/preview eu-west\nthanks!", "user": map[string]any{"login": "arikkfir"}},
		"repository":   repository,
		"installation": map[string]any{"id": 5},
		"sender":       map[string]any{"login": "arikkfir"},
	}
	if rec := e.deliver("issue_comment", "d-3", comment, true); rec.Code != http.StatusAccepted {
		t.Fatalf("issue_comment: %d %s", rec.Code, rec.Body.String())
	}
	preview := e.waitForRun("octomaton-preview-abcdef0-1")
	params, _, _ = unstructured.NestedSlice(preview.Object, "spec", "params")
	if fmt.Sprint(params) != fmt.Sprint([]any{map[string]any{"name": "revision", "value": headSHA}, map[string]any{"name": "target", "value": "eu-west"}}) {
		t.Fatalf("comment run params = %v", params)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.gh.Reactions()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if r := e.gh.Reactions(); len(r) != 1 || r[0].Content != "eyes" || r[0].CommentID != 555 {
		t.Fatalf("reactions = %+v", r)
	}

	// Events from other owners are ignored.
	foreign := pullRequestPayload("opened")
	foreign["repository"] = map[string]any{"id": 1, "name": "x", "full_name": "someone/x", "owner": map[string]any{"login": "someone"}}
	if rec := e.deliver("pull_request", "d-4", foreign, true); !strings.Contains(rec.Body.String(), "owner is not allowed") {
		t.Fatalf("foreign owner: %s", rec.Body.String())
	}
}

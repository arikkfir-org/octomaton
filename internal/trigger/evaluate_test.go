package trigger

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"octomaton.dev/internal/adapters/github/githubtest"
	"octomaton.dev/internal/adapters/tekton"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/system/metrics"
)

func TestEvaluatePullRequestCreatesAndReleasesARun(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	h.gh.SetPullRequest(fullName, githubtest.PullRequest{Number: 5, State: "open", HeadSHA: sha1, HeadRef: "feature", BaseRef: "main"})
	c := trustedPR(sha1)

	h.svc.Evaluate(context.Background(), c, EvalOptions{ReportConfigErrors: true})

	runs := h.allRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	pr := &runs[0]
	name := "demo-ci-1111111-1"
	if pr.GetName() != name || pr.GetGenerateName() != "" {
		t.Fatalf("run name = %q (generateName %q), want %q", pr.GetName(), pr.GetGenerateName(), name)
	}
	if tekton.IsPending(pr) {
		t.Fatalf("the run must be released once its check and token exist")
	}
	params, _, _ := unstructured.NestedSlice(pr.Object, "spec", "params")
	want := []any{
		map[string]any{"name": "revision", "value": sha1},
		map[string]any{"name": "repo-url", "value": "https://github.com/octo-org/demo.git"},
	}
	if !slices.EqualFunc(params, want, func(a, b any) bool {
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		return string(ja) == string(jb)
	}) {
		t.Fatalf("params = %v, want %v", params, want)
	}
	labels := pr.GetLabels()
	for k, v := range map[string]string{
		tekton.LabelManagedBy: "octomaton", tekton.LabelPipeline: "ci", tekton.LabelEvent: "pull_request",
		tekton.LabelRepositoryID: "1001", tekton.LabelSHA: sha1, tekton.LabelConcurrencyGroup: tekton.GroupLabel(fullName, "ci/pr-5"),
	} {
		if labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, labels[k], v)
		}
	}
	check := h.onlyCheck("ci")
	ann := pr.GetAnnotations()
	for k, v := range map[string]string{
		tekton.AnnotationRepository: fullName, tekton.AnnotationSHA: sha1, tekton.AnnotationInstallationID: "7",
		tekton.AnnotationCheckRunID: strconv.FormatInt(check.ID, 10), tekton.AnnotationDeliveryID: c.DeliveryID,
		tekton.AnnotationConcurrencyGroup: "pr-5", tekton.AnnotationConcurrencyPolicy: "supersede",
		tekton.AnnotationHead: "feature", tekton.AnnotationAttempt: "1", tekton.AnnotationReported: tekton.ReportedQueued,
	} {
		if ann[k] != v {
			t.Errorf("annotation %s = %q, want %q", k, ann[k], v)
		}
	}
	if got := contextAnnotation(t, pr); got.Pipeline != "ci" || got.Revision != sha1 {
		t.Errorf("context annotation = %+v", got)
	}
	mustContain(t, ann[tekton.AnnotationToken], `"workspace":"github-token"`, `"contents":"read"`)

	if check.Status != "queued" || check.HeadSHA != sha1 || check.ExternalID != namespace+"/"+name ||
		check.DetailsURL != dashboard+"/#/namespaces/ci-demo/pipelineruns/"+name {
		t.Fatalf("check run = %+v", check)
	}
	if got, found, err := checkrun.DecodeMarker(check.Text); !found || err != nil || got.Pipeline != "ci" || got.Revision != sha1 {
		t.Fatalf("check run marker = %+v, %v, %v", got, found, err)
	}

	ws, _, _ := unstructured.NestedSlice(pr.Object, "spec", "workspaces")
	if len(ws) != 2 || ws[1].(map[string]any)["secret"].(map[string]any)["secretName"] != name+"-github-token" {
		t.Fatalf("workspaces = %v", ws)
	}
	secret, err := h.kube.CoreV1().Secrets(namespace).Get(context.Background(), name+"-github-token", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("token Secret: %v", err)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != name || secret.OwnerReferences[0].UID != pr.GetUID() {
		t.Fatalf("token Secret owner = %+v", secret.OwnerReferences)
	}
	var scoped *githubtest.TokenRequest
	for _, req := range h.gh.TokenRequests() {
		if len(req.RepositoryIDs) > 0 {
			scoped = &req
		}
	}
	if scoped == nil || !slices.Equal(scoped.RepositoryIDs, []int64{repoID}) || len(scoped.Permissions) != 1 || scoped.Permissions["contents"] != "read" {
		t.Fatalf("repository token request = %+v", scoped)
	}
	if string(secret.Data["token"]) == "" {
		t.Fatalf("token Secret is empty")
	}
	if got := h.m.Count(t, "octomaton.runs.created", attribute.String("result", metrics.RunCreated)); got != 1 {
		t.Fatalf("runs created metric = %v", got)
	}
}

func TestEvaluateIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	c := trustedPR(sha1)
	h.svc.Evaluate(context.Background(), c, EvalOptions{})
	h.svc.Evaluate(context.Background(), c, EvalOptions{}) // redelivery
	push := pushCtx(sha1, "feature")                       // another event for the same commit and branch
	push.Branch = "feature"
	h.svc.Evaluate(context.Background(), push, EvalOptions{})
	if runs := h.allRuns(); len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if n := len(h.checks("ci")); n != 1 {
		t.Fatalf("check runs = %d, want 1", n)
	}
	if got := h.m.Count(t, "octomaton.runs.created", attribute.String("result", metrics.RunExisting)); got != 1 {
		t.Fatalf("existing metric = %v, want 1 (the push to feature does not match ci)", got)
	}
}

func TestEvaluateWithoutConfigDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.svc.Evaluate(context.Background(), trustedPR(sha1), EvalOptions{ReportConfigErrors: true})
	if len(h.gh.CheckRuns()) != 0 || len(h.allRuns()) != 0 {
		t.Fatalf("a repository without .octomaton.yaml gets nothing")
	}
}

func TestEvaluateInvalidConfig(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, bogus: 1}\n", "")
	h.svc.Evaluate(context.Background(), trustedPR(sha1), EvalOptions{ReportConfigErrors: false})
	if len(h.gh.CheckRuns()) != 0 {
		t.Fatalf("configuration errors must not be reported when not asked to")
	}
	h.svc.Evaluate(context.Background(), trustedPR(sha1), EvalOptions{ReportConfigErrors: true})
	check := h.onlyCheck(checkrun.ConfigCheckName)
	if check.Conclusion != "failure" || check.Title != "Invalid .octomaton.yaml" {
		t.Fatalf("config check = %+v", check)
	}
	mustContain(t, check.Summary, "field bogus not found in pipeline")
	c, found, err := checkrun.DecodeMarker(check.Text)
	if !found || err != nil || c.Pipeline != "" || c.Revision != sha1 {
		t.Fatalf("config check marker = %+v, %v, %v", c, found, err)
	}
}

func TestEvaluateUnreadableConfig(t *testing.T) {
	h := newHarness(t)
	h.gh.SetFailFiles(true)
	h.svc.Evaluate(context.Background(), trustedPR(sha1), EvalOptions{ReportConfigErrors: true})
	check := h.onlyCheck(checkrun.ConfigCheckName)
	if check.Conclusion != "failure" || check.Title != "Could not read .octomaton.yaml" {
		t.Fatalf("config check = %+v", check)
	}
}

const pathsConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {paths: ["src/**"]}
`

func TestEvaluatePathFilters(t *testing.T) {
	tests := []struct {
		name     string
		files    []string // nil: the files API fails
		wantRun  bool
		wantSkip bool
	}{
		{name: "no relevant change is reported as skipped", files: []string{"README.md", "docs/a.md"}, wantSkip: true},
		{name: "a relevant change runs", files: []string{"README.md", "src/main.go"}, wantRun: true},
		{name: "unknown changes fail open", files: nil, wantRun: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, pathsConfig, ciRun)
			if tt.files != nil {
				h.gh.SetPullRequestFiles(fullName, 5, tt.files)
			}
			h.svc.Evaluate(context.Background(), trustedPR(sha1), EvalOptions{})
			check := h.onlyCheck("ci")
			if (len(h.allRuns()) == 1) != tt.wantRun {
				t.Fatalf("runs = %d, want run %v", len(h.allRuns()), tt.wantRun)
			}
			if tt.wantSkip {
				if check.Status != "completed" || check.Conclusion != "skipped" {
					t.Fatalf("check = %+v, want completed/skipped", check)
				}
				mustContain(t, check.Summary, "`paths`: `src/**`")
				if c, found, _ := checkrun.DecodeMarker(check.Text); !found || c.Pipeline != "ci" {
					t.Fatalf("a skipped check must carry its context for re-runs")
				}
			}
		})
	}
}

func TestEvaluateUntrustedPullRequestNeedsApproval(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	h.svc.Evaluate(context.Background(), prContext(sha1, 7, "CONTRIBUTOR", "stranger/demo"), EvalOptions{})
	if len(h.allRuns()) != 0 {
		t.Fatalf("an untrusted pull request must not run")
	}
	check := h.onlyCheck("ci")
	if check.Conclusion != "action_required" || check.Title != "Approval required" || check.DetailsURL != "https://github.com/octo-org/demo/pull/7" {
		t.Fatalf("check = %+v", check)
	}
	if len(check.Actions) != 1 || check.Actions[0]["identifier"] != ApproveAction || check.Actions[0]["label"] != "Approve and run" {
		t.Fatalf("actions = %+v", check.Actions)
	}
	mustContain(t, check.Summary, "@alice", "`contributor`", "`stranger/demo`")

	// A branch of the repository itself is trusted whatever the association.
	h.svc.Evaluate(context.Background(), prContext(sha2, 8, "NONE", fullName), EvalOptions{})
	if len(h.allRuns()) != 0 {
		t.Fatalf("sha2 has no configuration, nothing should run")
	}
	h.files(sha2, ciConfig, ciRun)
	h.svc.Evaluate(context.Background(), prContext(sha2, 8, "NONE", fullName), EvalOptions{})
	if len(h.allRuns()) != 1 {
		t.Fatalf("a same-repository pull request must run")
	}
}

func TestTrusted(t *testing.T) {
	tests := []struct {
		association, headRepo string
		want                  bool
	}{
		{"OWNER", "fork/demo", true},
		{"MEMBER", "fork/demo", true},
		{"COLLABORATOR", "fork/demo", true},
		{"member", "fork/demo", true},
		{"CONTRIBUTOR", "fork/demo", false},
		{"FIRST_TIME_CONTRIBUTOR", "fork/demo", false},
		{"NONE", "", false},
		{"NONE", fullName, true},
		{"NONE", "OCTO-ORG/Demo", true},
	}
	for _, tt := range tests {
		pr := &checkrun.PullRequest{AuthorAssociation: tt.association, HeadRepo: tt.headRepo}
		if got := Trusted(pr, fullName); got != tt.want {
			t.Errorf("Trusted(%s, %s) = %v, want %v", tt.association, tt.headRepo, got, tt.want)
		}
	}
	if !Trusted(nil, fullName) {
		t.Fatalf("events other than pull requests are trusted")
	}
}

func TestEvaluateSetupFailures(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		run     string
		prepare func(h *harness)
		want    string
	}{
		{
			name: "namespace missing",
			cfg:  ciConfig,
			run:  ciRun,
			prepare: func(h *harness) {
				_ = h.kube.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
			},
			want: "repository not onboarded: namespace ci-demo not found",
		},
		{name: "pipelineRun file missing", cfg: ciConfig, want: "`.tekton/ci.yaml` does not exist"},
		{name: "two documents", cfg: ciConfig, run: ciRun + "---\n" + ciRun, want: "exactly one YAML document, found 2"},
		{name: "wrong namespace", cfg: ciConfig, run: strings.Replace(ciRun, "generateName: ci-", "namespace: elsewhere", 1), want: "sets namespace `elsewhere`"},
		{name: "param renders a nil object", cfg: strings.Replace(ciConfig, `revision: "{{ .Revision }}"`, `revision: "{{ .Push.After }}"`, 1), run: ciRun, want: ".Push is only set for push events"},
		{name: "foreign Secret", cfg: ciConfig, run: strings.Replace(ciRun, "{name: source, emptyDir: {}}", "{name: source, secret: {secretName: registry}}", 1), want: `references Secret "registry"`},
		{name: "taskChecks without an inline pipeline", cfg: strings.Replace(ciConfig, "githubToken: {workspace: github-token}", "taskChecks: true", 1),
			run: "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: {pipelineRef: {name: p}}\n", want: "needs the PipelineRun's own `spec.pipelineSpec`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, tt.cfg, tt.run)
			if tt.prepare != nil {
				tt.prepare(h)
			}
			h.svc.Evaluate(context.Background(), trustedPR(sha1), EvalOptions{})
			if len(h.allRuns()) != 0 {
				t.Fatalf("no run may be created")
			}
			check := h.onlyCheck("ci")
			if check.Status != "completed" || check.Conclusion != "failure" {
				t.Fatalf("check = %+v", check)
			}
			mustContain(t, check.Summary, tt.want)
			if _, found, _ := checkrun.DecodeMarker(check.Text); !found {
				t.Fatalf("a failed check must carry its context for re-runs")
			}
		})
	}
}

func TestStartAbortsWhenTheTokenCannotBeMinted(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	h.gh.SetFailTokens(func(req githubtest.TokenRequest) bool { return len(req.RepositoryIDs) > 0 })
	h.svc.Evaluate(context.Background(), trustedPR(sha1), EvalOptions{})
	pr := h.run("demo-ci-1111111-1")
	if !tekton.CancelRequested(pr) || pr.GetLabels()[tekton.LabelDone] != "true" || pr.GetAnnotations()[tekton.AnnotationReported] != tekton.ReportedCompleted {
		t.Fatalf("the run must be cancelled and marked reported: %v %v", pr.Object["spec"], pr.GetAnnotations())
	}
	check := h.onlyCheck("ci")
	if check.Conclusion != "failure" || check.Title != "The run could not be started" {
		t.Fatalf("check = %+v", check)
	}
	mustContain(t, check.Summary, "minting the GitHub token")
	if got := h.m.Count(t, "octomaton.runs.created", attribute.String("result", metrics.RunError)); got != 1 {
		t.Fatalf("error metric = %v", got)
	}
}

func TestTaskChecksAreOpened(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, strings.Replace(ciConfig, "githubToken: {workspace: github-token}", "taskChecks: true", 1), ciRun)
	h.svc.Evaluate(context.Background(), trustedPR(sha1), EvalOptions{})
	build, test := h.onlyCheck("ci / build"), h.onlyCheck("ci / test")
	pr := h.run("demo-ci-1111111-1")
	ids := TaskCheckIDs(pr)
	if ids["build"] != build.ID || ids["test"] != test.ID {
		t.Fatalf("task check IDs = %v", ids)
	}
	if build.ExternalID != namespace+"/demo-ci-1111111-1" || !strings.Contains(build.DetailsURL, "?pipelineTask=build") {
		t.Fatalf("task check = %+v", build)
	}
	if pr.GetAnnotations()[tekton.AnnotationTaskChecks] != "true" {
		t.Fatalf("the run must record that it has task checks")
	}
}

func TestMissingAssociationIsReadFromThePullRequest(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	h.gh.SetPullRequest(fullName, githubtest.PullRequest{Number: 11, State: "open", HeadSHA: sha1, HeadRepo: "member/demo", AuthorAssociation: "MEMBER"})
	h.svc.Evaluate(context.Background(), prContext(sha1, 11, "", "member/demo"), EvalOptions{})
	if len(h.allRuns()) != 1 {
		t.Fatalf("a member's fork pull request must run when the payload lacks the association")
	}
}

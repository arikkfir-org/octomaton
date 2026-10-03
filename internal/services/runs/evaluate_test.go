package runs

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/system/metrics"
)

func TestEvaluatePullRequestCreatesAndReleasesARun(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	tr := branchPR(sha1)
	h.evaluate(tr, EvalOptions{ReportConfigErrors: true})

	runs := h.runner.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.ID.Name != "demo-ci-1111111-1" || run.Phase != ci.Released || run.Attempt != 1 {
		t.Fatalf("run = %+v, want demo-ci-1111111-1 released", run)
	}
	spec := h.runner.Spec(run.ID)
	wantSpec := ci.RunSpec{
		Trigger: func() ci.Trigger { w := tr; w.Pipeline = "ci"; return w }(), Definition: []byte(ciRun), Path: ".tekton/ci.yaml",
		Params: map[string]string{"repo-url": "https://github.com/octo-org/demo.git", "revision": sha1},
		Token:  &ci.TokenSettings{Workspace: "github-token", Permissions: map[string]string{"contents": "read"}},
		// Pull requests share their pipeline's group "pr-<number>" by default, and supersede.
		Concurrency: ci.Concurrency{Group: "pr-5", Key: "ci/pr-5", Policy: ci.Supersede},
	}
	if !reflect.DeepEqual(spec, wantSpec) {
		t.Fatalf("spec = %+v\nwant %+v", spec, wantSpec)
	}
	report := h.onlyReport("ci")
	if report.Status != ci.StatusQueued || report.Revision != sha1 || report.ExternalID != "ci-demo/demo-ci-1111111-1" ||
		report.URL != "https://runs.example/ci-demo/demo-ci-1111111-1" || report.Trigger == nil || report.Trigger.Pipeline != "ci" {
		t.Fatalf("report = %+v", report)
	}
	mustContain(t, report.Summary, "**PipelineRun:** [`ci-demo/demo-ci-1111111-1`](https://runs.example/ci-demo/demo-ci-1111111-1)", "**Trigger:** Pull request #5")
	if run = h.run(run.ID.Name); run.ReportID != report.ID || run.Reported != ci.ReportedQueued {
		t.Fatalf("the run must record its report: %+v", run)
	}
	tokens := h.host.TokenRequests()
	if len(tokens) != 1 || tokens[0].RepositoryID != repo.ID || !reflect.DeepEqual(tokens[0].Permissions, map[string]string{"contents": "read"}) {
		t.Fatalf("token requests = %+v", tokens)
	}
	if tok, ok := h.runner.Token(run.ID); !ok || tok.Value != "token-1" {
		t.Fatalf("the run's token = %+v, %v", tok, ok)
	}
	if got := h.m.Count(t, "octomaton.runs.created", attribute.String("result", metrics.RunCreated)); got != 1 {
		t.Fatalf("runs created = %v", got)
	}
}

func TestEvaluateIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	h.evaluate(branchPR(sha1), EvalOptions{})
	h.evaluate(branchPR(sha1), EvalOptions{})               // a redelivery
	h.evaluate(pushTrigger(sha1, "feature"), EvalOptions{}) // another event for the commit: ci does not run on pushes to feature
	if runs := h.runner.Runs(); len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if n := len(h.host.ReportsNamed("ci")); n != 1 {
		t.Fatalf("reports = %d, want 1", n)
	}
	if got := h.m.Count(t, "octomaton.runs.created", attribute.String("result", metrics.RunExisting)); got != 1 {
		t.Fatalf("existing runs = %v, want 1", got)
	}
}

func TestEvaluateConfigProblems(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(h *harness)
		report    bool
		wantTitle string
		wantText  string
	}{
		{name: "no configuration", setup: func(h *harness) {}, report: true},
		{name: "an invalid one not reported", setup: func(h *harness) { h.files(sha1, "apiVersion: octomaton.dev/v1\npipelines:\n  - {bogus: 1}\n", "") }},
		{name: "an invalid one", report: true, wantTitle: "Invalid .octomaton.yaml", wantText: "field bogus not found in pipeline",
			setup: func(h *harness) { h.files(sha1, "apiVersion: octomaton.dev/v1\npipelines:\n  - {bogus: 1}\n", "") }},
		{name: "an unreadable one", report: true, wantTitle: "Could not read .octomaton.yaml", wantText: "Re-run this check to try again.",
			setup: func(h *harness) { h.host.Fail("ReadFile", errors.New("GitHub is down")) }},
		// An invalid configuration is the repository's to report or not; one GitHub would not serve is Octomaton's.
		{name: "an unreadable one, where invalid ones go unreported", wantTitle: "Could not read .octomaton.yaml", wantText: "Re-run this check to try again.",
			setup: func(h *harness) { h.host.Fail("ReadFile", errors.New("GitHub is down")) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: tt.report})
			if len(h.runner.Runs()) != 0 {
				t.Fatalf("no run may start")
			}
			reports := h.host.Reports()
			if tt.wantTitle == "" {
				if len(reports) != 0 {
					t.Fatalf("reports = %+v, want none", reports)
				}
				return
			}
			r := h.onlyReport(ci.ConfigReportName)
			if r.Conclusion != ci.Failure || r.Title != tt.wantTitle || r.Trigger == nil || r.Trigger.Pipeline != "" || r.Trigger.Revision != sha1 {
				t.Fatalf("report = %+v", r)
			}
			mustContain(t, r.Summary, tt.wantText)
		})
	}
}

const pathsConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {paths: ["src/**"]}
      push: {paths: ["src/**"]}
`

func TestEvaluatePathFilters(t *testing.T) {
	tests := []struct {
		name     string
		trigger  ci.Trigger
		files    *ci.ChangedFiles
		wantRun  bool
		wantSkip bool
	}{
		{name: "no relevant change is reported as skipped", trigger: branchPR(sha1), files: &ci.ChangedFiles{Files: []string{"README.md", "docs/a.md"}, Complete: true}, wantSkip: true},
		{name: "a relevant change runs", trigger: branchPR(sha1), files: &ci.ChangedFiles{Files: []string{"README.md", "src/main.go"}, Complete: true}, wantRun: true},
		{name: "an incomplete list fails open", trigger: branchPR(sha1), files: &ci.ChangedFiles{Files: []string{"README.md"}}, wantRun: true},
		{name: "unknown changes fail open", trigger: branchPR(sha1), wantRun: true},
		{name: "a new branch changes everything", trigger: func() ci.Trigger { p := pushTrigger(sha1, "main"); p.Push.Created = true; return p }(), wantRun: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, pathsConfig, ciRun)
			if tt.files != nil {
				h.host.SetPullRequestFiles(repo, 5, *tt.files)
			}
			h.evaluate(tt.trigger, EvalOptions{})
			if (len(h.runner.Runs()) == 1) != tt.wantRun {
				t.Fatalf("runs = %d, want a run: %v", len(h.runner.Runs()), tt.wantRun)
			}
			if tt.wantSkip {
				r := h.onlyReport("ci")
				if r.Conclusion != ci.Skipped || r.Trigger == nil || r.Trigger.Pipeline != "ci" {
					t.Fatalf("report = %+v, want skipped with its trigger", r)
				}
				mustContain(t, r.Summary, "None of the 2 file(s) changed by this pull request", "`paths`: `src/**`")
			}
		})
	}
}

func TestEvaluateIgnoresForks(t *testing.T) {
	tests := []struct {
		name     string
		headRepo string
		wantRun  bool
	}{
		{name: "pull request from a fork", headRepo: "stranger/demo"},
		{name: "pull request from a deleted repository", headRepo: ""},
		{name: "pull request from a branch of the repository", headRepo: repo.FullName, wantRun: true},
		{name: "branch of the repository, other letter case", headRepo: "OCTO-ORG/Demo", wantRun: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, ciConfig, ciRun)
			h.evaluate(prTrigger(sha1, 7, tt.headRepo), EvalOptions{ReportConfigErrors: true})
			if runs := h.runner.Runs(); (len(runs) == 1) != tt.wantRun {
				t.Fatalf("runs = %d, want a run: %v", len(runs), tt.wantRun)
			}
			if !tt.wantRun && len(h.host.Reports()) != 0 {
				t.Fatalf("a pull request from a fork got reports: %+v", h.host.Reports())
			}
		})
	}
}

func TestEvaluateRefusals(t *testing.T) {
	tests := []struct {
		name      string
		cfg       string
		run       string
		setup     func(h *harness)
		wantTitle string
		want      string
	}{
		{name: "a missing pipelineRun file", cfg: ciConfig, wantTitle: "Could not start the pipeline", want: "The pipelineRun file `.tekton/ci.yaml` does not exist at `1111111`."},
		{name: "a param that renders a nil object", cfg: strings.Replace(ciConfig, `revision: "{{ .Revision }}"`, `revision: "{{ .Push.After }}"`, 1), run: ciRun,
			wantTitle: "Could not start the pipeline", want: ".Push is only set for push events"},
		{name: "the runner's check", cfg: ciConfig, run: ciRun, wantTitle: "Could not start the pipeline", want: "repository not onboarded",
			setup: func(h *harness) {
				h.runner.CheckError = func(ci.RunSpec) error {
					return &ci.Refusal{Title: "Could not start the pipeline", Reason: "repository not onboarded: namespace ci-demo not found"}
				}
			}},
		{name: "a runner that refuses the run", cfg: ciConfig, run: ciRun, wantTitle: "Refused", want: `references Secret "registry"`,
			setup: func(h *harness) {
				h.runner.Fail("Create", &ci.Refusal{Title: "Refused", Reason: `the PipelineRun references Secret "registry"`})
			}},
		{name: "a runner that fails", cfg: ciConfig, run: ciRun, wantTitle: "Could not start the pipeline", want: "boom",
			setup: func(h *harness) { h.runner.Fail("Create", errors.New("boom")) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, tt.cfg, tt.run)
			if tt.setup != nil {
				tt.setup(h)
			}
			h.evaluate(branchPR(sha1), EvalOptions{})
			if len(h.runner.Runs()) != 0 {
				t.Fatalf("no run may be created")
			}
			r := h.onlyReport("ci")
			if r.Conclusion != ci.Failure || r.Title != tt.wantTitle || r.Trigger == nil {
				t.Fatalf("report = %+v", r)
			}
			mustContain(t, r.Summary, tt.want)
			if got := h.m.Count(t, "octomaton.runs.created", attribute.String("result", metrics.RunFailed)); got != 1 {
				t.Fatalf("failed runs = %v", got)
			}
		})
	}
}

func TestStartAbortsWhenTheTokenCannotBeMinted(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	h.host.Fail("RepositoryToken", errors.New("the permissions requested are not granted"))
	h.evaluate(branchPR(sha1), EvalOptions{})
	run := h.run("demo-ci-1111111-1")
	if !run.CancelRequested || run.Cancellation.Reason != "could not be started" || !run.Done || run.Reported != ci.ReportedCompleted {
		t.Fatalf("the run must be cancelled and let go: %+v", run)
	}
	r := h.onlyReport("ci")
	if r.Conclusion != ci.Failure || r.Title != "The run could not be started" {
		t.Fatalf("report = %+v", r)
	}
	mustContain(t, r.Summary, "Octomaton could not start PipelineRun `ci-demo/demo-ci-1111111-1`", "minting the run's token")
	if got := h.m.Count(t, "octomaton.runs.created", attribute.String("result", metrics.RunError)); got != 1 {
		t.Fatalf("errors = %v", got)
	}
}

func TestTaskReportsAreOpened(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, strings.Replace(ciConfig, "githubToken: {workspace: github-token}", "taskChecks: true", 1), ciRun)
	h.evaluate(branchPR(sha1), EvalOptions{})
	build, test := h.onlyReport("ci / build"), h.onlyReport("ci / test")
	run := h.run("demo-ci-1111111-1")
	if run.TaskReportIDs["build"] != build.ID || run.TaskReportIDs["test"] != test.ID || run.Phase != ci.Released {
		t.Fatalf("run = %+v", run)
	}
	if build.ExternalID != "ci-demo/demo-ci-1111111-1" || build.URL != "https://runs.example/ci-demo/demo-ci-1111111-1/build" || build.Trigger == nil {
		t.Fatalf("task report = %+v", build)
	}
	mustContain(t, build.Summary, "Task `build` of **PipelineRun:**")
}

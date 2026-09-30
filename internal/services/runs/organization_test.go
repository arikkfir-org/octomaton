package runs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// orgRepo is the owner's .github repository, which declares the organization pipelines.
var orgRepo = ci.Repository{
	ID: 1002, Owner: repo.Owner, Name: ".github", FullName: repo.Owner + "/.github",
	CloneURL: "https://github.com/octo-org/.github.git", HTMLURL: "https://github.com/octo-org/.github", DefaultBranch: "main",
}

// orgConfig is .github's configuration at its default branch: its own pipeline publish, and three
// organization pipelines, two defined in .github and one in repository shared.
const orgConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - {name: publish, pipelineRun: .tekton/publish.yaml, on: {push: {branches: [main]}}}
organization:
  pipelines:
    - name: lint
      pipelineRun: .tekton/lint.yaml
      on:
        pull_request: {branches: [main]}
      params:
        repository: "{{ .Repository.FullName }}"
        revision: "{{ .Revision }}"
      githubToken: {workspace: github-token}
    - name: review
      displayName: AI Review
      pipelineRun: {repository: shared, path: review/pipelinerun.yaml}
      on:
        review_request: {reviewers: [octo-reviewer]}
      secrets: [api-key]
    - name: format
      pipelineRun: .tekton/format.yaml
      on:
        comment: {pattern: "^/format\\b"}
`

// The organization pipelines' definitions, each told apart from the others and from ciRun.
const (
	lintRun   = "kind: PipelineRun\nmetadata: {name: lint}\n"
	reviewRun = "kind: PipelineRun\nmetadata: {name: review}\n"
	formatRun = "kind: PipelineRun\nmetadata: {name: format}\n"
)

// setupOrganization serves orgConfig and the definitions of its organization pipelines, each from the
// default branch of its repository, and an open pull request #5 on demo.
func setupOrganization(h *harness) {
	h.host.SetFile(orgRepo, "", ".octomaton.yaml", orgConfig)
	h.host.SetFile(orgRepo, "", ".tekton/lint.yaml", lintRun)
	h.host.SetFile(orgRepo, "", ".tekton/format.yaml", formatRun)
	h.host.SetFile(shared, "", "review/pipelinerun.yaml", reviewRun)
	h.host.SetPullRequest(repo, openPR(func(*ci.PullRequestState) {}))
}

// runsOf returns each run's name, and the path and definition it was created from.
func runsOf(h *harness) [][3]string {
	var out [][3]string
	for _, r := range h.runner.Runs() {
		spec := h.runner.Spec(r.ID)
		out = append(out, [3]string{r.ID.Name, spec.Path, string(spec.Definition)})
	}
	return out
}

func TestOrganizationPipelinesRun(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(h *harness)
		act   func(h *harness)
		want  [][3]string
	}{
		{
			name:  "a pull request in a repository without .octomaton.yaml",
			setup: func(*harness) {},
			act:   func(h *harness) { h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true}) },
			want:  [][3]string{{"demo-lint-1111111-1", ".github:.tekton/lint.yaml", lintRun}},
		},
		{
			name:  "a pull request, with the repository's own pipelines first",
			setup: func(h *harness) { h.files(sha1, ciConfig, ciRun) },
			act:   func(h *harness) { h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true}) },
			want: [][3]string{
				{"demo-ci-1111111-1", ".tekton/ci.yaml", ciRun},
				{"demo-lint-1111111-1", ".github:.tekton/lint.yaml", lintRun},
			},
		},
		{
			name:  "a review request",
			setup: func(*harness) {},
			act:   func(h *harness) { h.requestReview("d-1", "octo-reviewer") },
			want:  [][3]string{{"demo-review-1111111-1", "shared:review/pipelinerun.yaml", reviewRun}},
		},
		{
			name:  "a comment command",
			setup: func(*harness) {},
			act:   func(h *harness) { h.svc.Handle(ctx, command(101, "maintainer", "/format")) },
			want:  [][3]string{{"demo-format-1111111-1", ".github:.tekton/format.yaml", formatRun}},
		},
		{
			name:  "a push, which no organization pipeline runs on",
			setup: func(h *harness) { h.files(sha1, ciConfig, ciRun) },
			act:   func(h *harness) { h.evaluate(pushTrigger(sha1, "main"), EvalOptions{ReportConfigErrors: true}) },
			want:  [][3]string{{"demo-ci-1111111-1", ".tekton/ci.yaml", ciRun}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupOrganization(h)
			tt.setup(h)
			tt.act(h)
			if got := runsOf(h); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("runs = %q\nwant %q", got, tt.want)
			}
			if reports := h.host.ReportsNamed(ci.ConfigReportName); len(reports) != 0 {
				t.Fatalf("configuration reports = %+v", reports)
			}
		})
	}
}

// TestOrganizationPipelineRunsBelongToTheRepository: an organization pipeline's run gets the
// repository's namespace, report, template context and token, like the repository's own.
func TestOrganizationPipelineRunsBelongToTheRepository(t *testing.T) {
	h := newHarness(t)
	setupOrganization(h)
	h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true})

	run := h.run("demo-lint-1111111-1")
	spec := h.runner.Spec(run.ID)
	if run.ID.Tenant != "ci-demo" || spec.Trigger.Repository != repo {
		t.Fatalf("run %+v of %+v, want one of %s", run.ID, spec.Trigger.Repository, repo.FullName)
	}
	if want := map[string]string{"repository": "octo-org/demo", "revision": sha1}; !reflect.DeepEqual(spec.Params, want) {
		t.Fatalf("params = %v, want %v", spec.Params, want)
	}
	if report := h.onlyReport("lint"); report.Repository != repo.FullName || report.Revision != sha1 {
		t.Fatalf("report = %+v", report)
	}
	if tokens := h.host.TokenRequests(); len(tokens) != 1 || tokens[0].RepositoryID != repo.ID {
		t.Fatalf("token requests = %+v, want one for %s", tokens, repo.FullName)
	}
}

// TestTheOrganizationRepositoryRunsTheDefaultBranchsOrganizationPipelines: a pull request on .github
// runs its own pipelines from its head, but the organization pipelines of its default branch, so it
// cannot change what it runs.
func TestTheOrganizationRepositoryRunsTheDefaultBranchsOrganizationPipelines(t *testing.T) {
	h := newHarness(t)
	setupOrganization(h)
	// The pull request adds pipeline ci, points lint at another file, and adds organization pipeline spell.
	h.host.SetFile(orgRepo, sha1, ".octomaton.yaml", `
apiVersion: octomaton.dev/v1
pipelines:
  - {name: ci, pipelineRun: .tekton/ci.yaml, on: {pull_request: {}}}
organization:
  pipelines:
    - {name: lint, pipelineRun: .tekton/lint-v2.yaml, on: {pull_request: {}}}
    - {name: spell, pipelineRun: .tekton/spell.yaml, on: {pull_request: {}}}
`)
	h.host.SetFile(orgRepo, sha1, ".tekton/ci.yaml", ciRun)
	h.host.SetFile(orgRepo, sha1, ".tekton/lint-v2.yaml", "kind: PipelineRun\nmetadata: {name: lint-v2}\n")
	h.host.SetFile(orgRepo, sha1, ".tekton/spell.yaml", "kind: PipelineRun\nmetadata: {name: spell}\n")
	t0 := prTrigger(sha1, 5, orgRepo.FullName)
	t0.Repository = orgRepo
	h.evaluate(t0, EvalOptions{ReportConfigErrors: true})

	want := [][3]string{
		{".github-ci-1111111-1", ".tekton/ci.yaml", ciRun},
		{".github-lint-1111111-1", ".github:.tekton/lint.yaml", lintRun},
	}
	if got := runsOf(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("runs = %q\nwant %q", got, want)
	}
}

func TestOrganizationConfigProblems(t *testing.T) {
	const invalidOrg = "apiVersion: octomaton.dev/v1\norganization:\n  pipelines:\n    - {bogus: 1}\n"
	tests := []struct {
		name      string
		setup     func(h *harness)
		report    bool
		wantTitle string
		wantText  []string
	}{
		{
			name: "organization pipelines declared by another repository", report: true,
			setup:     func(h *harness) { h.files(sha1, orgConfig, ciRun) },
			wantTitle: "Invalid .octomaton.yaml",
			wantText: []string{"`.octomaton.yaml` at `1111111` is invalid, so no pipeline was started",
				"organization: only the owner's .github repository declares organization pipelines"},
		},
		{
			name: "a pipeline named like an organization pipeline", report: true,
			setup: func(h *harness) {
				h.files(sha1, "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: lint, pipelineRun: .tekton/ci.yaml, on: {pull_request: {}}}\n", ciRun)
			},
			wantTitle: "Invalid .octomaton.yaml",
			wantText:  []string{`pipelines[0] (lint): name "lint" is taken by an organization pipeline of .github`},
		},
		{
			name: "invalid organization pipelines", report: true,
			setup:     func(h *harness) { h.host.SetFile(orgRepo, "", ".octomaton.yaml", invalidOrg) },
			wantTitle: "Invalid organization pipelines",
			wantText: []string{"`.octomaton.yaml` at the default branch of `octo-org/.github` (which declares the organization pipelines) is invalid, so no pipeline was started",
				"field bogus not found in pipeline"},
		},
		{
			name: "unreadable organization pipelines", report: true,
			setup: func(h *harness) {
				h.host.FailFile(orgRepo, "", ".octomaton.yaml", errors.New("GitHub is down"))
			},
			wantTitle: "Could not read organization pipelines",
			wantText: []string{"Octomaton could not read `.octomaton.yaml` at the default branch of `octo-org/.github` (which declares the organization pipelines):",
				"GitHub is down", "Re-run this check to try again."},
		},
		{
			name:  "invalid organization pipelines, not reported",
			setup: func(h *harness) { h.host.SetFile(orgRepo, "", ".octomaton.yaml", invalidOrg) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupOrganization(h)
			h.files(sha1, ciConfig, ciRun)
			tt.setup(h)
			h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: tt.report})
			// No pipeline runs, the repository's own included.
			if runs := h.runner.Runs(); len(runs) != 0 {
				t.Fatalf("runs = %+v, want none", runs)
			}
			reports := h.host.Reports()
			if tt.wantTitle == "" {
				if len(reports) != 0 {
					t.Fatalf("reports = %+v, want none", reports)
				}
				return
			}
			r := h.onlyReport(ci.ConfigReportName)
			if r.Conclusion != ci.Failure || r.Title != tt.wantTitle || r.Trigger == nil || r.Trigger.Revision != sha1 {
				t.Fatalf("report = %+v", r)
			}
			mustContain(t, r.Summary, tt.wantText...)
		})
	}
}

func TestRerunOfAnOrganizationPipeline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setupOrganization(h)
	h.files(sha1, ciConfig, ciRun)
	h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true})
	h.finish("demo-lint-1111111-1", ci.Failure)

	h.svc.Rerun(ctx, rerun("maintainer", h.onlyReport("lint")))
	if spec := h.runner.Spec(h.run("demo-lint-1111111-2").ID); spec.Path != ".github:.tekton/lint.yaml" || string(spec.Definition) != lintRun {
		t.Fatalf("re-run spec = %+v", spec)
	}

	// Once .github no longer declares it, its re-run fails.
	h.finish("demo-lint-1111111-2", ci.Failure)
	h.host.SetFile(orgRepo, "", ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n")
	reports := h.host.ReportsNamed("lint")
	h.svc.Rerun(ctx, rerun("maintainer", reports[len(reports)-1]))
	reports = h.host.ReportsNamed("lint")
	last := reports[len(reports)-1]
	if len(h.runner.Runs()) != 3 || last.Title != "Pipeline not found" {
		t.Fatalf("runs = %d, last report = %+v", len(h.runner.Runs()), last)
	}
	mustContain(t, last.Summary, "Neither `.octomaton.yaml` at `1111111` nor the organization pipelines of `octo-org/.github` define pipeline `lint` anymore.")
}

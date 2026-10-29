package runs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// orgRepo is the owner's organization repository, which declares the organization pipelines.
var orgRepo = ci.Repository{
	ID: 1002, Owner: repo.Owner, Name: "tooling", FullName: repo.Owner + "/tooling",
	CloneURL: "https://github.com/octo-org/tooling.git", HTMLURL: "https://github.com/octo-org/tooling", DefaultBranch: "main",
}

// orgConfig is the organization repository's configuration at its default branch: its own pipeline
// publish, and three organization pipelines, two defined in it and one in repository shared.
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
      githubToken: {workspace: github-token, permissions: {contents: read, pull_requests: read}, repositories: all}
      secrets: [api-key]
    - name: format
      pipelineRun: .tekton/format.yaml
      on:
        comment: {pattern: "^/format\\b", allow: {users: [maintainer]}}
`

// The organization pipelines' definitions, each told apart from the others and from ciRun.
const (
	lintRun   = "kind: PipelineRun\nmetadata: {name: lint}\n"
	reviewRun = "kind: PipelineRun\nmetadata: {name: review}\n"
	formatRun = "kind: PipelineRun\nmetadata: {name: format}\n"
)

// setupOrganization makes orgRepo the organization repository, serves orgConfig and the definitions
// of its organization pipelines, each from the default branch of its repository, and an open pull
// request #5 on demo.
func setupOrganization(h *harness) {
	h.svc.OrganizationRepository = orgRepo.Name
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
			want:  [][3]string{{"demo-lint-1111111-1", "tooling:.tekton/lint.yaml", lintRun}},
		},
		{
			name:  "a pull request, with the repository's own pipelines first",
			setup: func(h *harness) { h.files(sha1, ciConfig, ciRun) },
			act:   func(h *harness) { h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true}) },
			want: [][3]string{
				{"demo-ci-1111111-1", ".tekton/ci.yaml", ciRun},
				{"demo-lint-1111111-1", "tooling:.tekton/lint.yaml", lintRun},
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
			want:  [][3]string{{"demo-format-1111111-1", "tooling:.tekton/format.yaml", formatRun}},
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

// TestOrganizationPipelineMountsSecretsOnPullRequests: an organization pipeline is read at the organization
// repository's default branch, so a pull request's run of it may mount the Secrets it lists.
func TestOrganizationPipelineMountsSecretsOnPullRequests(t *testing.T) {
	h := newHarness(t)
	setupOrganization(h)
	lint := "      githubToken: {workspace: github-token}\n"
	h.host.SetFile(orgRepo, "", ".octomaton.yaml", strings.Replace(orgConfig, lint, lint+"      secrets: [lint-key]\n", 1))
	h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true})

	run := h.run("demo-lint-1111111-1")
	if got := h.runner.Spec(run.ID).Secrets; !reflect.DeepEqual(got, []string{"lint-key"}) {
		t.Fatalf("secrets = %v, want [lint-key]", got)
	}
}

// TestOrganizationPipelineSecretsAreTheOrganizationRepositorys: an organization pipeline's Secrets
// live in the organization repository's namespace: its run gets copies of them, made before it is
// released, and again when its start is resumed.
func TestOrganizationPipelineSecretsAreTheOrganizationRepositorys(t *testing.T) {
	h := newHarness(t)
	setupOrganization(h)
	h.requestReview("d-1", "octo-reviewer")

	run := h.run("demo-review-1111111-1")
	org := ci.Repository{Owner: repo.Owner, Name: orgRepo.Name, FullName: orgRepo.FullName}
	if spec := h.runner.Spec(run.ID); !reflect.DeepEqual(spec.SecretsFrom, &org) || !reflect.DeepEqual(spec.Secrets, []string{"api-key"}) {
		t.Fatalf("spec.SecretsFrom = %+v, secrets %v; want %s's namespace", spec.SecretsFrom, spec.Secrets, orgRepo.FullName)
	}
	if want := (&ci.SecretSource{Repository: org, Names: []string{"api-key"}}); !reflect.DeepEqual(run.Secrets, want) {
		t.Fatalf("run.Secrets = %+v, want %+v", run.Secrets, want)
	}
	if got := h.runner.SecretsCopied(run.ID); got != 1 || run.Phase != ci.Released {
		t.Fatalf("copies = %d, phase %s; want 1 copy before the release", got, run.Phase)
	}
	h.runner.Update(run.ID, func(r *ci.Run) { r.Phase = ci.Held })
	if err := h.svc.Resume(context.Background(), h.run(run.ID.Name)); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := h.runner.SecretsCopied(run.ID); got != 2 {
		t.Fatalf("copies after a resume = %d, want 2", got)
	}

	// An organization pipeline without secrets gets no copies.
	h.svc.Handle(context.Background(), command(102, "maintainer", "/format"))
	if format := h.run("demo-format-1111111-1"); format.Secrets != nil || h.runner.Spec(format.ID).SecretsFrom != nil || h.runner.SecretsCopied(format.ID) != 0 {
		t.Fatalf("an organization pipeline without secrets got %+v", format.Secrets)
	}
}

// TestSecretCopiesThatFailAbortTheRun: a run whose Secrets cannot be copied is cancelled and its
// report says why.
func TestSecretCopiesThatFailAbortTheRun(t *testing.T) {
	h := newHarness(t)
	setupOrganization(h)
	h.runner.Fail("CopySecrets", errors.New("Secret api-key is not in namespace ci-tooling"))
	h.requestReview("d-1", "octo-reviewer")

	run := h.run("demo-review-1111111-1")
	if !run.CancelRequested || run.Cancellation.Reason != "could not be started" || !run.Done {
		t.Fatalf("run = %+v, want it aborted", run)
	}
	if r := h.onlyReport("AI Review"); r.Conclusion != ci.Failure || !strings.Contains(r.Summary, "copying the run's Secrets: Secret api-key is not in namespace ci-tooling") {
		t.Fatalf("report = %+v", r)
	}
}

// TestTokenForEveryRepository: a pipeline that asks for a token for every repository gets one token
// for the whole installation, with the permissions it asks for, not one restricted to its repository.
func TestTokenForEveryRepository(t *testing.T) {
	h := newHarness(t)
	setupOrganization(h)
	h.requestReview("d-1", "octo-reviewer")

	run := h.run("demo-review-1111111-1")
	want := &ci.TokenSettings{Workspace: "github-token", Permissions: map[string]string{"contents": "read", "pull_requests": "read"}, AllRepositories: true}
	if got := h.runner.Spec(run.ID).Token; !reflect.DeepEqual(got, want) {
		t.Fatalf("token settings = %+v, want %+v", got, want)
	}
	tokens := h.host.TokenRequests()
	if len(tokens) != 1 || tokens[0].RepositoryID != 0 || tokens[0].InstallationID != installationID || !reflect.DeepEqual(tokens[0].Permissions, want.Permissions) {
		t.Fatalf("token requests = %+v, want one for every repository of installation %d", tokens, installationID)
	}
	if tok, ok := h.runner.Token(run.ID); !ok || tok.Value != "token-1" {
		t.Fatalf("the run's token = %+v, %v", tok, ok)
	}
}

// TestTheOrganizationRepositoryRunsTheDefaultBranchsOrganizationPipelines: a pull request on the
// organization repository runs its own pipelines from its head, but the organization pipelines of its
// default branch, so it cannot change what it runs.
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
		{"tooling-ci-1111111-1", ".tekton/ci.yaml", ciRun},
		{"tooling-lint-1111111-1", "tooling:.tekton/lint.yaml", lintRun},
	}
	if got := runsOf(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("runs = %q\nwant %q", got, want)
	}
}

// TestOrganizationPipelinesOnBranchPushes: an organization pipeline that runs on every branch's push
// is read at the organization repository's default branch, also on that repository's own branches,
// which cannot change it, and its runs are marked as an organization pipeline's, which a
// ServiceAccount's octomaton.dev/organization-pipelines admits whatever the branch. A repository's own
// pipeline on the same push is not.
func TestOrganizationPipelinesOnBranchPushes(t *testing.T) {
	const (
		pagesRun   = "kind: PipelineRun\nmetadata: {name: pages}\n"
		pagesV2Run = "kind: PipelineRun\nmetadata: {name: pages-v2}\n"
	)
	heading := "organization:\n  pipelines:\n"
	orgCfg := strings.Replace(orgConfig, heading, heading+"    - {name: pages, pipelineRun: .tekton/pages.yaml, on: {push: {branches: [\"**\"], paths: [docs/**]}}}\n", 1)
	// A branch of the organization repository that points pages at another file, on every push.
	rewritten := "apiVersion: octomaton.dev/v1\norganization:\n  pipelines:\n    - {name: pages, pipelineRun: .tekton/pages-v2.yaml, on: {push: {}}}\n"
	anyBranch := strings.Replace(ciConfig, "push: {branches: [main]}", "push: {branches: [\"**\"]}", 1)
	docs := ci.ChangedFiles{Files: []string{"docs/walkthrough.md"}, Complete: true}
	type run struct {
		name, path, definition string
		organization           bool
	}
	tests := []struct {
		name    string
		repo    ci.Repository
		branch  string
		created bool // the push creates the branch
		changed ci.ChangedFiles
		setup   func(h *harness)
		want    []run
	}{
		{name: "a branch of another repository", repo: repo, branch: "feat/x", changed: docs,
			want: []run{{"demo-pages-1111111-1", "tooling:.tekton/pages.yaml", pagesRun, true}}},
		{name: "main", repo: repo, branch: "main", changed: docs,
			want: []run{{"demo-pages-1111111-1", "tooling:.tekton/pages.yaml", pagesRun, true}}},
		{name: "a new branch's first push, which counts as changing every path", repo: repo, branch: "feat/x", created: true,
			want: []run{{"demo-pages-1111111-1", "tooling:.tekton/pages.yaml", pagesRun, true}}},
		{name: "a push that changes no docs", repo: repo, branch: "feat/x", changed: ci.ChangedFiles{Files: []string{"main.go"}, Complete: true}},
		{name: "beside the repository's own pipeline", repo: repo, branch: "feat/x", changed: docs,
			setup: func(h *harness) { h.files(sha1, anyBranch, ciRun) },
			want: []run{
				{"demo-ci-1111111-1", ".tekton/ci.yaml", ciRun, false},
				{"demo-pages-1111111-1", "tooling:.tekton/pages.yaml", pagesRun, true},
			}},
		{name: "a branch of the organization repository that rewrites the pipeline", repo: orgRepo, branch: "feat/x", changed: docs,
			setup: func(h *harness) {
				h.host.SetFile(orgRepo, sha1, ".octomaton.yaml", rewritten)
				h.host.SetFile(orgRepo, sha1, ".tekton/pages.yaml", pagesV2Run)
				h.host.SetFile(orgRepo, sha1, ".tekton/pages-v2.yaml", pagesV2Run)
			},
			want: []run{{"tooling-pages-1111111-1", "tooling:.tekton/pages.yaml", pagesRun, true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupOrganization(h)
			h.host.SetFile(orgRepo, "", ".octomaton.yaml", orgCfg)
			h.host.SetFile(orgRepo, "", ".tekton/pages.yaml", pagesRun)
			if tt.setup != nil {
				tt.setup(h)
			}
			tr := pushTrigger(sha1, tt.branch)
			tr.Repository = tt.repo
			if tt.created {
				tr.Push.Before, tr.Push.Created = strings.Repeat("0", 40), true
			} else {
				h.host.SetComparison(tt.repo, baseSHA, sha1, tt.changed)
			}
			h.evaluate(tr, EvalOptions{ReportConfigErrors: true})

			var got []run
			for _, r := range h.runner.Runs() {
				spec := h.runner.Spec(r.ID)
				got = append(got, run{r.ID.Name, spec.Path, string(spec.Definition), spec.Organization})
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("runs = %+v\nwant %+v", got, tt.want)
			}
		})
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
				"organization: only the owner's tooling repository declares organization pipelines"},
		},
		{
			name: "a pipeline named like an organization pipeline", report: true,
			setup: func(h *harness) {
				h.files(sha1, "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: lint, pipelineRun: .tekton/ci.yaml, on: {pull_request: {}}}\n", ciRun)
			},
			wantTitle: "Invalid .octomaton.yaml",
			wantText:  []string{`pipelines[0] (lint): name "lint" is taken by an organization pipeline of tooling`},
		},
		{
			name: "invalid organization pipelines", report: true,
			setup:     func(h *harness) { h.host.SetFile(orgRepo, "", ".octomaton.yaml", invalidOrg) },
			wantTitle: "Invalid organization pipelines",
			wantText: []string{"`.octomaton.yaml` at the default branch of `octo-org/tooling` (which declares the organization pipelines) is invalid, so no pipeline was started",
				"field bogus not found in pipeline"},
		},
		{
			name: "unreadable organization pipelines", report: true,
			setup: func(h *harness) {
				h.host.FailFile(orgRepo, "", ".octomaton.yaml", errors.New("GitHub is down"))
			},
			wantTitle: "Could not read organization pipelines",
			wantText: []string{"Octomaton could not read `.octomaton.yaml` at the default branch of `octo-org/tooling` (which declares the organization pipelines):",
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
	if spec := h.runner.Spec(h.run("demo-lint-1111111-2").ID); spec.Path != "tooling:.tekton/lint.yaml" || string(spec.Definition) != lintRun {
		t.Fatalf("re-run spec = %+v", spec)
	}

	// Once the organization repository no longer declares it, its re-run fails.
	h.finish("demo-lint-1111111-2", ci.Failure)
	h.host.SetFile(orgRepo, "", ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n")
	reports := h.host.ReportsNamed("lint")
	h.svc.Rerun(ctx, rerun("maintainer", reports[len(reports)-1]))
	reports = h.host.ReportsNamed("lint")
	last := reports[len(reports)-1]
	if len(h.runner.Runs()) != 3 || last.Title != "Pipeline not found" {
		t.Fatalf("runs = %d, last report = %+v", len(h.runner.Runs()), last)
	}
	mustContain(t, last.Summary, "Neither `.octomaton.yaml` at `1111111` nor the organization pipelines of `octo-org/tooling` define pipeline `lint` anymore.")
}

// TestWithoutAnOrganizationRepository: without the setting, no configuration is read for organization
// pipelines, and a configuration that declares some is invalid.
func TestWithoutAnOrganizationRepository(t *testing.T) {
	tests := []struct {
		name      string
		own       string
		wantRuns  [][3]string
		wantTitle string
		wantText  string
	}{
		{name: "the repository's own pipelines run", own: ciConfig,
			wantRuns: [][3]string{{"demo-ci-1111111-1", ".tekton/ci.yaml", ciRun}}},
		{name: "organization pipelines are a configuration error", own: orgConfig,
			wantTitle: "Invalid .octomaton.yaml", wantText: "organization: this Octomaton has no organization repository, so it reads no organization pipelines"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupOrganization(h)
			h.svc.OrganizationRepository = ""
			// Reading the organization repository would fail every pipeline.
			h.host.FailFile(orgRepo, "", ".octomaton.yaml", errors.New("GitHub is down"))
			h.files(sha1, tt.own, ciRun)
			h.evaluate(branchPR(sha1), EvalOptions{ReportConfigErrors: true})
			if got := runsOf(h); !reflect.DeepEqual(got, tt.wantRuns) {
				t.Fatalf("runs = %q\nwant %q", got, tt.wantRuns)
			}
			for _, read := range h.host.Reads() {
				if !strings.HasPrefix(read, repo.FullName+"@") {
					t.Fatalf("read %s: only the repository's own files may be read", read)
				}
			}
			if tt.wantTitle == "" {
				if reports := h.host.ReportsNamed(ci.ConfigReportName); len(reports) != 0 {
					t.Fatalf("configuration reports = %+v", reports)
				}
				return
			}
			r := h.onlyReport(ci.ConfigReportName)
			if r.Title != tt.wantTitle {
				t.Fatalf("report = %+v", r)
			}
			mustContain(t, r.Summary, tt.wantText)
		})
	}
}

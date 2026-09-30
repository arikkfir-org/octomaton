package lint

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
)

const goodConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {branches: [main]}
      merge_group:
    params:
      revision: "{{ .Revision }}"
      pr: "{{ if .PullRequest }}{{ .PullRequest.Number }}{{ end }}"
    githubToken: {workspace: github-token}
    taskChecks: true
`

// renderer stands in for the runner's: it fails definitions and specs as told, and renders the rest
// as their params.
type renderer struct {
	definitionErr error
	renderErr     func(ci.RunSpec) error
	specs         []ci.RunSpec
}

func (r *renderer) CheckDefinition(spec ci.RunSpec) error { return r.definitionErr }

func (r *renderer) Render(spec ci.RunSpec) (map[string]any, error) {
	r.specs = append(r.specs, spec)
	if r.renderErr != nil {
		if err := r.renderErr(spec); err != nil {
			return nil, err
		}
	}
	return map[string]any{"name": spec.Trigger.Repository.Name + "-" + spec.Trigger.Pipeline, "params": spec.Params}, nil
}

func repoDir(t *testing.T, cfg string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".octomaton.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLint(t *testing.T) {
	run := map[string]string{".tekton/ci.yaml": "kind: PipelineRun\n"}
	tests := []struct {
		name         string
		cfg          string
		files        map[string]string
		renderer     *renderer
		permissions  error
		want         []string // problems, as "<file>: <message>" with the file relative to the directory
		wantRendered []string // "<pipeline> on <event>"
	}{
		{name: "valid", cfg: goodConfig, files: run, wantRendered: []string{"ci on pull_request", "ci on merge_group"}},
		{name: "an invalid configuration", cfg: "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, unknown: 1}\n",
			want: []string{".octomaton.yaml: line 3: field unknown not found in pipeline"}},
		{name: "permissions the code host cannot grant", cfg: strings.Replace(goodConfig, "{workspace: github-token}", "{workspace: w, permissions: {contnet: read}}", 1),
			permissions: errors.New("unknown permission contnet"), want: []string{".octomaton.yaml: pipelines[0] (ci): githubToken.permissions: unknown permission contnet"}},
		{name: "a missing definition", cfg: goodConfig, want: []string{".octomaton.yaml: pipeline ci: pipelineRun: open "}},
		{name: "a bad definition is reported once", cfg: goodConfig, files: run, renderer: &renderer{definitionErr: errors.New("expected exactly one YAML document, found 2")},
			want: []string{".tekton/ci.yaml: expected exactly one YAML document, found 2"}},
		{name: "a run the runner refuses", cfg: goodConfig, files: run,
			renderer: &renderer{renderErr: func(spec ci.RunSpec) error {
				if spec.Trigger.Event == ci.EventMergeGroup {
					return errors.New(`the PipelineRun references Secret "prod-db"`)
				}
				return nil
			}},
			want: []string{`.tekton/ci.yaml: the PipelineRun references Secret "prod-db"`}, wantRendered: []string{"ci on pull_request"}},
		{name: "a param only valid for another event", files: run,
			cfg:          strings.Replace(goodConfig, `pr: "{{ if .PullRequest }}{{ .PullRequest.Number }}{{ end }}"`, `pr: "{{ .PullRequest.Number }}"`, 1),
			want:         []string{".octomaton.yaml: pipeline ci, on merge_group: param \"pr\": "},
			wantRendered: []string{"ci on pull_request"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := repoDir(t, tt.cfg, tt.files)
			r := tt.renderer
			if r == nil {
				r = &renderer{}
			}
			l := &Linter{Renderer: r, CheckPermissions: func(map[string]string) error { return tt.permissions }}
			res := l.Lint(filepath.Join(dir, ".octomaton.yaml"))
			var problems []string
			for _, p := range res.Problems {
				problems = append(problems, strings.TrimPrefix(p.String(), dir+string(filepath.Separator)))
			}
			if len(problems) != len(tt.want) {
				t.Fatalf("problems = %q, want %q", problems, tt.want)
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(problems[i], w) {
					t.Fatalf("problem %d = %q, want it to start with %q", i, problems[i], w)
				}
			}
			var rendered []string
			for _, rr := range res.Rendered {
				rendered = append(rendered, rr.Pipeline+" on "+rr.Event)
			}
			if !slices.Equal(rendered, tt.wantRendered) {
				t.Fatalf("rendered = %q, want %q", rendered, tt.wantRendered)
			}
		})
	}
}

func TestLintRendersWithPlaceholders(t *testing.T) {
	dir := repoDir(t, goodConfig, map[string]string{".tekton/ci.yaml": "kind: PipelineRun\n"})
	r := &renderer{}
	(&Linter{Renderer: r}).Lint(filepath.Join(dir, ".octomaton.yaml"))
	if len(r.specs) != 2 {
		t.Fatalf("specs = %d, want one per event", len(r.specs))
	}
	spec := r.specs[0]
	if spec.Trigger.Repository.Name != "octo-repo" || spec.Trigger.Pipeline != "ci" || spec.Trigger.Event != ci.EventPullRequest ||
		spec.Params["pr"] != "1" || spec.Token == nil || spec.Token.Workspace != "github-token" || !spec.TaskReports || spec.Path != ".tekton/ci.yaml" {
		t.Fatalf("spec = %+v", spec)
	}
	if r.specs[1].Params["pr"] != "" {
		t.Fatalf("a merge group has no pull request: %v", r.specs[1].Params)
	}
}

func TestRun(t *testing.T) {
	dir := repoDir(t, goodConfig, map[string]string{".tekton/ci.yaml": "kind: PipelineRun\n"})
	l := &Linter{Renderer: &renderer{}}
	var stdout, stderr bytes.Buffer
	if code := l.Run([]string{dir}, false, &stdout, &stderr); code != ExitOK || !strings.Contains(stdout.String(), "ok (2 pipeline runs rendered)") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := l.Run([]string{filepath.Join(dir, ".octomaton.yaml")}, true, &stdout, &stderr); code != ExitOK {
		t.Fatalf("render exit %d", code)
	}
	for _, want := range []string{"---\n# " + filepath.Join(dir, ".octomaton.yaml") + ": pipeline ci on pull_request\n", "name: octo-repo-ci\n", "pr: \"1\""} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("rendered output lacks %q:\n%s", want, stdout.String())
		}
	}
	bad := repoDir(t, "apiVersion: nope\n", nil)
	stderr.Reset()
	if code := l.Run([]string{bad}, false, &stdout, &stderr); code != ExitProblems || !strings.Contains(stderr.String(), "1 problem(s) found") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if code := l.Run([]string{filepath.Join(dir, "missing")}, false, &stdout, &stderr); code != ExitProblems {
		t.Fatalf("a missing path is a problem, exit %d", code)
	}
	if code := l.Run(nil, false, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("no paths is a usage error, exit %d", code)
	}
}

// reviewConfig has a review pipeline whose definition is in another repository, beside one in the repository itself.
const reviewConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: review
    pipelineRun: {repository: shared, path: review/pipelinerun.yaml}
    on:
      review_request: {reviewers: [octo-reviewer]}
    params:
      number: "{{ .PullRequest.Number }}"
      reviewer: "{{ .ReviewRequest.Reviewer }}"
    secrets: [api-key]
  - name: local-review
    pipelineRun: .tekton/review.yaml
    on:
      review_request: {reviewers: [octo-reviewer]}
    params:
      reviewer: "{{ .ReviewRequest.Reviewer }}"
    secrets: [api-key]
`

func TestLintDefinitionsInOtherRepositories(t *testing.T) {
	tests := []struct {
		name         string
		cfg          string
		want         []string
		wantNotes    []string
		wantRendered []string
	}{
		{
			name:         "noted, with its templates still checked",
			cfg:          reviewConfig,
			wantNotes:    []string{"pipeline review: pipelineRun shared:review/pipelinerun.yaml is in another repository, so its runs are not rendered"},
			wantRendered: []string{"local-review on review_request"},
		},
		{
			name:      "a template that fails for its event",
			cfg:       strings.Replace(reviewConfig, `number: "{{ .PullRequest.Number }}"`, `author: "{{ .Comment.Author }}"`, 1),
			want:      []string{`.octomaton.yaml: pipeline review, on review_request: param "author": `},
			wantNotes: []string{"pipeline review: pipelineRun shared:review/pipelinerun.yaml is in another repository, so its runs are not rendered"},
			// The local pipeline still renders.
			wantRendered: []string{"local-review on review_request"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := repoDir(t, tt.cfg, map[string]string{".tekton/review.yaml": "kind: PipelineRun\n"})
			r := &renderer{}
			res := (&Linter{Renderer: r}).Lint(filepath.Join(dir, ".octomaton.yaml"))
			var problems []string
			for _, p := range res.Problems {
				problems = append(problems, strings.TrimPrefix(p.String(), dir+string(filepath.Separator)))
			}
			if len(problems) != len(tt.want) {
				t.Fatalf("problems = %q, want %q", problems, tt.want)
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(problems[i], w) {
					t.Fatalf("problem %d = %q, want it to start with %q", i, problems[i], w)
				}
			}
			if !slices.Equal(res.Notes, tt.wantNotes) {
				t.Fatalf("notes = %q, want %q", res.Notes, tt.wantNotes)
			}
			var rendered []string
			for _, rr := range res.Rendered {
				rendered = append(rendered, rr.Pipeline+" on "+rr.Event)
			}
			if !slices.Equal(rendered, tt.wantRendered) {
				t.Fatalf("rendered = %q, want %q", rendered, tt.wantRendered)
			}
			for _, spec := range r.specs {
				if spec.Trigger.Pipeline == "review" {
					t.Fatalf("the other repository's definition must not reach the renderer: %+v", spec)
				}
				if !slices.Equal(spec.Secrets, []string{"api-key"}) || spec.Params["reviewer"] != "octo-reviewer" {
					t.Fatalf("spec = %+v, want the pipeline's secrets and the sample reviewer", spec)
				}
			}
		})
	}
}

func TestRunPrintsNotes(t *testing.T) {
	dir := repoDir(t, reviewConfig, map[string]string{".tekton/review.yaml": "kind: PipelineRun\n"})
	var stdout, stderr bytes.Buffer
	if code := (&Linter{Renderer: &renderer{}}).Run([]string{dir}, false, &stdout, &stderr); code != ExitOK {
		t.Fatalf("a note is not a problem: exit %d, stderr %q", code, stderr.String())
	}
	want := filepath.Join(dir, ".octomaton.yaml") + ": note: pipeline review: pipelineRun shared:review/pipelinerun.yaml is in another repository"
	if !strings.Contains(stderr.String(), want) || !strings.Contains(stdout.String(), "ok (1 pipeline runs rendered)") {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

// orgConfig is an organization repository's configuration: its own pipeline, and organization
// pipelines defined in it and in another repository.
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
        revision: "{{ .Revision }}"
    - name: review
      pipelineRun: {repository: tooling, path: reviewer/pipelinerun.yaml}
      on:
        review_request: {reviewers: [octo-reviewer]}
      secrets: [api-key]
`

func TestLintOrganizationPipelines(t *testing.T) {
	const note = "organization pipeline review: pipelineRun tooling:reviewer/pipelinerun.yaml is in another repository, so its runs are not rendered"
	both := map[string]string{".tekton/publish.yaml": "kind: PipelineRun\n", ".tekton/lint.yaml": "kind: PipelineRun\n"}
	tests := []struct {
		name         string
		cfg          string
		files        map[string]string
		want         []string
		wantRendered []string
	}{
		{name: "rendered after the repository's own", cfg: orgConfig, files: both, wantRendered: []string{"publish on push", "lint on pull_request"}},
		{name: "a missing definition", cfg: orgConfig, files: map[string]string{".tekton/publish.yaml": "kind: PipelineRun\n"},
			want: []string{".octomaton.yaml: organization pipeline lint: pipelineRun: open "}, wantRendered: []string{"publish on push"}},
		{name: "a template that fails for its event", files: both,
			cfg:          strings.Replace(orgConfig, `revision: "{{ .Revision }}"`, `author: "{{ .Comment.Author }}"`, 1),
			want:         []string{`.octomaton.yaml: organization pipeline lint, on pull_request: param "author": `},
			wantRendered: []string{"publish on push"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := repoDir(t, tt.cfg, tt.files)
			res := (&Linter{Renderer: &renderer{}}).Lint(filepath.Join(dir, ".octomaton.yaml"))
			var problems []string
			for _, p := range res.Problems {
				problems = append(problems, strings.TrimPrefix(p.String(), dir+string(filepath.Separator)))
			}
			if len(problems) != len(tt.want) {
				t.Fatalf("problems = %q, want %q", problems, tt.want)
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(problems[i], w) {
					t.Fatalf("problem %d = %q, want it to start with %q", i, problems[i], w)
				}
			}
			if !slices.Equal(res.Notes, []string{note}) {
				t.Fatalf("notes = %q, want %q", res.Notes, note)
			}
			var rendered []string
			for _, rr := range res.Rendered {
				rendered = append(rendered, rr.Pipeline+" on "+rr.Event)
			}
			if !slices.Equal(rendered, tt.wantRendered) {
				t.Fatalf("rendered = %q, want %q", rendered, tt.wantRendered)
			}
		})
	}
}

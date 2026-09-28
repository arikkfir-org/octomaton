package lint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodConfig = `
apiVersion: switchboard.kfirs.com/v1
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

const goodRun = `
apiVersion: tekton.dev/v1
kind: PipelineRun
metadata: {generateName: ci-}
spec:
  workspaces: [{name: source, emptyDir: {}}]
  pipelineSpec:
    tasks:
      - {name: build, taskSpec: {steps: [{name: b, image: alpine, script: "true"}]}}
`

func repoDir(t *testing.T, cfg string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".switchboard.yaml"), []byte(cfg), 0o600); err != nil {
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
	tests := []struct {
		name  string
		cfg   string
		files map[string]string
		want  []string // expected problems (substrings); empty = clean
	}{
		{name: "valid", cfg: goodConfig, files: map[string]string{".tekton/ci.yaml": goodRun}},
		{name: "invalid configuration", cfg: "apiVersion: switchboard.kfirs.com/v1\npipelines:\n  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, unknown: 1}\n", want: []string{"field unknown not found in pipeline"}},
		{name: "missing PipelineRun file", cfg: goodConfig, want: []string{"pipeline ci: pipelineRun:"}},
		{name: "two documents", cfg: goodConfig, files: map[string]string{".tekton/ci.yaml": goodRun + "---\n" + goodRun}, want: []string{"exactly one YAML document"}},
		{
			name:  "param only valid for another event",
			cfg:   strings.Replace(goodConfig, `pr: "{{ if .PullRequest }}{{ .PullRequest.Number }}{{ end }}"`, `pr: "{{ .PullRequest.Number }}"`, 1),
			files: map[string]string{".tekton/ci.yaml": goodRun},
			want:  []string{"pipeline ci, on merge_group:", ".PullRequest is only set for pull_request events"},
		},
		{
			name:  "foreign Secret",
			cfg:   goodConfig,
			files: map[string]string{".tekton/ci.yaml": strings.Replace(goodRun, "{name: source, emptyDir: {}}", "{name: source, secret: {secretName: prod-db}}", 1)},
			want:  []string{`references Secret "prod-db"`},
		},
		{
			name:  "taskChecks without an inline pipeline",
			cfg:   goodConfig,
			files: map[string]string{".tekton/ci.yaml": "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: {pipelineRef: {name: p}}\n"},
			want:  []string{"sets taskChecks"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := repoDir(t, tt.cfg, tt.files)
			res := Lint(filepath.Join(dir, ".switchboard.yaml"))
			var all []string
			for _, p := range res.Problems {
				all = append(all, p.String())
			}
			joined := strings.Join(all, "\n")
			if len(tt.want) == 0 && len(res.Problems) > 0 {
				t.Fatalf("unexpected problems:\n%s", joined)
			}
			for _, w := range tt.want {
				if !strings.Contains(joined, w) {
					t.Fatalf("problems do not mention %q:\n%s", w, joined)
				}
			}
		})
	}
}

func TestRun(t *testing.T) {
	dir := repoDir(t, goodConfig, map[string]string{".tekton/ci.yaml": goodRun})
	var stdout, stderr bytes.Buffer
	if code := Run([]string{dir}, false, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ok (2 pipeline runs rendered)") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	stdout.Reset()
	if code := Run([]string{filepath.Join(dir, ".switchboard.yaml")}, true, &stdout, &stderr); code != ExitOK {
		t.Fatalf("render exit %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"# " + filepath.Join(dir, ".switchboard.yaml") + ": pipeline ci on pull_request", "name: octo-repo-ci-0123456-1", "secretName: octo-repo-ci-0123456-1-github-token", "value: \"1\""} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output lacks %q:\n%s", want, out)
		}
	}

	bad := repoDir(t, "apiVersion: nope\n", nil)
	stderr.Reset()
	if code := Run([]string{bad}, false, &stdout, &stderr); code != ExitProblems || !strings.Contains(stderr.String(), "1 problem(s) found") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if code := Run([]string{filepath.Join(dir, "missing")}, false, &stdout, &stderr); code != ExitProblems {
		t.Fatalf("a missing path is a problem, exit %d", code)
	}
	if code := Run(nil, false, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("no paths is a usage error, exit %d", code)
	}
}

package pipelines

import (
	"strings"
	"testing"
)

const matchConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - {name: any-push, pipelineRun: a.yaml, on: {push: {}}}
  - {name: main-only, pipelineRun: a.yaml, on: {push: {branches: [main, "release/**"]}}}
  - {name: tags-only, pipelineRun: a.yaml, on: {push: {tags: ["v*"]}}}
  - {name: both, pipelineRun: a.yaml, on: {push: {branches: [main], tags: ["v*"]}}}
  - {name: prs, pipelineRun: a.yaml, on: {pull_request: {}}}
  - {name: prs-main, pipelineRun: a.yaml, on: {pull_request: {branches: [main], types: [opened, closed]}}}
  - {name: no-drafts, pipelineRun: a.yaml, on: {pull_request: {drafts: false}}}
  - {name: queue, pipelineRun: a.yaml, on: {merge_group: {branches: [main]}}}
  - {name: deploy, pipelineRun: a.yaml, on: {comment: {pattern: "^/deploy(\\s|$)", branches: [main]}}}
`

func TestMatch(t *testing.T) {
	cfg := mustParse(t, matchConfig)
	tests := []struct {
		name     string
		pipeline string
		ev       Event
		want     bool
	}{
		{"push without filters matches branches", "any-push", Event{Name: "push", Branch: "feature/x"}, true},
		{"push without filters matches tags", "any-push", Event{Name: "push", Tag: "v1.0.0"}, true},
		{"branch glob", "main-only", Event{Name: "push", Branch: "main"}, true},
		{"double-star branch glob", "main-only", Event{Name: "push", Branch: "release/2026/q1"}, true},
		{"branch not listed", "main-only", Event{Name: "push", Branch: "feature/x"}, false},
		{"branches only ignores tags", "main-only", Event{Name: "push", Tag: "v1.0.0"}, false},
		{"tags only ignores branches", "tags-only", Event{Name: "push", Branch: "main"}, false},
		{"tag glob", "tags-only", Event{Name: "push", Tag: "v1.2.3"}, true},
		{"tag not matching", "tags-only", Event{Name: "push", Tag: "release-1"}, false},
		{"both: branch", "both", Event{Name: "push", Branch: "main"}, true},
		{"both: tag", "both", Event{Name: "push", Tag: "v2"}, true},
		{"both: other branch", "both", Event{Name: "push", Branch: "dev"}, false},
		{"push trigger ignores pull requests", "any-push", Event{Name: "pull_request", Action: "opened", Branch: "main"}, false},
		{"default PR types: opened", "prs", Event{Name: "pull_request", Action: "opened", Branch: "anything"}, true},
		{"default PR types: ready_for_review", "prs", Event{Name: "pull_request", Action: "ready_for_review"}, true},
		{"default PR types: labeled is not included", "prs", Event{Name: "pull_request", Action: "labeled"}, false},
		{"drafts run by default", "prs", Event{Name: "pull_request", Action: "synchronize", Draft: true}, true},
		{"drafts: false skips drafts", "no-drafts", Event{Name: "pull_request", Action: "synchronize", Draft: true}, false},
		{"drafts: false runs ready pull requests", "no-drafts", Event{Name: "pull_request", Action: "synchronize"}, true},
		{"custom PR types", "prs-main", Event{Name: "pull_request", Action: "closed", Branch: "main"}, true},
		{"custom PR types exclude defaults", "prs-main", Event{Name: "pull_request", Action: "synchronize", Branch: "main"}, false},
		{"PR base branch filter", "prs-main", Event{Name: "pull_request", Action: "opened", Branch: "dev"}, false},
		{"merge group base branch", "queue", Event{Name: "merge_group", Branch: "main"}, true},
		{"merge group other base", "queue", Event{Name: "merge_group", Branch: "dev"}, false},
		{"comment trigger does not match events", "deploy", Event{Name: "push", Branch: "main"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := cfg.Pipeline(tt.pipeline).Match(tt.ev); got != tt.want {
				t.Fatalf("Match(%+v) = %v, want %v", tt.ev, got, tt.want)
			}
		})
	}
}

func TestPathFilter(t *testing.T) {
	tests := []struct {
		name   string
		filter PathFilter
		files  []string
		want   bool
	}{
		{"no filter matches anything", PathFilter{}, nil, true},
		{"paths match one file", PathFilter{Paths: []string{"src/**"}}, []string{"README.md", "src/a/b.go"}, true},
		{"paths match none (skipped)", PathFilter{Paths: []string{"src/**"}}, []string{"README.md", "docs/x.md"}, false},
		{"single star stays in a directory", PathFilter{Paths: []string{"src/*.go"}}, []string{"src/a/b.go"}, false},
		{"pathsIgnore: all ignored (skipped)", PathFilter{PathsIgnore: []string{"docs/**", "*.md"}}, []string{"README.md", "docs/a.md"}, false},
		{"pathsIgnore: one relevant file", PathFilter{PathsIgnore: []string{"docs/**"}}, []string{"docs/a.md", "main.go"}, true},
		{"paths and pathsIgnore", PathFilter{Paths: []string{"src/**"}, PathsIgnore: []string{"src/**/*_test.go"}}, []string{"src/x_test.go", "src/a/y_test.go"}, false},
		{"paths and pathsIgnore: relevant", PathFilter{Paths: []string{"src/**"}, PathsIgnore: []string{"**/*_test.go"}}, []string{"src/x_test.go", "src/x.go"}, true},
		{"empty change set with a filter (skipped)", PathFilter{Paths: []string{"**"}}, []string{}, false},
		{"dotfiles match double star", PathFilter{Paths: []string{"**"}}, []string{".tekton/ci.yaml"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.filter.Matches(tt.files); got != tt.want {
				t.Fatalf("Matches(%v) = %v, want %v", tt.files, got, tt.want)
			}
		})
	}
}

func TestMatchReturnsPathFilter(t *testing.T) {
	cfg := mustParse(t, `
apiVersion: octomaton.dev/v1
pipelines:
  - {name: ci, pipelineRun: a.yaml, on: {pull_request: {paths: ["src/**"], pathsIgnore: ["**/*.md"]}}}
`)
	f, ok := cfg.Pipeline("ci").Match(Event{Name: "pull_request", Action: "opened"})
	if !ok || !f.Active() || f.Paths[0] != "src/**" || f.PathsIgnore[0] != "**/*.md" {
		t.Fatalf("Match returned %+v, %v", f, ok)
	}
}

func TestMatchComment(t *testing.T) {
	cfg := mustParse(t, matchConfig)
	p := cfg.Pipeline("deploy")
	tests := []struct {
		line, base  string
		matched     bool
		declineWant string
	}{
		{"/deploy staging", "main", true, ""},
		{"/deploy", "main", true, ""},
		{"/deployment", "main", false, ""},
		{"please /deploy", "main", false, ""},
		{"/deploy now", "dev", true, "runs only on pull requests into main"},
	}
	for _, tt := range tests {
		matched, declined := p.MatchComment(tt.line, tt.base)
		if matched != tt.matched || (tt.declineWant == "") != (declined == "") || !strings.Contains(declined, tt.declineWant) {
			t.Errorf("MatchComment(%q, %q) = %v, %q", tt.line, tt.base, matched, declined)
		}
		if p.IsCommand(tt.line) != tt.matched {
			t.Errorf("IsCommand(%q) != %v", tt.line, tt.matched)
		}
	}
	if cfg.Pipeline("prs").IsCommand("/deploy") {
		t.Fatalf("a pipeline without a comment trigger has no command")
	}
}

package repoconfig

import (
	"errors"
	"strings"
	"testing"
	"time"

	"octomaton.dev/internal/tmpl"
)

// referenceExample is the .octomaton.yaml example from the hub reference.
const referenceExample = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci                           # check-run name; unique; [a-z0-9][a-z0-9-]*
    pipelineRun: .tekton/ci.yaml       # repository-relative file holding exactly one tekton.dev/v1 PipelineRun
    on:
      pull_request:
        branches: [main]               # base-branch globs; omitted = all
        types: [opened, reopened, synchronize, ready_for_review]  # default
        drafts: true                   # default; false skips draft pull requests
        paths: ["**"]                  # optional globs; no match = check reported as skipped
        pathsIgnore: []                # optional
      merge_group:                     # merge queue; optional base-branch globs
        branches: [main]
      push:
        branches: [main]               # branch globs
        tags: ["v*"]                   # tag globs
        paths: []                      # optional
      comment:                         # pull request comment command, e.g. "/deploy staging"
        pattern: "^/deploy\\b"         # regexp on the comment's first line; commenter needs write access
        branches: [main]               # optional pull request base-branch globs
      schedule:                        # cron, 5 fields, UTC; runs at the default branch head
        - cron: "0 3 * * *"
    params:                            # set/override PipelineRun spec.params; values are Go templates
      repo-url: "{{ .Repository.CloneURL }}"
      revision: "{{ .Revision }}"
    githubToken:                       # optional installation token for this repository, refreshed while the run lives
      workspace: github-token          # bound as a Secret workspace (key: token)
      permissions: {contents: read}    # default
    timeout: 1h                        # optional, sets spec.timeouts.pipeline
    concurrency:                       # optional; default for pull_request: group "pr-<number>", policy supersede
      group: "publish"                 # Go template, scoped to the repository
      policy: latest                   # supersede | queue | latest
    taskChecks: false                  # optional: also report each pipeline task as "<name> / <task>"
`

func mustParse(t *testing.T, doc string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func TestParseReferenceExample(t *testing.T) {
	cfg := mustParse(t, referenceExample)
	if len(cfg.Pipelines) != 1 {
		t.Fatalf("pipelines = %d", len(cfg.Pipelines))
	}
	p := cfg.Pipelines[0]
	on := p.On
	if on.PullRequest == nil || on.MergeGroup == nil || on.Push == nil || on.Comment == nil || len(on.Schedule) != 1 {
		t.Fatalf("triggers not all parsed: %+v", on)
	}
	if p.TimeoutDuration() != time.Hour || p.TokenWorkspace() != "github-token" || p.TokenPermissions()["contents"] != "read" {
		t.Fatalf("pipeline settings: timeout %v, workspace %q, permissions %v", p.TimeoutDuration(), p.TokenWorkspace(), p.TokenPermissions())
	}
	if p.Concurrency.Policy != PolicyLatest || p.TaskChecks {
		t.Fatalf("concurrency %+v, taskChecks %v", p.Concurrency, p.TaskChecks)
	}
	if got := p.Events(); strings.Join(got, ",") != "push,pull_request,merge_group,comment,schedule" {
		t.Fatalf("Events() = %v", got)
	}
}

// TestUnquotedOnKey proves .octomaton.yaml is parsed as YAML 1.2: with YAML 1.1
// parsers an unquoted "on" key is the boolean true and every trigger vanishes.
func TestUnquotedOnKey(t *testing.T) {
	cfg := mustParse(t, `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: ci.yaml
    on:
      push: {branches: [main]}
`)
	if cfg.Pipelines[0].On.Push == nil {
		t.Fatalf("the unquoted on: key was not read as \"on\"")
	}
	if _, ok := cfg.Pipelines[0].Match(Event{Name: "push", Branch: "main"}); !ok {
		t.Fatalf("the push trigger does not match")
	}
}

func TestNullTriggersAreEnabled(t *testing.T) {
	cfg := mustParse(t, `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: ci.yaml
    on:
      pull_request:
      merge_group:
      push: ~
`)
	on := cfg.Pipelines[0].On
	if on.PullRequest == nil || on.MergeGroup == nil || on.Push == nil {
		t.Fatalf("null triggers must be enabled: %+v", on)
	}
}

func TestParseProblems(t *testing.T) {
	const head = "apiVersion: octomaton.dev/v1\npipelines:\n"
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "empty file", yaml: "", want: "file is empty"},
		{name: "comments only", yaml: "# nothing\n", want: "file is empty"},
		{name: "two documents", yaml: head + "---\nfoo: bar\n", want: "exactly one YAML document"},
		{name: "wrong apiVersion", yaml: "apiVersion: v1\n", want: `apiVersion must be "octomaton.dev/v1"`},
		{name: "syntax error", yaml: head + "  - name: [\n", want: "yaml:"},
		{name: "unknown top-level key", yaml: head + "extra: true\n", want: "field extra not found at the top level"},
		{name: "unknown pipeline key", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, retries: 3}\n", want: "field retries not found in pipeline"},
		{name: "unknown trigger", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {release: {}}}\n", want: "field release not found in on"},
		{name: "unknown trigger key", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {branch: [main]}}}\n", want: "field branch not found in on.push"},
		{name: "duplicate YAML key", yaml: head + "  - name: ci\n    name: cd\n    pipelineRun: a.yaml\n    on: {push: {}}\n", want: "already defined"},
		{name: "wrong type", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, taskChecks: sometimes}\n", want: "cannot unmarshal"},
		{name: "duplicate names", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}}\n  - {name: ci, pipelineRun: b.yaml, on: {push: {}}}\n", want: `duplicate pipeline name "ci"`},
		{name: "missing name", yaml: head + "  - {pipelineRun: a.yaml, on: {push: {}}}\n", want: "name is required"},
		{name: "bad name", yaml: head + "  - {name: CI_Pipeline, pipelineRun: a.yaml, on: {push: {}}}\n", want: "must match"},
		{name: "reserved name", yaml: head + "  - {name: octomaton, pipelineRun: a.yaml, on: {push: {}}}\n", want: "is reserved"},
		{name: "missing pipelineRun", yaml: head + "  - {name: ci, on: {push: {}}}\n", want: "pipelineRun is required"},
		{name: "absolute pipelineRun", yaml: head + "  - {name: ci, pipelineRun: /etc/passwd, on: {push: {}}}\n", want: "clean repository-relative"},
		{name: "escaping pipelineRun", yaml: head + "  - {name: ci, pipelineRun: ../x.yaml, on: {push: {}}}\n", want: "clean repository-relative"},
		{name: "no triggers", yaml: head + "  - {name: ci, pipelineRun: a.yaml}\n", want: "at least one of"},
		{name: "bad branch glob", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {branches: [\"feature/[\"]}}}\n", want: "invalid glob"},
		{name: "bad path glob", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {pull_request: {paths: [\"src/{a\"]}}}\n", want: "on.pull_request.paths[0]: invalid glob"},
		{name: "empty glob", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {merge_group: {pathsIgnore: [\"\"]}}}\n", want: "empty pattern"},
		{name: "unknown PR type", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {pull_request: {types: [opend]}}}\n", want: `unknown pull_request action "opend"`},
		{name: "template syntax", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, params: {rev: \"{{ .Revision \"}}\n", want: "params.rev"},
		{name: "template unknown field", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, params: {rev: \"{{ .Commit }}\"}}\n", want: "can't evaluate field Commit"},
		{name: "bad param name", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, params: {\"9lives\": x}}\n", want: "not a valid Tekton parameter name"},
		{name: "token without workspace", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, githubToken: {}}\n", want: "githubToken.workspace is required"},
		{name: "unknown token permission", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, githubToken: {workspace: w, permissions: {contnet: read}}}\n", want: "githubToken.permissions"},
		{name: "bad token access", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, githubToken: {workspace: w, permissions: {contents: all}}}\n", want: "access level"},
		{name: "bad timeout", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, timeout: forever}\n", want: "is not a duration"},
		{name: "zero timeout", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, timeout: 0s}\n", want: "must be positive"},
		{name: "concurrency without group", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, concurrency: {policy: queue}}\n", want: "concurrency.group is required"},
		{name: "bad concurrency policy", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, concurrency: {group: g, policy: cancel}}\n", want: "must be supersede, queue or latest"},
		{name: "bad concurrency template", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, concurrency: {group: \"{{ .Nope }}\"}}\n", want: "concurrency.group"},
		{name: "comment without pattern", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {comment: {}}}\n", want: "on.comment.pattern is required"},
		{name: "null comment", yaml: head + "  - name: ci\n    pipelineRun: a.yaml\n    on:\n      comment:\n", want: "on.comment.pattern is required"},
		{name: "unanchored comment pattern", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {comment: {pattern: deploy}}}\n", want: "must start with ^/"},
		{name: "invalid comment pattern", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {comment: {pattern: \"^/(deploy\"}}}\n", want: "on.comment.pattern"},
		{name: "bad cron", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {schedule: [{cron: \"61 * * * *\"}]}}\n", want: "not a valid 5-field cron"},
		{name: "cron with time zone", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {schedule: [{cron: \"TZ=UTC 0 3 * * *\"}]}}\n", want: "time zones are not supported"},
		{name: "empty cron", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {schedule: [{cron: \"\"}]}}\n", want: "cron is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error = %v, want *Error", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
apiVersion: octomaton.dev/v1
pipelines:
  - {name: a, pipelineRun: a.yaml}
  - {name: b, pipelineRun: b.yaml, on: {push: {}}, timeout: x}
`))
	var ce *Error
	if !errors.As(err, &ce) || len(ce.Problems) != 2 {
		t.Fatalf("want 2 problems, got %v", err)
	}
}

func TestRenderParams(t *testing.T) {
	cfg := mustParse(t, `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: a.yaml
    on: {push: {}, pull_request: {}}
    params:
      revision: "{{ .Revision }}"
      pr: "{{ .PullRequest.Number }}"
      safe-pr: "{{ if .PullRequest }}{{ .PullRequest.Number }}{{ else }}none{{ end }}"
`)
	p := cfg.Pipeline("ci")
	got, err := p.RenderParams(tmpl.SampleFor("pull_request"))
	if err != nil || got["pr"] != "1" || got["safe-pr"] != "1" || got["revision"] == "" {
		t.Fatalf("RenderParams(pull_request) = %v, %v", got, err)
	}
	_, err = p.RenderParams(tmpl.SampleFor("push"))
	if err == nil || !strings.Contains(err.Error(), `param "pr"`) || !strings.Contains(err.Error(), ".PullRequest is only set for pull_request events") {
		t.Fatalf("RenderParams(push) error = %v, want a clear nil-object error for param pr", err)
	}
	if cfg.Pipeline("missing") != nil {
		t.Fatalf("Pipeline(missing) must be nil")
	}
}

func TestConcurrencyFor(t *testing.T) {
	cfg := mustParse(t, `
apiVersion: octomaton.dev/v1
pipelines:
  - {name: ci, pipelineRun: a.yaml, on: {push: {}, pull_request: {}}}
  - {name: docs, pipelineRun: a.yaml, on: {push: {}}, concurrency: {group: "docs-{{ .Branch }}"}}
  - {name: deploy, pipelineRun: a.yaml, on: {push: {}}, concurrency: {group: "deploy", policy: supersede}}
  - {name: off, pipelineRun: a.yaml, on: {pull_request: {}}, concurrency: {group: "{{ if false }}x{{ end }}"}}
`)
	tests := []struct {
		pipeline, event          string
		wantGroup, wantKey, want string
	}{
		{"ci", "pull_request", "pr-1", "ci/pr-1", PolicySupersede},
		{"ci", "push", "", "", ""},
		{"docs", "push", "docs-main", "docs-main", PolicyQueue},
		{"deploy", "push", "deploy", "deploy", PolicySupersede},
		{"off", "pull_request", "", "", ""},
	}
	for _, tt := range tests {
		got, err := cfg.Pipeline(tt.pipeline).ConcurrencyFor(tmpl.SampleFor(tt.event))
		if err != nil || got.Group != tt.wantGroup || got.Key != tt.wantKey || got.Policy != tt.want {
			t.Errorf("%s on %s: ConcurrencyFor = %+v, %v; want group %q key %q policy %q", tt.pipeline, tt.event, got, err, tt.wantGroup, tt.wantKey, tt.want)
		}
	}
}

func TestScheduleLast(t *testing.T) {
	cfg := mustParse(t, `
apiVersion: octomaton.dev/v1
pipelines:
  - {name: nightly, pipelineRun: a.yaml, on: {schedule: [{cron: "0 3 * * *"}, {cron: "*/15 * * * *"}]}}
`)
	s := cfg.Pipeline("nightly").Schedules()
	at := func(h, m int) time.Time { return time.Date(2026, 5, 1, h, m, 0, 0, time.UTC) }
	tests := []struct {
		sched    ScheduleTrigger
		from, to time.Time
		want     time.Time
	}{
		{s[0], at(2, 55), at(3, 5), at(3, 0)},
		{s[0], at(3, 0), at(3, 5), time.Time{}}, // (from, to]: 03:00 itself is excluded
		{s[0], at(2, 50), at(2, 59), time.Time{}},
		{s[1], at(3, 0), at(3, 40), at(3, 30)}, // the latest of several slots
	}
	for i, tt := range tests {
		if got := tt.sched.Last(tt.from, tt.to); !got.Equal(tt.want) {
			t.Errorf("case %d: Last = %v, want %v", i, got, tt.want)
		}
	}
}

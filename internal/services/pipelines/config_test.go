package pipelines

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"octomaton.dev/internal/services/ci"
)

// referenceExample is the .octomaton.yaml example from the hub reference.
const referenceExample = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci                           # identifies the pipeline and its runs; unique; [a-z0-9][a-z0-9-]*
    displayName: Continuous Integration  # optional: the check's name on GitHub (default: name); unique
    pipelineRun: .tekton/ci.yaml       # repository-relative file holding exactly one tekton.dev/v1 PipelineRun
    # pipelineRun:                     # or a file in another repository of the same owner, read at its default branch
    #   repository: tooling
    #   path: reviewer/pipelinerun.yaml
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
      review_request:                  # a review is requested from one of these users on a pull request
        reviewers: [arikkfir-reviewer] # GitHub logins, case-insensitive; required
        branches: [main]               # optional pull request base-branch globs
      schedule:                        # cron, 5 fields, UTC; runs at the default branch head
        - cron: "0 3 * * *"
    params:                            # set/override PipelineRun spec.params; values are Go templates
      repo-url: "{{ .Repository.CloneURL }}"
      revision: "{{ .Revision }}"
    githubToken:                       # optional installation token for this repository, refreshed while the run lives
      workspace: github-token          # bound as a Secret workspace (key: token)
      permissions: {contents: read}    # default
      # repositories: all              # optional: every repository the App is installed on in the owner, not just this
                                       # one; only when every trigger reads definitions from the default branch
    secrets: []                        # optional: Secrets in the run's namespace it may mount; only when every
                                       # trigger reads definitions from the default branch (comment, review_request,
                                       # schedule)
    timeout: 1h                        # optional, sets spec.timeouts.pipeline
    concurrency:                       # optional; default for pull_request and review_request:
                                       # group "pr-<number>", policy supersede
      group: "publish"                 # Go template, scoped to the repository
      policy: latest                   # supersede | queue | latest
    taskChecks: false                  # optional: also report each pipeline task as "<check> / <task>"
organization:                          # only in the organization repository: pipelines for every repository
  pipelines: []                        # same fields as pipelines; read at its default branch
`

// checkPermissions stands in for the code host's check of githubToken permissions.
func checkPermissions(m map[string]string) error {
	for name, level := range m {
		switch {
		case level != "read" && level != "write" && level != "admin":
			return fmt.Errorf("permission %q: access level must be read, write or admin (got %q)", name, level)
		case name != "contents" && name != "pull_requests" && name != "checks":
			return fmt.Errorf("unknown permission %q", name)
		}
	}
	return nil
}

func mustParse(t *testing.T, doc string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(doc), checkPermissions)
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
	if on.PullRequest == nil || on.MergeGroup == nil || on.Push == nil || on.Comment == nil || on.ReviewRequest == nil || len(on.Schedule) != 1 {
		t.Fatalf("triggers not all parsed: %+v", on)
	}
	if p.TimeoutDuration() != time.Hour || p.TokenWorkspace() != "github-token" || p.TokenPermissions()["contents"] != "read" || p.Token().AllRepositories {
		t.Fatalf("pipeline settings: timeout %v, workspace %q, token %+v", p.TimeoutDuration(), p.TokenWorkspace(), p.Token())
	}
	if ci.Policy(p.Concurrency.Policy) != ci.Latest || p.TaskChecks {
		t.Fatalf("concurrency %+v, taskChecks %v", p.Concurrency, p.TaskChecks)
	}
	if p.Name != "ci" || p.CheckName() != "Continuous Integration" {
		t.Fatalf("name %q, check name %q", p.Name, p.CheckName())
	}
	if p.PipelineRun != (PipelineRunRef{Path: ".tekton/ci.yaml"}) || len(p.Secrets) != 0 {
		t.Fatalf("pipelineRun %+v, secrets %v", p.PipelineRun, p.Secrets)
	}
	if got := p.Events(); strings.Join(got, ",") != "push,pull_request,merge_group,comment,review_request,schedule" {
		t.Fatalf("Events() = %v", got)
	}
	if cfg.Organization == nil || len(cfg.Organization.Pipelines) != 0 {
		t.Fatalf("organization = %+v", cfg.Organization)
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
	const pipeline = `
  - name: ci
    pipelineRun: ci.yaml
    on:
      pull_request:
      merge_group:
      push: ~
`
	tests := []struct {
		name string
		yaml string
		on   func(*Config) Triggers
	}{
		{name: "in pipelines", yaml: "pipelines:" + pipeline, on: func(c *Config) Triggers { return c.Pipelines[0].On }},
		{name: "in organization pipelines", yaml: "organization:\n  pipelines:" + strings.ReplaceAll(pipeline, "\n  ", "\n    "),
			on: func(c *Config) Triggers { return c.Organization.Pipelines[0].On }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			on := tt.on(mustParse(t, "apiVersion: octomaton.dev/v1\n"+tt.yaml))
			if on.PullRequest == nil || on.MergeGroup == nil || on.Push == nil {
				t.Fatalf("null triggers must be enabled: %+v", on)
			}
		})
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
		{name: "display name with spaces around it", yaml: head + "  - {name: ci, displayName: \" CI \", pipelineRun: a.yaml, on: {push: {}}}\n", want: "must not start or end with spaces"},
		{name: "display name with a control character", yaml: head + "  - {name: ci, displayName: \"CI\\tbuild\", pipelineRun: a.yaml, on: {push: {}}}\n", want: "control characters"},
		{name: "long display name", yaml: head + "  - {name: ci, displayName: " + strings.Repeat("x", 101) + ", pipelineRun: a.yaml, on: {push: {}}}\n", want: "longer than 100 characters"},
		{name: "reserved display name", yaml: head + "  - {name: ci, displayName: Octomaton, pipelineRun: a.yaml, on: {push: {}}}\n", want: `displayName "Octomaton" is reserved`},
		{name: "display name taken by a name", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}}\n  - {name: build, displayName: ci, pipelineRun: b.yaml, on: {push: {}}}\n", want: `check name "ci" is taken by pipeline "ci"`},
		{name: "duplicate display names", yaml: head + "  - {name: a, displayName: Checks, pipelineRun: a.yaml, on: {push: {}}}\n  - {name: b, displayName: Checks, pipelineRun: b.yaml, on: {push: {}}}\n", want: `check name "Checks" is taken by pipeline "a"`},
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
		{name: "token repositories other than all", yaml: head + "  - {name: review, pipelineRun: a.yaml, on: {review_request: {reviewers: [r]}}, githubToken: {workspace: w, repositories: docs}}\n", want: `githubToken.repositories "docs" must be all`},
		{name: "token repositories misspelt", yaml: head + "  - {name: review, pipelineRun: a.yaml, on: {review_request: {reviewers: [r]}}, githubToken: {workspace: w, repositories: All}}\n", want: `githubToken.repositories "All" must be all (or omitted for the run's repository)`},
		{name: "token for every repository with a pull_request trigger", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {pull_request: {}, comment: {pattern: \"^/x\"}}, githubToken: {workspace: w, repositories: all}}\n", want: "githubToken.repositories: only pipelines whose every trigger is comment, review_request or schedule may have a token for every repository"},
		{name: "token for every repository with a push trigger", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, githubToken: {workspace: w, repositories: all}}\n", want: "githubToken.repositories: only pipelines"},
		{name: "token for every repository with a merge_group trigger", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {merge_group: {}}, githubToken: {workspace: w, repositories: all}}\n", want: "githubToken.repositories: only pipelines"},
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
		{name: "review_requested as a pull_request type", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {pull_request: {types: [review_requested]}}}\n", want: "on.pull_request.types[0]: review requests trigger on.review_request, not on.pull_request"},
		{name: "review_request without reviewers", yaml: head + "  - {name: review, pipelineRun: a.yaml, on: {review_request: {}}}\n", want: "on.review_request.reviewers is required"},
		{name: "null review_request", yaml: head + "  - name: review\n    pipelineRun: a.yaml\n    on:\n      review_request:\n", want: "on.review_request.reviewers is required"},
		{name: "reviewer that is not a login", yaml: head + "  - {name: review, pipelineRun: a.yaml, on: {review_request: {reviewers: [octo-org/reviewers]}}}\n", want: `on.review_request.reviewers[0]: "octo-org/reviewers" is not a GitHub login`},
		{name: "reviewer with a leading hyphen", yaml: head + "  - {name: review, pipelineRun: a.yaml, on: {review_request: {reviewers: [-bot]}}}\n", want: "is not a GitHub login"},
		{name: "bad review_request branch glob", yaml: head + "  - {name: review, pipelineRun: a.yaml, on: {review_request: {reviewers: [bot], branches: [\"[\"]}}}\n", want: "on.review_request.branches[0]: invalid glob"},
		{name: "unknown review_request key", yaml: head + "  - {name: review, pipelineRun: a.yaml, on: {review_request: {reviewer: bot}}}\n", want: "field reviewer not found in on.review_request"},
		{name: "pipelineRun that is not a string", yaml: head + "  - {name: ci, pipelineRun: 42, on: {push: {}}}\n", want: "pipelineRun must be a file path, or a mapping with repository and path"},
		{name: "pipelineRun as a list", yaml: head + "  - {name: ci, pipelineRun: [a.yaml], on: {push: {}}}\n", want: "pipelineRun must be a file path, or a mapping with repository and path"},
		{name: "unknown pipelineRun key", yaml: head + "  - {name: ci, pipelineRun: {repository: tooling, path: a.yaml, ref: main}, on: {push: {}}}\n", want: "field ref not found in pipelineRun (only repository and path)"},
		{name: "pipelineRun without a path", yaml: head + "  - {name: ci, pipelineRun: {repository: tooling}, on: {push: {}}}\n", want: "pipelineRun is required"},
		{name: "escaping pipelineRun path in another repository", yaml: head + "  - {name: ci, pipelineRun: {repository: tooling, path: ../a.yaml}, on: {push: {}}}\n", want: "clean repository-relative"},
		{name: "pipelineRun repository with an owner", yaml: head + "  - {name: ci, pipelineRun: {repository: octo-org/tooling, path: a.yaml}, on: {push: {}}}\n", want: `pipelineRun.repository "octo-org/tooling" is not a repository name`},
		{name: "pipelineRun repository ..", yaml: head + "  - {name: ci, pipelineRun: {repository: \"..\", path: a.yaml}, on: {push: {}}}\n", want: "is not a repository name"},
		{name: "secrets with a pull_request trigger", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {pull_request: {}, comment: {pattern: \"^/x\"}}, secrets: [api-key]}\n", want: "secrets: only pipelines whose every trigger is comment, review_request or schedule may mount Secrets"},
		{name: "secrets with a push trigger", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}, secrets: [api-key]}\n", want: "secrets: only pipelines"},
		{name: "secrets with a merge_group trigger", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {merge_group: {}}, secrets: [api-key]}\n", want: "secrets: only pipelines"},
		{name: "secret name with capitals", yaml: head + "  - {name: nightly, pipelineRun: a.yaml, on: {schedule: [{cron: \"0 3 * * *\"}]}, secrets: [API_KEY]}\n", want: `secrets[0]: "API_KEY" is not a Secret name`},
		{name: "secret name too long", yaml: head + "  - {name: nightly, pipelineRun: a.yaml, on: {schedule: [{cron: \"0 3 * * *\"}]}, secrets: [" + strings.Repeat("a", 254) + "]}\n", want: "secrets[0]: " + `"` + strings.Repeat("a", 254) + `" is not a Secret name`},
		{name: "duplicate secret", yaml: head + "  - {name: nightly, pipelineRun: a.yaml, on: {schedule: [{cron: \"0 3 * * *\"}]}, secrets: [api-key, api-key]}\n", want: `secrets[1]: duplicate Secret "api-key"`},
		{name: "unknown organization key", yaml: head + "organization: {pipeline: []}\n", want: "field pipeline not found in organization"},
		{name: "invalid organization pipeline", yaml: head + "organization:\n  pipelines:\n    - {name: lint, pipelineRun: a.yaml}\n", want: "organization.pipelines[0] (lint): on: at least one of"},
		{name: "unknown organization pipeline key", yaml: head + "organization:\n  pipelines:\n    - {name: lint, pipelineRun: a.yaml, on: {push: {}}, retries: 3}\n", want: "field retries not found in pipeline"},
		{name: "organization pipeline on a schedule", yaml: head + "organization:\n  pipelines:\n    - {name: nightly, pipelineRun: a.yaml, on: {schedule: [{cron: \"0 3 * * *\"}]}}\n", want: "organization.pipelines[0] (nightly): organization pipelines can't use on.schedule"},
		{name: "organization pipeline with secrets on pull requests", yaml: head + "organization:\n  pipelines:\n    - {name: lint, pipelineRun: a.yaml, on: {pull_request: {}}, secrets: [api-key]}\n", want: "organization.pipelines[0] (lint): secrets: only pipelines"},
		{name: "organization pipeline with a token for every repository on pull requests", yaml: head + "organization:\n  pipelines:\n    - {name: lint, pipelineRun: a.yaml, on: {pull_request: {}}, githubToken: {workspace: w, repositories: all}}\n", want: "organization.pipelines[0] (lint): githubToken.repositories: only pipelines"},
		{name: "organization pipeline named like a pipeline", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}}\norganization:\n  pipelines:\n    - {name: ci, pipelineRun: b.yaml, on: {push: {}}}\n", want: `organization.pipelines[0] (ci): duplicate pipeline name "ci"`},
		{name: "organization check named like a pipeline", yaml: head + "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}}\norganization:\n  pipelines:\n    - {name: lint, displayName: ci, pipelineRun: b.yaml, on: {push: {}}}\n", want: `organization.pipelines[0] (lint): check name "ci" is taken by pipeline "ci"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), checkPermissions)
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
`), nil)
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
	got, err := p.RenderParams(SampleFor("pull_request"))
	if err != nil || got["pr"] != "1" || got["safe-pr"] != "1" || got["revision"] == "" {
		t.Fatalf("RenderParams(pull_request) = %v, %v", got, err)
	}
	_, err = p.RenderParams(SampleFor("push"))
	if err == nil || !strings.Contains(err.Error(), `param "pr"`) || !strings.Contains(err.Error(), ".PullRequest is only set for pull_request, comment and review_request events") {
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
		pipeline, event    string
		wantGroup, wantKey string
		want               ci.Policy
	}{
		{"ci", "pull_request", "pr-1", "ci/pr-1", ci.Supersede},
		{"ci", "push", "", "", ""},
		{"docs", "push", "docs-main", "docs-main", ci.Queue},
		{"deploy", "push", "deploy", "deploy", ci.Supersede},
		{"off", "pull_request", "", "", ""},
	}
	for _, tt := range tests {
		got, err := cfg.Pipeline(tt.pipeline).ConcurrencyFor(SampleFor(tt.event))
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

func TestCheckName(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "the name by default", yaml: "  - {name: ci, pipelineRun: a.yaml, on: {push: {}}}\n", want: "ci"},
		{name: "the display name when set", yaml: "  - {name: ci, displayName: Continuous Integration, pipelineRun: a.yaml, on: {push: {}}}\n", want: "Continuous Integration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParse(t, "apiVersion: octomaton.dev/v1\npipelines:\n"+tt.yaml)
			if got := cfg.Pipelines[0].CheckName(); got != tt.want {
				t.Fatalf("CheckName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPipelineRunRef(t *testing.T) {
	tests := []struct {
		name, yaml string
		want       PipelineRunRef
		wantString string
	}{
		{"a path", "pipelineRun: .tekton/ci.yaml", PipelineRunRef{Path: ".tekton/ci.yaml"}, ".tekton/ci.yaml"},
		{"a quoted path", `pipelineRun: "ci.yaml"`, PipelineRunRef{Path: "ci.yaml"}, "ci.yaml"},
		{"a mapping with only a path", "pipelineRun: {path: .tekton/ci.yaml}", PipelineRunRef{Path: ".tekton/ci.yaml"}, ".tekton/ci.yaml"},
		{"another repository", "pipelineRun: {repository: tooling, path: reviewer/pipelinerun.yaml}", PipelineRunRef{Repository: "tooling", Path: "reviewer/pipelinerun.yaml"}, "tooling:reviewer/pipelinerun.yaml"},
		{"a repository name with dots", "pipelineRun: {repository: .github, path: ci.yaml}", PipelineRunRef{Repository: ".github", Path: "ci.yaml"}, ".github:ci.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParse(t, "apiVersion: octomaton.dev/v1\npipelines:\n  - name: ci\n    "+tt.yaml+"\n    on: {push: {}}\n")
			got := cfg.Pipelines[0].PipelineRun
			if got != tt.want || got.String() != tt.wantString {
				t.Fatalf("pipelineRun = %+v (%q), want %+v (%q)", got, got.String(), tt.want, tt.wantString)
			}
		})
	}
}

// TestDefaultBranchTriggers covers the pipelines that may list Secrets and ask for a token for every
// repository: those whose every trigger reads its definitions from the default branch.
func TestDefaultBranchTriggers(t *testing.T) {
	tests := []struct {
		name, on string
	}{
		{"comment", `{comment: {pattern: "^/deploy"}}`},
		{"review_request", "{review_request: {reviewers: [octo-reviewer]}}"},
		{"schedule", `{schedule: [{cron: "0 3 * * *"}]}`},
		{"all three", `{comment: {pattern: "^/review"}, review_request: {reviewers: [octo-reviewer]}, schedule: [{cron: "0 3 * * *"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParse(t, "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: review, pipelineRun: a.yaml, on: "+tt.on+", secrets: [api-key, bot.token], githubToken: {workspace: github-token, permissions: {contents: read, pull_requests: read}, repositories: all}}\n")
			p := cfg.Pipelines[0]
			if got := p.Secrets; strings.Join(got, ",") != "api-key,bot.token" {
				t.Fatalf("secrets = %v", got)
			}
			want := &ci.TokenSettings{Workspace: "github-token", Permissions: map[string]string{"contents": "read", "pull_requests": "read"}, AllRepositories: true}
			if got := p.Token(); !reflect.DeepEqual(got, want) {
				t.Fatalf("Token() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestReviewRequestConcurrency(t *testing.T) {
	cfg := mustParse(t, `
apiVersion: octomaton.dev/v1
pipelines:
  - {name: review, pipelineRun: a.yaml, on: {review_request: {reviewers: [octo-reviewer]}}}
  - {name: deploy, pipelineRun: a.yaml, on: {comment: {pattern: "^/deploy"}}}
`)
	tests := []struct {
		pipeline, event string
		want            ci.Concurrency
	}{
		// Runs of the same pipeline on the same pull request supersede each other, as for pull_request.
		{"review", "review_request", ci.Concurrency{Group: "pr-1", Key: "review/pr-1", Policy: ci.Supersede}},
		// Comment commands stay unconstrained.
		{"deploy", "comment", ci.Concurrency{}},
	}
	for _, tt := range tests {
		got, err := cfg.Pipeline(tt.pipeline).ConcurrencyFor(SampleFor(tt.event))
		if err != nil || got != tt.want {
			t.Errorf("%s on %s: ConcurrencyFor = %+v, %v; want %+v", tt.pipeline, tt.event, got, err, tt.want)
		}
	}
}

func TestForRepository(t *testing.T) {
	const own = "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: ci, pipelineRun: .tekton/ci.yaml, on: {push: {}}}\n"
	// org is the organization repository's configuration: its own pipeline publish, and two organization pipelines.
	const org = `
apiVersion: octomaton.dev/v1
pipelines:
  - {name: publish, pipelineRun: .tekton/publish.yaml, on: {push: {}}}
organization:
  pipelines:
    - {name: review, displayName: AI Review, pipelineRun: {repository: shared, path: review/pipelinerun.yaml}, on: {review_request: {reviewers: [octo-reviewer]}}}
    - {name: lint, pipelineRun: .tekton/lint.yaml, on: {pull_request: {}}}
`
	tests := []struct {
		name         string
		repository   string
		organization string // the organization repository; "" is none
		own, org     string // "" is no file
		want         []string
		wantNil      bool
		wantErr      string
	}{
		{name: "neither file", repository: "demo", organization: "tooling", wantNil: true},
		{name: "no organization section", repository: "demo", organization: "tooling", org: own, wantNil: true},
		{name: "no organization pipelines", repository: "demo", organization: "tooling", org: "apiVersion: octomaton.dev/v1\norganization: {pipelines: []}\n", wantNil: true},
		{name: "own pipelines only", repository: "demo", organization: "tooling", own: own, want: []string{"ci .tekton/ci.yaml"}},
		{name: "an empty configuration", repository: "demo", organization: "tooling", own: "apiVersion: octomaton.dev/v1\npipelines: []\n", want: []string{}},
		{name: "organization pipelines only", repository: "demo", organization: "tooling", org: org,
			want: []string{"review shared:review/pipelinerun.yaml", "lint tooling:.tekton/lint.yaml"}},
		{name: "own, then organization pipelines", repository: "demo", organization: "tooling", own: own, org: org,
			want: []string{"ci .tekton/ci.yaml", "review shared:review/pipelinerun.yaml", "lint tooling:.tekton/lint.yaml"}},
		{name: "the organization repository", repository: "tooling", organization: "tooling", own: org, org: org,
			want: []string{"publish .tekton/publish.yaml", "review shared:review/pipelinerun.yaml", "lint tooling:.tekton/lint.yaml"}},
		{name: "the organization repository, named in another case", repository: "Tooling", organization: "tooling", own: org, org: org,
			want: []string{"publish .tekton/publish.yaml", "review shared:review/pipelinerun.yaml", "lint tooling:.tekton/lint.yaml"}},
		{name: "the organization repository runs the organization pipelines of org, not its own", repository: "tooling", organization: "tooling",
			own: strings.Replace(org, "name: lint", "name: format", 1), org: org,
			want: []string{"publish .tekton/publish.yaml", "review shared:review/pipelinerun.yaml", "lint tooling:.tekton/lint.yaml"}},
		{name: "a repository with a dot in its name", repository: "demo", organization: ".github", org: org,
			want: []string{"review shared:review/pipelinerun.yaml", "lint .github:.tekton/lint.yaml"}},
		{name: "no organization repository", repository: "demo", own: own, want: []string{"ci .tekton/ci.yaml"}},
		{name: "organization pipelines without an organization repository", repository: "tooling", own: org,
			wantErr: "organization: this Octomaton has no organization repository, so it reads no organization pipelines"},
		{name: "organization pipelines in another repository", repository: "demo", organization: "tooling", own: org, org: org,
			wantErr: "organization: only the owner's tooling repository declares organization pipelines"},
		{name: "a pipeline named like an organization pipeline", repository: "demo", organization: "tooling", org: org,
			own:     "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: lint, pipelineRun: lint.yaml, on: {push: {}}}\n",
			wantErr: `pipelines[0] (lint): name "lint" is taken by an organization pipeline of tooling`},
		{name: "a check named like an organization pipeline's", repository: "demo", organization: "tooling", org: org,
			own:     "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: ai, displayName: AI Review, pipelineRun: ai.yaml, on: {push: {}}}\n",
			wantErr: `pipelines[0] (ai): check name "AI Review" is taken by organization pipeline "review" of tooling`},
		{name: "a check named like an organization pipeline", repository: "demo", organization: "tooling", org: org,
			own:     "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: style, displayName: lint, pipelineRun: style.yaml, on: {push: {}}}\n",
			wantErr: `pipelines[0] (style): check name "lint" is taken by organization pipeline "lint" of tooling`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parse := func(doc string) *Config {
				if doc == "" {
					return nil
				}
				return mustParse(t, doc)
			}
			orgCfg := parse(tt.org)
			cfg, err := ForRepository(tt.repository, tt.organization, parse(tt.own), orgCfg)
			if tt.wantErr != "" {
				var ce *Error
				if !errors.As(err, &ce) || !strings.Contains(err.Error(), tt.wantErr) || cfg != nil {
					t.Fatalf("ForRepository = %+v, %v; want an error containing %q", cfg, err, tt.wantErr)
				}
				return
			}
			if err != nil || (cfg == nil) != tt.wantNil {
				t.Fatalf("ForRepository = %+v, %v", cfg, err)
			}
			if cfg == nil {
				return
			}
			got := []string{}
			for _, p := range cfg.Pipelines {
				got = append(got, p.Name+" "+p.PipelineRun.String())
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") || cfg.APIVersion != APIVersion || cfg.Organization != nil {
				t.Fatalf("pipelines = %q (apiVersion %q, organization %+v), want %q", got, cfg.APIVersion, cfg.Organization, tt.want)
			}
			if orgCfg != nil && orgCfg.Organization != nil {
				for _, p := range orgCfg.Organization.Pipelines {
					if p.Name == "lint" && p.PipelineRun.Repository != "" {
						t.Fatalf("ForRepository changed org's pipeline: %+v", p.PipelineRun)
					}
				}
			}
		})
	}
}

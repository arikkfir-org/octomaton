// Package repoconfig parses and validates .octomatron.yaml, the only file
// Octomatron reads from a repository, and decides which pipelines an event triggers.
package repoconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/arikkfir-org/octomatron/internal/githubapp"
	"github.com/arikkfir-org/octomatron/internal/tmpl"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

const (
	// FileName is the repository configuration file, always read from the repository root.
	FileName = ".octomatron.yaml"
	// APIVersion is the only supported configuration version.
	APIVersion = "octomatron.kfirs.com/v1"
	// ReservedName is used for configuration-level check runs and cannot name a pipeline.
	ReservedName  = "octomatron"
	maxNameLength = 63
)

// Concurrency policies.
const (
	PolicySupersede = "supersede"
	PolicyQueue     = "queue"
	PolicyLatest    = "latest"
)

// DefaultPullRequestTypes are the pull_request actions that trigger a pipeline when `types` is omitted.
var DefaultPullRequestTypes = []string{"opened", "reopened", "synchronize", "ready_for_review"}

// pullRequestActions are the pull_request webhook actions GitHub sends.
var pullRequestActions = map[string]bool{
	"assigned": true, "auto_merge_disabled": true, "auto_merge_enabled": true, "closed": true,
	"converted_to_draft": true, "demilestoned": true, "dequeued": true, "edited": true, "enqueued": true,
	"labeled": true, "locked": true, "milestoned": true, "opened": true, "ready_for_review": true,
	"reopened": true, "review_request_removed": true, "review_requested": true, "synchronize": true,
	"unassigned": true, "unlabeled": true, "unlocked": true,
}

var (
	nameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	paramNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]*$`)
)

// Config is a parsed and validated .octomatron.yaml.
type Config struct {
	APIVersion string     `yaml:"apiVersion"`
	Pipelines  []Pipeline `yaml:"pipelines"`
}

// Pipeline binds events to a PipelineRun file.
type Pipeline struct {
	Name        string            `yaml:"name"`
	PipelineRun string            `yaml:"pipelineRun"`
	On          Triggers          `yaml:"on"`
	Params      map[string]string `yaml:"params"`
	GitHubToken *GitHubToken      `yaml:"githubToken"`
	Timeout     string            `yaml:"timeout"`
	Concurrency *Concurrency      `yaml:"concurrency"`
	TaskChecks  bool              `yaml:"taskChecks"`

	timeout time.Duration
	params  map[string]*template.Template
}

// Triggers lists the events that trigger a pipeline. A pull_request,
// merge_group or push trigger written with a null value (e.g. "merge_group:")
// is enabled with default settings.
type Triggers struct {
	PullRequest *PullRequestTrigger `yaml:"pull_request"`
	MergeGroup  *MergeGroupTrigger  `yaml:"merge_group"`
	Push        *PushTrigger        `yaml:"push"`
	Comment     *CommentTrigger     `yaml:"comment"`
	Schedule    []ScheduleTrigger   `yaml:"schedule"`
}

// PullRequestTrigger configures pull_request events.
type PullRequestTrigger struct {
	Branches    []string `yaml:"branches"`
	Types       []string `yaml:"types"`
	Paths       []string `yaml:"paths"`
	PathsIgnore []string `yaml:"pathsIgnore"`
	// Drafts runs the pipeline on draft pull requests too (default true).
	Drafts *bool `yaml:"drafts"`
}

// MergeGroupTrigger configures merge_group (merge queue) events.
type MergeGroupTrigger struct {
	Branches    []string `yaml:"branches"`
	Paths       []string `yaml:"paths"`
	PathsIgnore []string `yaml:"pathsIgnore"`
}

// PushTrigger configures push events.
type PushTrigger struct {
	Branches    []string `yaml:"branches"`
	Tags        []string `yaml:"tags"`
	Paths       []string `yaml:"paths"`
	PathsIgnore []string `yaml:"pathsIgnore"`
}

// CommentTrigger runs a pipeline when someone with write access comments a
// matching command on an open, non-draft pull request.
type CommentTrigger struct {
	// Pattern is a regular expression anchored with "^/" matched against the
	// comment's first line.
	Pattern string `yaml:"pattern"`
	// Branches are globs the pull request's base branch must match (omitted = all).
	Branches []string `yaml:"branches"`

	pattern *regexp.Regexp
}

// ScheduleTrigger runs a pipeline at the head of the default branch on a cron schedule.
type ScheduleTrigger struct {
	// Cron is a standard 5-field cron expression, evaluated in UTC.
	Cron string `yaml:"cron"`

	schedule cron.Schedule
}

// Concurrency limits how runs sharing a group run together.
type Concurrency struct {
	// Group is a Go template naming the group; groups are scoped to the repository.
	Group string `yaml:"group"`
	// Policy is supersede, queue (default) or latest.
	Policy string `yaml:"policy"`

	group *template.Template
}

// GitHubToken requests a short-lived, repository-scoped installation token bound
// to the PipelineRun as a Secret workspace.
type GitHubToken struct {
	Workspace   string            `yaml:"workspace"`
	Permissions map[string]string `yaml:"permissions"`
}

// Error lists every problem found in a configuration file.
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	return "invalid " + FileName + ": " + strings.Join(e.Problems, "; ")
}

// Parse decodes data with a YAML 1.2 parser (so an unquoted `on:` key is the
// string "on", not a boolean), strictly: unknown fields, duplicate keys and type
// mismatches are errors. It then validates the result. Problems are reported as *Error.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &Error{Problems: []string{"file is empty"}}
		}
		return nil, &Error{Problems: yamlProblems(err)}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &Error{Problems: []string{"file must contain exactly one YAML document"}}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err == nil {
		enableNullTriggers(&root, &cfg)
	}
	if problems := cfg.validate(); len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return &cfg, nil
}

var typeNames = strings.NewReplacer(
	"in type repoconfig.Config", "at the top level",
	"in type repoconfig.Pipeline", "in pipeline",
	"in type repoconfig.Triggers", "in on",
	"in type repoconfig.PullRequestTrigger", "in on.pull_request",
	"in type repoconfig.MergeGroupTrigger", "in on.merge_group",
	"in type repoconfig.PushTrigger", "in on.push",
	"in type repoconfig.CommentTrigger", "in on.comment",
	"in type repoconfig.ScheduleTrigger", "in on.schedule",
	"in type repoconfig.Concurrency", "in concurrency",
	"in type repoconfig.GitHubToken", "in githubToken",
)

func yamlProblems(err error) []string {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		problems := make([]string, 0, len(te.Errors))
		for _, e := range te.Errors {
			problems = append(problems, typeNames.Replace(e))
		}
		return problems
	}
	return []string{typeNames.Replace(err.Error())}
}

// enableNullTriggers turns triggers written as "name:" (a null value) into enabled
// triggers with default settings; the struct decoder leaves them nil.
func enableNullTriggers(root *yaml.Node, cfg *Config) {
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		doc = doc.Content[0]
	}
	pipelines := mappingValue(doc, "pipelines")
	if pipelines == nil || pipelines.Kind != yaml.SequenceNode {
		return
	}
	for i, item := range pipelines.Content {
		if i >= len(cfg.Pipelines) {
			return
		}
		on := mappingValue(item, "on")
		if on == nil || on.Kind != yaml.MappingNode {
			continue
		}
		t := &cfg.Pipelines[i].On
		for j := 0; j+1 < len(on.Content); j += 2 {
			if on.Content[j+1].Tag != "!!null" {
				continue
			}
			switch on.Content[j].Value {
			case "pull_request":
				t.PullRequest = &PullRequestTrigger{}
			case "merge_group":
				t.MergeGroup = &MergeGroupTrigger{}
			case "push":
				t.Push = &PushTrigger{}
			case "comment":
				t.Comment = &CommentTrigger{}
			}
		}
	}
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func (c *Config) validate() []string {
	var problems []string
	if c.APIVersion != APIVersion {
		problems = append(problems, fmt.Sprintf("apiVersion must be %q (got %q)", APIVersion, c.APIVersion))
	}
	seen := map[string]bool{}
	for i := range c.Pipelines {
		p := &c.Pipelines[i]
		prefix := fmt.Sprintf("pipelines[%d]", i)
		if p.Name != "" {
			prefix = fmt.Sprintf("pipelines[%d] (%s)", i, p.Name)
		}
		for _, problem := range p.validate() {
			problems = append(problems, prefix+": "+problem)
		}
		if p.Name != "" {
			if seen[p.Name] {
				problems = append(problems, fmt.Sprintf("%s: duplicate pipeline name %q", prefix, p.Name))
			}
			seen[p.Name] = true
		}
	}
	return problems
}

func (p *Pipeline) validate() []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	switch {
	case p.Name == "":
		add("name is required")
	case !nameRE.MatchString(p.Name):
		add("name %q must match [a-z0-9][a-z0-9-]*", p.Name)
	case len(p.Name) > maxNameLength:
		add("name %q is longer than %d characters", p.Name, maxNameLength)
	case p.Name == ReservedName:
		add("name %q is reserved", p.Name)
	}

	switch {
	case p.PipelineRun == "":
		add("pipelineRun is required")
	case !isRepoRelativeFile(p.PipelineRun):
		add("pipelineRun %q must be a clean repository-relative file path", p.PipelineRun)
	}

	on := &p.On
	if on.PullRequest == nil && on.MergeGroup == nil && on.Push == nil && on.Comment == nil && len(on.Schedule) == 0 {
		add("on: at least one of pull_request, merge_group, push, comment or schedule is required")
	}
	if t := on.PullRequest; t != nil {
		problems = append(problems, validateGlobs("on.pull_request.branches", t.Branches)...)
		problems = append(problems, validateGlobs("on.pull_request.paths", t.Paths)...)
		problems = append(problems, validateGlobs("on.pull_request.pathsIgnore", t.PathsIgnore)...)
		for i, typ := range t.Types {
			if !pullRequestActions[typ] {
				add("on.pull_request.types[%d]: unknown pull_request action %q", i, typ)
			}
		}
	}
	if t := on.MergeGroup; t != nil {
		problems = append(problems, validateGlobs("on.merge_group.branches", t.Branches)...)
		problems = append(problems, validateGlobs("on.merge_group.paths", t.Paths)...)
		problems = append(problems, validateGlobs("on.merge_group.pathsIgnore", t.PathsIgnore)...)
	}
	if t := on.Push; t != nil {
		problems = append(problems, validateGlobs("on.push.branches", t.Branches)...)
		problems = append(problems, validateGlobs("on.push.tags", t.Tags)...)
		problems = append(problems, validateGlobs("on.push.paths", t.Paths)...)
		problems = append(problems, validateGlobs("on.push.pathsIgnore", t.PathsIgnore)...)
	}
	if t := on.Comment; t != nil {
		switch {
		case t.Pattern == "":
			add("on.comment.pattern is required")
		case !strings.HasPrefix(t.Pattern, "^/"):
			add("on.comment.pattern %q must start with ^/ (it matches the comment's first line from its start)", t.Pattern)
		default:
			re, err := regexp.Compile(t.Pattern)
			if err != nil {
				add("on.comment.pattern: %v", err)
			} else {
				t.pattern = re
			}
		}
		problems = append(problems, validateGlobs("on.comment.branches", t.Branches)...)
	}
	for i := range on.Schedule {
		s := &on.Schedule[i]
		switch {
		case strings.TrimSpace(s.Cron) == "":
			add("on.schedule[%d].cron is required", i)
		case strings.Contains(s.Cron, "TZ="):
			add("on.schedule[%d].cron: time zones are not supported; schedules are evaluated in UTC", i)
		default:
			sched, err := cron.ParseStandard(s.Cron)
			if err != nil {
				add("on.schedule[%d].cron: %q is not a valid 5-field cron expression: %v", i, s.Cron, err)
			} else {
				s.schedule = sched
			}
		}
	}

	sample := tmpl.Sample()
	sample.Pipeline = p.Name
	p.params = make(map[string]*template.Template, len(p.Params))
	for _, name := range sortedKeys(p.Params) {
		if !paramNameRE.MatchString(name) {
			add("params: %q is not a valid Tekton parameter name", name)
			continue
		}
		t, err := parseTemplate("params."+name, p.Params[name], sample)
		if err != nil {
			add("params.%s: %v", name, err)
			continue
		}
		p.params[name] = t
	}

	if c := p.Concurrency; c != nil {
		if strings.TrimSpace(c.Group) == "" {
			add("concurrency.group is required")
		} else if t, err := parseTemplate("concurrency.group", c.Group, sample); err != nil {
			add("concurrency.group: %v", err)
		} else {
			c.group = t
		}
		switch c.Policy {
		case "":
			c.Policy = PolicyQueue
		case PolicySupersede, PolicyQueue, PolicyLatest:
		default:
			add("concurrency.policy %q must be supersede, queue or latest", c.Policy)
		}
	}

	if gt := p.GitHubToken; gt != nil {
		if strings.TrimSpace(gt.Workspace) == "" {
			add("githubToken.workspace is required")
		}
		if len(gt.Permissions) > 0 {
			if _, err := githubapp.ParsePermissions(gt.Permissions); err != nil {
				add("githubToken.permissions: %v", err)
			}
		}
	}

	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		switch {
		case err != nil:
			add("timeout: %q is not a duration (examples: 30m, 1h30m)", p.Timeout)
		case d <= 0:
			add("timeout: must be positive")
		default:
			p.timeout = d
		}
	}
	return problems
}

// parseTemplate parses a template and dry-runs it against a context in which every
// field is set, so that references to unknown fields are reported up front.
func parseTemplate(name, text string, sample tmpl.Context) (*template.Template, error) {
	t, err := tmpl.Parse(name, text)
	if err != nil {
		return nil, err
	}
	if _, err := tmpl.Execute(t, sample); err != nil {
		return nil, err
	}
	return t, nil
}

func isRepoRelativeFile(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func validateGlobs(field string, globs []string) []string {
	var problems []string
	for i, g := range globs {
		if g == "" {
			problems = append(problems, fmt.Sprintf("%s[%d]: empty pattern", field, i))
		} else if !doublestar.ValidatePattern(g) {
			problems = append(problems, fmt.Sprintf("%s[%d]: invalid glob %q", field, i, g))
		}
	}
	return problems
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Pipeline returns the pipeline with the given name, or nil.
func (c *Config) Pipeline(name string) *Pipeline {
	for i := range c.Pipelines {
		if c.Pipelines[i].Name == name {
			return &c.Pipelines[i]
		}
	}
	return nil
}

// RenderParams renders every param template against ctx.
func (p *Pipeline) RenderParams(ctx tmpl.Context) (map[string]string, error) {
	out := make(map[string]string, len(p.params))
	for _, name := range sortedKeys(p.params) {
		v, err := tmpl.Execute(p.params[name], ctx)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// TimeoutDuration returns the configured pipeline timeout, or zero.
func (p *Pipeline) TimeoutDuration() time.Duration { return p.timeout }

// TokenPermissions returns the permissions requested for the GitHub token
// (contents:read when none are configured).
func (p *Pipeline) TokenPermissions() map[string]string {
	if p.GitHubToken == nil || len(p.GitHubToken.Permissions) == 0 {
		return map[string]string{"contents": "read"}
	}
	return p.GitHubToken.Permissions
}

// TokenWorkspace returns the workspace the GitHub token is bound to, or "".
func (p *Pipeline) TokenWorkspace() string {
	if p.GitHubToken == nil {
		return ""
	}
	return p.GitHubToken.Workspace
}

// ConcurrencySettings is a run's resolved concurrency group and policy.
type ConcurrencySettings struct {
	// Group is the rendered group name ("" = unconstrained).
	Group string
	// Key identifies the group within the repository.
	Key    string
	Policy string
}

// ConcurrencyFor resolves the run's concurrency group for ctx. Without a
// concurrency setting, pull_request runs of the same pipeline and pull request
// share the group "pr-<number>" (scoped to the pipeline) with policy supersede,
// and other runs are unconstrained.
func (p *Pipeline) ConcurrencyFor(ctx tmpl.Context) (ConcurrencySettings, error) {
	if c := p.Concurrency; c != nil && c.group != nil {
		group, err := tmpl.Execute(c.group, ctx)
		if err != nil {
			return ConcurrencySettings{}, fmt.Errorf("concurrency.group: %w", err)
		}
		if strings.TrimSpace(group) == "" {
			return ConcurrencySettings{}, nil
		}
		return ConcurrencySettings{Group: group, Key: group, Policy: c.Policy}, nil
	}
	if ctx.Event == "pull_request" && ctx.PullRequest != nil {
		group := fmt.Sprintf("pr-%d", ctx.PullRequest.Number)
		return ConcurrencySettings{Group: group, Key: p.Name + "/" + group, Policy: PolicySupersede}, nil
	}
	return ConcurrencySettings{}, nil
}

// Schedules returns the pipeline's schedule triggers.
func (p *Pipeline) Schedules() []ScheduleTrigger { return p.On.Schedule }

// Last returns the latest time in (from, to] the schedule fires at, or the zero time.
func (s ScheduleTrigger) Last(from, to time.Time) time.Time {
	if s.schedule == nil {
		return time.Time{}
	}
	var last time.Time
	for t := s.schedule.Next(from.UTC()); !t.IsZero() && !t.After(to); t = s.schedule.Next(t) {
		last = t
	}
	return last
}

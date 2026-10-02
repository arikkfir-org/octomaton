// Package pipelines reads .octomaton.yaml, the only file Octomaton reads from a repository besides
// the pipeline definitions it names: its schema and validation, which pipelines an event triggers,
// and the Go templates of params and concurrency groups over the documented template context.
package pipelines

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
	"octomaton.dev/internal/services/ci"
)

const (
	// FileName is the repository configuration file, always read from the repository root.
	FileName = ".octomaton.yaml"
	// APIVersion is the only supported configuration version.
	APIVersion = "octomaton.dev/v1"
	// ReservedName names the configuration's own report, so it cannot name a pipeline.
	ReservedName  = ci.ConfigReportName
	maxNameLength = 63
	// maxDisplayNameLength bounds a check name, which pages show in narrow lists.
	maxDisplayNameLength = 100
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
	nameRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	paramNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]*$`)
	loginRE      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repoNameRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	secretNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// maxSecretNameLength bounds a Kubernetes Secret name (a DNS subdomain).
const maxSecretNameLength = 253

// Config is a parsed and validated .octomaton.yaml.
type Config struct {
	APIVersion string     `yaml:"apiVersion"`
	Pipelines  []Pipeline `yaml:"pipelines"`
	// Organization lists the pipelines every repository of the owner runs. Only the owner's
	// organization repository, a server setting, may declare it.
	Organization *Organization `yaml:"organization"`
}

// Organization holds an owner's organization pipelines.
type Organization struct {
	Pipelines []Pipeline `yaml:"pipelines"`
}

// Pipeline binds events to a PipelineRun file.
type Pipeline struct {
	Name        string            `yaml:"name"`
	DisplayName string            `yaml:"displayName"`
	PipelineRun PipelineRunRef    `yaml:"pipelineRun"`
	On          Triggers          `yaml:"on"`
	Params      map[string]string `yaml:"params"`
	GitHubToken *GitHubToken      `yaml:"githubToken"`
	// Secrets name the Secrets in the run's namespace that its runs may mount besides their token.
	Secrets     []string     `yaml:"secrets"`
	Timeout     string       `yaml:"timeout"`
	Concurrency *Concurrency `yaml:"concurrency"`
	TaskChecks  bool         `yaml:"taskChecks"`

	timeout time.Duration
	params  map[string]*template.Template
}

// PipelineRunRef locates a pipeline definition: a file in the repository itself, read where its
// .octomaton.yaml is, or a file in another repository of the same owner, read at that repository's
// default branch.
type PipelineRunRef struct {
	// Repository names another repository of the same owner; empty means the repository itself.
	Repository string
	// Path is the file, relative to the repository's root.
	Path string
}

// UnmarshalYAML accepts a repository-relative path, or a mapping with repository and path.
func (r *PipelineRunRef) UnmarshalYAML(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.ScalarNode && n.Tag == "!!str":
		*r = PipelineRunRef{Path: n.Value}
		return nil
	case n.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i].Value; k != "repository" && k != "path" {
				return fmt.Errorf("line %d: field %s not found in pipelineRun (only repository and path)", n.Content[i].Line, k)
			}
		}
		var m struct {
			Repository string `yaml:"repository"`
			Path       string `yaml:"path"`
		}
		if err := n.Decode(&m); err != nil {
			return fmt.Errorf("pipelineRun: %w", err)
		}
		*r = PipelineRunRef{Repository: m.Repository, Path: m.Path}
		return nil
	}
	return fmt.Errorf("line %d: pipelineRun must be a file path, or a mapping with repository and path", n.Line)
}

// String names the definition in messages: its path, prefixed with "<repository>:" when it is in
// another repository.
func (r PipelineRunRef) String() string {
	if r.Repository == "" {
		return r.Path
	}
	return r.Repository + ":" + r.Path
}

// Triggers lists the events that trigger a pipeline. A pull_request,
// merge_group, push or comment trigger written with a null value (e.g. "merge_group:")
// is enabled with default settings.
type Triggers struct {
	PullRequest   *PullRequestTrigger   `yaml:"pull_request"`
	MergeGroup    *MergeGroupTrigger    `yaml:"merge_group"`
	Push          *PushTrigger          `yaml:"push"`
	Comment       *CommentTrigger       `yaml:"comment"`
	ReviewRequest *ReviewRequestTrigger `yaml:"review_request"`
	Schedule      []ScheduleTrigger     `yaml:"schedule"`
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

// ReviewRequestTrigger runs a pipeline when a review is requested from one of its reviewers on an
// open pull request. Like comment commands, it reads definitions from the default branch and runs
// against the pull request's head commit.
type ReviewRequestTrigger struct {
	// Reviewers are the GitHub logins whose review requests start the pipeline (case-insensitive).
	Reviewers []string `yaml:"reviewers"`
	// Branches are globs the pull request's base branch must match (omitted = all).
	Branches []string `yaml:"branches"`
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

// GitHubToken requests a short-lived installation token bound to the PipelineRun as a Secret
// workspace: for the run's repository, or for every repository of the installation.
type GitHubToken struct {
	Workspace   string            `yaml:"workspace"`
	Permissions map[string]string `yaml:"permissions"`
	// Repositories is empty for the run's repository alone, or AllRepositories.
	Repositories string `yaml:"repositories"`
}

// AllRepositories asks for a token for every repository of the installation.
const AllRepositories = "all"

// Error lists every problem found in a configuration file.
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	return "invalid " + FileName + ": " + strings.Join(e.Problems, "; ")
}

// PermissionCheck reports githubToken permissions the code host cannot grant (ci.CodeHost's
// CheckPermissions).
type PermissionCheck func(permissions map[string]string) error

// Parse decodes data with a YAML 1.2 parser (so an unquoted `on:` key is the
// string "on", not a boolean), strictly: unknown fields, duplicate keys and type
// mismatches are errors. It then validates the result, checking githubToken
// permissions with check when it is set. Problems are reported as *Error.
func Parse(data []byte, check PermissionCheck) (*Config, error) {
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
	if problems := cfg.validate(check); len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return &cfg, nil
}

var typeNames = strings.NewReplacer(
	"in type pipelines.Config", "at the top level",
	"in type pipelines.Organization", "in organization",
	"in type pipelines.Pipeline", "in pipeline",
	"in type pipelines.Triggers", "in on",
	"in type pipelines.PullRequestTrigger", "in on.pull_request",
	"in type pipelines.MergeGroupTrigger", "in on.merge_group",
	"in type pipelines.PushTrigger", "in on.push",
	"in type pipelines.CommentTrigger", "in on.comment",
	"in type pipelines.ReviewRequestTrigger", "in on.review_request",
	"in type pipelines.ScheduleTrigger", "in on.schedule",
	"in type pipelines.Concurrency", "in concurrency",
	"in type pipelines.GitHubToken", "in githubToken",
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
	enableNull(mappingValue(doc, "pipelines"), cfg.Pipelines)
	if cfg.Organization != nil {
		enableNull(mappingValue(mappingValue(doc, "organization"), "pipelines"), cfg.Organization.Pipelines)
	}
}

func enableNull(pipelines *yaml.Node, decoded []Pipeline) {
	if pipelines == nil || pipelines.Kind != yaml.SequenceNode {
		return
	}
	for i, item := range pipelines.Content {
		if i >= len(decoded) {
			return
		}
		on := mappingValue(item, "on")
		if on == nil || on.Kind != yaml.MappingNode {
			continue
		}
		t := &decoded[i].On
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
			case "review_request":
				t.ReviewRequest = &ReviewRequestTrigger{}
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

func (c *Config) validate(check PermissionCheck) []string {
	var problems []string
	if c.APIVersion != APIVersion {
		problems = append(problems, fmt.Sprintf("apiVersion must be %q (got %q)", APIVersion, c.APIVersion))
	}
	// Names and check names are unique across both lists: the organization repository runs both.
	names := newNames()
	problems = append(problems, names.validate("pipelines", c.Pipelines, check)...)
	if c.Organization != nil {
		problems = append(problems, names.validate("organization.pipelines", c.Organization.Pipelines, check)...)
		for i, p := range c.Organization.Pipelines {
			if len(p.On.Schedule) > 0 {
				problems = append(problems, fmt.Sprintf("%s: organization pipelines can't use on.schedule", label("organization.pipelines", i, p.Name)))
			}
		}
	}
	return problems
}

// names tracks the pipeline names and check names taken so far.
type names struct {
	pipelines map[string]bool
	checks    map[string]string
}

func newNames() names { return names{pipelines: map[string]bool{}, checks: map[string]string{}} }

func (n names) validate(list string, ps []Pipeline, check PermissionCheck) []string {
	var problems []string
	for i := range ps {
		p := &ps[i]
		prefix := label(list, i, p.Name)
		for _, problem := range p.validate(check) {
			problems = append(problems, prefix+": "+problem)
		}
		problems = append(problems, n.take(prefix, p)...)
	}
	return problems
}

// take claims p's name and check name, and reports those already taken.
func (n names) take(prefix string, p *Pipeline) []string {
	var problems []string
	if p.Name != "" {
		if n.pipelines[p.Name] {
			problems = append(problems, fmt.Sprintf("%s: duplicate pipeline name %q", prefix, p.Name))
		}
		n.pipelines[p.Name] = true
	}
	if check := p.CheckName(); check != "" {
		if other, ok := n.checks[check]; ok && other != p.Name {
			problems = append(problems, fmt.Sprintf("%s: check name %q is taken by pipeline %q", prefix, check, other))
		}
		n.checks[check] = p.Name
	}
	return problems
}

func label(list string, i int, name string) string {
	if name == "" {
		return fmt.Sprintf("%s[%d]", list, i)
	}
	return fmt.Sprintf("%s[%d] (%s)", list, i, name)
}

// ForRepository returns the configuration a repository runs by: own, its .octomaton.yaml (nil
// without one), with the organization pipelines of org added, the .octomaton.yaml of its owner's
// organization repository at its default branch (nil without one). organization names that
// repository; empty means there is none. It returns nil when own is nil and org declares no
// organization pipelines. A plain pipelineRun path of an organization pipeline names a file of the
// organization repository. The problems it reports are own's: organization pipelines outside the
// organization repository, and a pipeline with the name or check name of an organization pipeline,
// so no repository can replace one.
func ForRepository(repository, organization string, own, org *Config) (*Config, error) {
	var shared []Pipeline
	if org != nil && org.Organization != nil {
		shared = org.Organization.Pipelines
	}
	if own == nil {
		if len(shared) == 0 {
			return nil, nil
		}
		own = &Config{APIVersion: APIVersion}
	}
	switch {
	case own.Organization == nil:
	case organization == "":
		return nil, &Error{Problems: []string{"organization: this Octomaton has no organization repository, so it reads no organization pipelines"}}
	case !strings.EqualFold(repository, organization):
		return nil, &Error{Problems: []string{fmt.Sprintf("organization: only the owner's %s repository declares organization pipelines", organization)}}
	}
	taken := newNames()
	for i := range shared {
		taken.take("", &shared[i])
	}
	var problems []string
	for i := range own.Pipelines {
		p := &own.Pipelines[i]
		switch {
		case taken.pipelines[p.Name]:
			problems = append(problems, fmt.Sprintf("%s: name %q is taken by an organization pipeline of %s", label("pipelines", i, p.Name), p.Name, organization))
		case taken.checks[p.CheckName()] != "":
			problems = append(problems, fmt.Sprintf("%s: check name %q is taken by organization pipeline %q of %s", label("pipelines", i, p.Name), p.CheckName(), taken.checks[p.CheckName()], organization))
		}
	}
	if len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	merged := &Config{APIVersion: own.APIVersion, Pipelines: slices.Clone(own.Pipelines)}
	for _, p := range shared {
		if p.PipelineRun.Repository == "" {
			p.PipelineRun.Repository = organization
		}
		merged.Pipelines = append(merged.Pipelines, p)
	}
	return merged, nil
}

func (p *Pipeline) validate(check PermissionCheck) []string {
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
	if p.DisplayName != "" {
		switch {
		case strings.TrimSpace(p.DisplayName) != p.DisplayName:
			add("displayName %q must not start or end with spaces", p.DisplayName)
		case strings.IndexFunc(p.DisplayName, unicode.IsControl) >= 0:
			add("displayName %q must not contain control characters", p.DisplayName)
		case utf8.RuneCountInString(p.DisplayName) > maxDisplayNameLength:
			add("displayName %q is longer than %d characters", p.DisplayName, maxDisplayNameLength)
		case strings.EqualFold(p.DisplayName, ReservedName):
			add("displayName %q is reserved", p.DisplayName)
		}
	}

	switch ref := p.PipelineRun; {
	case ref.Path == "":
		add("pipelineRun is required")
	case !isRepoRelativeFile(ref.Path):
		add("pipelineRun %q must be a clean repository-relative file path", ref.Path)
	case ref.Repository != "" && (!repoNameRE.MatchString(ref.Repository) || ref.Repository == "." || ref.Repository == ".."):
		add("pipelineRun.repository %q is not a repository name", ref.Repository)
	}

	on := &p.On
	if on.PullRequest == nil && on.MergeGroup == nil && on.Push == nil && on.Comment == nil && on.ReviewRequest == nil && len(on.Schedule) == 0 {
		add("on: at least one of pull_request, merge_group, push, comment, review_request or schedule is required")
	}
	if t := on.PullRequest; t != nil {
		problems = append(problems, validateGlobs("on.pull_request.branches", t.Branches)...)
		problems = append(problems, validateGlobs("on.pull_request.paths", t.Paths)...)
		problems = append(problems, validateGlobs("on.pull_request.pathsIgnore", t.PathsIgnore)...)
		for i, typ := range t.Types {
			switch {
			case typ == "review_requested":
				add("on.pull_request.types[%d]: review requests trigger on.review_request, not on.pull_request", i)
			case !pullRequestActions[typ]:
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
	if t := on.ReviewRequest; t != nil {
		if len(t.Reviewers) == 0 {
			add("on.review_request.reviewers is required")
		}
		for i, r := range t.Reviewers {
			if !loginRE.MatchString(r) {
				add("on.review_request.reviewers[%d]: %q is not a GitHub login", i, r)
			}
		}
		problems = append(problems, validateGlobs("on.review_request.branches", t.Branches)...)
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

	sample := Sample()
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
		switch ci.Policy(c.Policy) {
		case "":
			c.Policy = string(ci.Queue)
		case ci.Supersede, ci.Queue, ci.Latest:
		default:
			add("concurrency.policy %q must be supersede, queue or latest", c.Policy)
		}
	}

	// Pull requests, merge groups and pushes read their definitions at the commit under test.
	headDefinitions := on.PullRequest != nil || on.MergeGroup != nil || on.Push != nil
	if gt := p.GitHubToken; gt != nil {
		if strings.TrimSpace(gt.Workspace) == "" {
			add("githubToken.workspace is required")
		}
		if len(gt.Permissions) > 0 && check != nil {
			if err := check(gt.Permissions); err != nil {
				add("githubToken.permissions: %v", err)
			}
		}
		switch gt.Repositories {
		case "":
		case AllRepositories:
			if headDefinitions {
				add("githubToken.repositories: only pipelines whose every trigger is comment, review_request or schedule may have a token for every repository, because only they read their definitions from the default branch")
			}
		default:
			add("githubToken.repositories %q must be %s (or omitted for the run's repository)", gt.Repositories, AllRepositories)
		}
	}

	if len(p.Secrets) > 0 && headDefinitions {
		add("secrets: only pipelines whose every trigger is comment, review_request or schedule may mount Secrets, because only they read their definitions from the default branch")
	}
	seenSecrets := map[string]bool{}
	for i, name := range p.Secrets {
		switch {
		case len(name) > maxSecretNameLength || !secretNameRE.MatchString(name):
			add("secrets[%d]: %q is not a Secret name", i, name)
		case seenSecrets[name]:
			add("secrets[%d]: duplicate Secret %q", i, name)
		}
		seenSecrets[name] = true
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
func parseTemplate(name, text string, sample TemplateContext) (*template.Template, error) {
	t, err := ParseTemplate(name, text)
	if err != nil {
		return nil, err
	}
	if _, err := ExecuteTemplate(t, sample); err != nil {
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

// CheckName is the name of the pipeline's report on the code host: its displayName, or its name.
func (p *Pipeline) CheckName() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
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
func (p *Pipeline) RenderParams(ctx TemplateContext) (map[string]string, error) {
	out := make(map[string]string, len(p.params))
	for _, name := range sortedKeys(p.params) {
		v, err := ExecuteTemplate(p.params[name], ctx)
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

// Token returns what a run of the pipeline asks for its GitHub token; nil without githubToken.
func (p *Pipeline) Token() *ci.TokenSettings {
	if p.GitHubToken == nil {
		return nil
	}
	return &ci.TokenSettings{
		Workspace:       p.GitHubToken.Workspace,
		Permissions:     p.TokenPermissions(),
		AllRepositories: p.GitHubToken.Repositories == AllRepositories,
	}
}

// TokenWorkspace returns the workspace the GitHub token is bound to, or "".
func (p *Pipeline) TokenWorkspace() string {
	if p.GitHubToken == nil {
		return ""
	}
	return p.GitHubToken.Workspace
}

// ConcurrencyFor resolves the run's concurrency group for ctx. Without a
// concurrency setting, pull_request and review_request runs of the same pipeline
// and pull request share the group "pr-<number>" (scoped to the pipeline) with
// policy supersede, and other runs are unconstrained.
func (p *Pipeline) ConcurrencyFor(ctx TemplateContext) (ci.Concurrency, error) {
	if c := p.Concurrency; c != nil && c.group != nil {
		group, err := ExecuteTemplate(c.group, ctx)
		if err != nil {
			return ci.Concurrency{}, fmt.Errorf("concurrency.group: %w", err)
		}
		if strings.TrimSpace(group) == "" {
			return ci.Concurrency{}, nil
		}
		return ci.Concurrency{Group: group, Key: group, Policy: ci.Policy(c.Policy)}, nil
	}
	if (ctx.Event == ci.EventPullRequest || ctx.Event == ci.EventReviewRequest) && ctx.PullRequest != nil {
		group := fmt.Sprintf("pr-%d", ctx.PullRequest.Number)
		return ci.Concurrency{Group: group, Key: p.Name + "/" + group, Policy: ci.Supersede}, nil
	}
	return ci.Concurrency{}, nil
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

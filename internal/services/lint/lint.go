// Package lint validates a .octomaton.yaml and the pipeline definitions it names the way Octomaton
// would at run time, without a code host or a cluster: schema, globs, templates rendered for each
// triggering event, and each definition rendered as the runner would create it.
package lint

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
	"sigs.k8s.io/yaml"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitProblems = 1
	ExitUsage    = 2
)

// Renderer renders runs as the runner would create them, without creating anything; the Tekton
// adapter's Renderer implements it.
type Renderer interface {
	// CheckDefinition reports what is wrong with a pipeline definition whatever the trigger.
	CheckDefinition(spec ci.RunSpec) error
	// Render returns the object the runner would create for spec's first attempt.
	Render(spec ci.RunSpec) (map[string]any, error)
}

// Problem is one finding.
type Problem struct {
	File    string
	Message string
}

func (p Problem) String() string { return p.File + ": " + p.Message }

// Result is what linting one configuration found.
type Result struct {
	Config   string
	Problems []Problem
	// Notes are what could not be checked, such as definitions in other repositories.
	Notes []string
	// Rendered holds each pipeline's run rendered with placeholder values for each of its events,
	// in order.
	Rendered []Rendered
}

// Rendered is one pipeline's run rendered for one event.
type Rendered struct {
	Pipeline string
	Event    string
	Object   map[string]any
}

// Linter lints configurations.
type Linter struct {
	Renderer Renderer
	// CheckPermissions checks githubToken permissions as the code host would.
	CheckPermissions pipelines.PermissionCheck
}

// ConfigPath returns the configuration file for a path argument: the file itself, or
// .octomaton.yaml in a directory.
func ConfigPath(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return filepath.Join(path, pipelines.FileName), nil
	}
	return path, nil
}

// Lint validates the configuration at configPath and every pipeline definition it names, resolved
// relative to the configuration's directory.
func (l *Linter) Lint(configPath string) Result {
	res := Result{Config: configPath}
	add := func(file, format string, args ...any) {
		res.Problems = append(res.Problems, Problem{File: file, Message: fmt.Sprintf(format, args...)})
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		add(configPath, "%v", err)
		return res
	}
	cfg, err := pipelines.Parse(data, l.CheckPermissions)
	if err != nil {
		var ce *pipelines.Error
		if errors.As(err, &ce) {
			for _, p := range ce.Problems {
				add(configPath, "%s", p)
			}
		} else {
			add(configPath, "%v", err)
		}
		return res
	}
	root := filepath.Dir(configPath)
	for i := range cfg.Pipelines {
		l.lintPipeline(&res, root, "pipeline", &cfg.Pipelines[i])
	}
	if cfg.Organization != nil {
		// Only the owner's .github repository may declare them; Octomaton reports them anywhere else.
		for i := range cfg.Organization.Pipelines {
			l.lintPipeline(&res, root, "organization pipeline", &cfg.Organization.Pipelines[i])
		}
	}
	return res
}

// lintPipeline checks one pipeline, named kind in messages, and renders its runs.
func (l *Linter) lintPipeline(res *Result, root, kind string, p *pipelines.Pipeline) {
	add := func(file, format string, args ...any) {
		res.Problems = append(res.Problems, Problem{File: file, Message: fmt.Sprintf(format, args...)})
	}
	// A definition in another repository is read from there at run time; only its templates are
	// checked here.
	var definition []byte
	file := res.Config
	if p.PipelineRun.Repository != "" {
		res.Notes = append(res.Notes, fmt.Sprintf("%s %s: pipelineRun %s is in another repository, so its runs are not rendered", kind, p.Name, p.PipelineRun))
	} else {
		file = filepath.Join(root, filepath.FromSlash(p.PipelineRun.Path))
		var err error
		if definition, err = os.ReadFile(file); err != nil {
			add(res.Config, "%s %s: pipelineRun: %v", kind, p.Name, err)
			return
		}
	}
	base := ci.RunSpec{Trigger: ci.Trigger{Pipeline: p.Name}, Definition: definition, Path: p.PipelineRun.String(), Timeout: p.TimeoutDuration(), Token: p.Token(), Secrets: p.Secrets, TaskReports: p.TaskChecks}
	if definition != nil {
		if err := l.Renderer.CheckDefinition(base); err != nil {
			add(file, "%v", err)
			return
		}
	}
	for _, event := range p.Events() {
		sample := pipelines.SampleFor(event)
		sample.Pipeline = p.Name
		spec := base
		spec.Trigger = ci.Trigger{
			Event: event, Pipeline: p.Name, Revision: sample.Revision,
			Repository: ci.Repository{Owner: sample.Repository.Owner, Name: sample.Repository.Name, FullName: sample.Repository.FullName},
		}
		var err error
		if spec.Params, err = p.RenderParams(sample); err != nil {
			add(res.Config, "%s %s, on %s: %v", kind, p.Name, event, err)
			continue
		}
		if _, err := p.ConcurrencyFor(sample); err != nil {
			add(res.Config, "%s %s, on %s: %v", kind, p.Name, event, err)
			continue
		}
		if definition == nil {
			continue
		}
		obj, err := l.Renderer.Render(spec)
		if err != nil {
			add(file, "%v", err)
			continue
		}
		res.Rendered = append(res.Rendered, Rendered{Pipeline: p.Name, Event: event, Object: obj})
	}
}

// Run lints each path and prints the problems to stderr, or, with render, the rendered runs to
// stdout, and returns the exit code.
func (l *Linter) Run(paths []string, render bool, stdout, stderr io.Writer) int {
	if len(paths) == 0 {
		return ExitUsage
	}
	problems := 0
	for _, path := range paths {
		configPath, err := ConfigPath(path)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", path, err)
			problems++
			continue
		}
		res := l.Lint(configPath)
		for _, p := range res.Problems {
			fmt.Fprintln(stderr, p.String())
		}
		for _, n := range res.Notes {
			fmt.Fprintf(stderr, "%s: note: %s\n", configPath, n)
		}
		problems += len(res.Problems)
		if render {
			for _, r := range res.Rendered {
				out, err := yaml.Marshal(r.Object)
				if err != nil {
					fmt.Fprintf(stderr, "%s: pipeline %s: %v\n", configPath, r.Pipeline, err)
					problems++
					continue
				}
				fmt.Fprintf(stdout, "---\n# %s: pipeline %s on %s\n%s", configPath, r.Pipeline, r.Event, out)
			}
		} else if len(res.Problems) == 0 {
			fmt.Fprintf(stdout, "%s: ok (%d pipeline runs rendered)\n", configPath, len(res.Rendered))
		}
	}
	if problems > 0 {
		fmt.Fprintf(stderr, "%d problem(s) found\n", problems)
		return ExitProblems
	}
	return ExitOK
}

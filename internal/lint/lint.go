// Package lint validates a .switchboard.yaml and the PipelineRun files it
// references the way Switchboard would at run time, without GitHub or a cluster.
package lint

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/arikkfir-org/switchboard/internal/repoconfig"
	"github.com/arikkfir-org/switchboard/internal/tekton"
	"github.com/arikkfir-org/switchboard/internal/tmpl"
	"sigs.k8s.io/yaml"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitProblems = 1
	ExitUsage    = 2
)

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
	// Rendered holds each PipelineRun rendered with placeholder values, by
	// "<pipeline> (<event>)", in order.
	Rendered []Rendered
}

// Rendered is one PipelineRun rendered for one event.
type Rendered struct {
	Pipeline string
	Event    string
	Object   map[string]any
}

// ConfigPath returns the configuration file for a path argument: the file
// itself, or .switchboard.yaml in a directory.
func ConfigPath(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return filepath.Join(path, repoconfig.FileName), nil
	}
	return path, nil
}

// Lint validates the configuration at configPath and every PipelineRun file it
// references (resolved relative to the configuration's directory): schema,
// globs, templates rendered with placeholder values for each triggering event,
// the Secret-mount guard, taskChecks and the single-document PipelineRun rule.
func Lint(configPath string) Result {
	res := Result{Config: configPath}
	add := func(file, format string, args ...any) {
		res.Problems = append(res.Problems, Problem{File: file, Message: fmt.Sprintf(format, args...)})
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		add(configPath, "%v", err)
		return res
	}
	cfg, err := repoconfig.Parse(data)
	if err != nil {
		var ce *repoconfig.Error
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
		p := &cfg.Pipelines[i]
		file := filepath.Join(root, filepath.FromSlash(p.PipelineRun))
		raw, err := os.ReadFile(file)
		if err != nil {
			add(configPath, "pipeline %s: pipelineRun: %v", p.Name, err)
			continue
		}
		pr, err := tekton.ParsePipelineRun(raw)
		if err != nil {
			add(file, "%v", err)
			continue
		}
		if p.TaskChecks && len(tekton.TaskNames(pr)) == 0 {
			add(file, "pipeline %s sets taskChecks, which needs the PipelineRun's own spec.pipelineSpec to list its tasks", p.Name)
		}
		for _, event := range p.Events() {
			ctx := tmpl.SampleFor(event)
			ctx.Pipeline = p.Name
			params, err := p.RenderParams(ctx)
			if err != nil {
				add(configPath, "pipeline %s, on %s: %v", p.Name, event, err)
				continue
			}
			if _, err := p.ConcurrencyFor(ctx); err != nil {
				add(configPath, "pipeline %s, on %s: %v", p.Name, event, err)
				continue
			}
			name := tekton.RunName(ctx.Repository.Name, p.Name, ctx.Revision, 1)
			rendered, err := tekton.Render(pr, tekton.RenderInput{
				Namespace:      pr.GetNamespace(),
				Name:           name,
				Params:         params,
				Timeout:        p.TimeoutDuration(),
				TokenWorkspace: p.TokenWorkspace(),
			})
			if err != nil {
				add(file, "%v", err)
				continue
			}
			allowed := ""
			if p.GitHubToken != nil {
				allowed = tekton.TokenSecretName(name)
			}
			if err := tekton.CheckSecrets(rendered, allowed); err != nil {
				add(file, "%v", err)
				continue
			}
			res.Rendered = append(res.Rendered, Rendered{Pipeline: p.Name, Event: event, Object: rendered.Object})
		}
	}
	return res
}

// Run implements "switchboard lint [--render] PATH...": it prints problems to
// stderr (or rendered PipelineRuns to stdout with render) and returns the exit code.
func Run(paths []string, render bool, stdout, stderr io.Writer) int {
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
		res := Lint(configPath)
		for _, p := range res.Problems {
			fmt.Fprintln(stderr, p.String())
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

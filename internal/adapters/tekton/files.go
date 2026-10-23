package tekton

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"octomaton.dev/internal/services/ci"
	"sigs.k8s.io/yaml"
)

// Tekton's references by name (spec.pipelineRef, a task's taskRef, a step's ref) name files beside the
// PipelineRun file: <name>.pipeline.yaml, <name>.task.yaml and <name>.step.yaml, each one Tekton
// object of that kind, at a version Tekton serves it (apiVersions). Octomaton inlines them before it checks the run, so Tekton gets a
// self-contained PipelineRun and every check sees every definition.
const (
	kindPipeline   = "Pipeline"
	kindTask       = "Task"
	kindStepAction = "StepAction"
	// maxFiles bounds the files one PipelineRun may reference: each is a read from the code host.
	maxFiles = 20
)

var fileSuffix = map[string]string{kindPipeline: ".pipeline.yaml", kindTask: ".task.yaml", kindStepAction: ".step.yaml"}

// refName is what a reference's name may be: a file's name, without separators.
var refName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// missingFile is a referenced file that does not exist; it Is ci.ErrNotFound.
type missingFile struct{ msg string }

func (e *missingFile) Error() string        { return e.msg }
func (e *missingFile) Is(target error) bool { return target == ci.ErrNotFound }
func missing(format string, args ...any) error {
	return &missingFile{msg: fmt.Sprintf(format, args...)}
}

// readError is a referenced file the reader could not read, for a reason a retry may get past.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

// expand inlines the Pipeline, Task and StepAction files definition (a PipelineRun file)
// references by name, read with read from beside it, and returns the self-contained definition:
// definition itself when it references none. The problems of a reference or a file are plain
// errors (a file that does not exist Is ci.ErrNotFound); a file that could not be read is a
// *readError.
func expand(ctx context.Context, definition []byte, read ci.FileReader) ([]byte, error) {
	pr, err := parsePipelineRun(definition)
	if err != nil {
		return nil, err
	}
	x := &expander{ctx: ctx, read: read, files: map[string]*unstructured.Unstructured{}}
	if err := x.pipelineRun(pr.Object); err != nil {
		return nil, err
	}
	if !x.changed {
		return definition, nil
	}
	out, err := yaml.Marshal(pr.Object)
	if err != nil {
		return nil, fmt.Errorf("serializing the expanded PipelineRun: %w", err)
	}
	return out, nil
}

type expander struct {
	ctx  context.Context
	read ci.FileReader
	// files holds each file read, by name, so a file referenced twice is read once.
	files   map[string]*unstructured.Unstructured
	changed bool
}

// pipelineRun inlines the pipeline, then the tasks of the pipeline, then the steps of every task.
func (x *expander) pipelineRun(obj map[string]any) error {
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	if ref, ok := spec["pipelineRef"]; ok {
		if _, both := spec["pipelineSpec"]; both {
			return errors.New("spec holds both pipelineRef and pipelineSpec")
		}
		name, err := x.name("spec.pipelineRef", ref)
		if err != nil || name == "" {
			return err
		}
		file, err := x.file("spec.pipelineRef", kindPipeline, name)
		if err != nil {
			return err
		}
		delete(spec, "pipelineRef")
		spec["pipelineSpec"] = file.Object["spec"]
	}
	ps, _ := spec["pipelineSpec"].(map[string]any)
	if ps == nil {
		return nil
	}
	for _, list := range []string{"tasks", "finally"} {
		tasks, _ := ps[list].([]any)
		for i, t := range tasks {
			task, _ := t.(map[string]any)
			if task == nil {
				continue
			}
			where := fmt.Sprintf("task `%s`", taskName(task, fmt.Sprintf("%s[%d]", list, i)))
			if list == "finally" {
				where = "finally " + where
			}
			if err := x.task(where, task); err != nil {
				return err
			}
		}
	}
	return nil
}

func taskName(task map[string]any, fallback string) string {
	if name, _ := task["name"].(string); name != "" {
		return name
	}
	return fallback
}

// task inlines a task's Task file, then the StepAction files of its steps.
func (x *expander) task(where string, task map[string]any) error {
	if ref, ok := task["taskRef"]; ok {
		if _, both := task["taskSpec"]; both {
			return fmt.Errorf("%s holds both taskRef and taskSpec", where)
		}
		name, err := x.name(where+"'s taskRef", ref)
		if err != nil {
			return err
		}
		if name != "" {
			file, err := x.file(where+"'s taskRef", kindTask, name)
			if err != nil {
				return err
			}
			delete(task, "taskRef")
			task["taskSpec"] = embeddedTask(file)
		}
	}
	ts, _ := task["taskSpec"].(map[string]any)
	if ts == nil {
		return nil
	}
	steps, _ := ts["steps"].([]any)
	for i, s := range steps {
		step, _ := s.(map[string]any)
		if step == nil {
			continue
		}
		name, _ := step["name"].(string)
		if name == "" {
			name = fmt.Sprintf("steps[%d]", i)
		}
		if err := x.step(fmt.Sprintf("step `%s` of %s", name, where), step); err != nil {
			return err
		}
	}
	return nil
}

// embeddedTask is a Task file as a pipeline task's taskSpec: its spec, with the labels and
// annotations of its metadata, which Tekton puts on the TaskRun as it would for a referenced Task.
// Each use gets its own copy: the steps of one may be expanded apart from another's.
func embeddedTask(file *unstructured.Unstructured) map[string]any {
	spec, _ := runtime.DeepCopyJSONValue(file.Object["spec"]).(map[string]any)
	meta := map[string]any{}
	for _, k := range []string{"labels", "annotations"} {
		if v, found, _ := unstructured.NestedFieldCopy(file.Object, "metadata", k); found {
			meta[k] = v
		}
	}
	if len(meta) > 0 {
		spec["metadata"] = meta
	}
	return spec
}

// step inlines a step's StepAction file: the action's fields become the step's, with the step's
// params substituted the way Tekton does when it resolves the ref.
func (x *expander) step(where string, step map[string]any) error {
	ref, ok := step["ref"]
	if !ok {
		return nil
	}
	name, err := x.name(where+"'s ref", ref)
	if err != nil || name == "" {
		return err
	}
	file, err := x.file(where+"'s ref", kindStepAction, name)
	if err != nil {
		return err
	}
	action, _ := runtime.DeepCopyJSONValue(file.Object["spec"]).(map[string]any)
	what := fmt.Sprintf("StepAction `%s`", name)
	values, err := stepParams(where, what, step, action)
	if err != nil {
		return err
	}
	delete(step, "ref")
	delete(step, "params")
	delete(action, "params")
	delete(action, "description")
	for _, k := range slices.Sorted(maps.Keys(action)) {
		if _, dup := step[k]; dup {
			return fmt.Errorf("%s sets `%s`, which %s sets too", where, k, what)
		}
		step[k] = substitute(action[k], values)
	}
	return nil
}

// stepParams returns the values of a StepAction's params for a step: the step's params as given
// (what they reference, such as the Task's params, is Tekton's to substitute), then the action's
// defaults, in which references to other params are resolved, as Tekton does. Only string params
// are inlined: Tekton expands array and object ones in ways that depend on where they are used.
func stepParams(where, what string, step, action map[string]any) (map[string]string, error) {
	declared, _ := action["params"].([]any)
	given, _ := step["params"].([]any)
	values := map[string]string{}
	defaults := map[string]bool{}
	for _, p := range given {
		m, _ := p.(map[string]any)
		name, _ := m["name"].(string)
		value, isString := m["value"].(string)
		if name == "" || !isString {
			return nil, fmt.Errorf("%s passes a param that is not a named string: %s inlines string params only", where, what)
		}
		values[name] = value
	}
	names := map[string]bool{}
	for _, p := range declared {
		m, _ := p.(map[string]any)
		name, _ := m["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("%s declares a param without a name", what)
		}
		names[name] = true
		if typ, _ := m["type"].(string); typ != "" && typ != "string" {
			return nil, fmt.Errorf("param `%s` of %s is %s %s; %s inlines string params only", name, what, article(typ), typ, where)
		}
		if _, ok := values[name]; ok {
			continue
		}
		def, has := m["default"]
		if !has {
			return nil, fmt.Errorf("%s gives no value to param `%s` of %s, which has no default", where, name, what)
		}
		s, isString := def.(string)
		if !isString {
			return nil, fmt.Errorf("param `%s` of %s has a default that is not a string; %s inlines string params only", name, what, where)
		}
		values[name], defaults[name] = s, true
	}
	for name := range values {
		if !names[name] {
			return nil, fmt.Errorf("%s passes param `%s`, which %s does not declare", where, name, what)
		}
	}
	resolveDefaults(values, defaults)
	return values, nil
}

// resolveDefaults replaces the params each default references, once, after the defaults it
// references are resolved, so a chain of defaults resolves whatever its order and nothing brought
// in by a replacement (a given value's reference to a Task param) is replaced again. Defaults
// that reference each other in a cycle stay as they are, for Tekton to reject.
func resolveDefaults(values map[string]string, defaults map[string]bool) {
	unresolved := maps.Clone(defaults)
	for progress := true; progress && len(unresolved) > 0; {
		progress = false
		for _, name := range slices.Sorted(maps.Keys(unresolved)) {
			ready := true
			for _, ref := range paramRef.FindAllStringSubmatch(values[name], -1) {
				if dep := ref[1] + ref[2] + ref[3]; dep != name && unresolved[dep] {
					ready = false
				}
			}
			if ready {
				values[name], _ = substitute(values[name], values).(string)
				delete(unresolved, name)
				progress = true
			}
		}
	}
}

func article(word string) string {
	if strings.HasPrefix(word, "a") || strings.HasPrefix(word, "o") {
		return "an"
	}
	return "a"
}

// paramRef matches a reference to a param: $(params.<name>), $(params['<name>']) or
// $(params["<name>"]).
var paramRef = regexp.MustCompile(`\$\(params\.([^.)\[\]]+)\)|\$\(params\['([^']+)'\]\)|\$\(params\["([^"]+)"\]\)`)

// substitute replaces, in every string of v, each reference to a param in values by its value, in
// one pass: what a value brings in is not scanned again.
func substitute(v any, values map[string]string) any {
	switch t := v.(type) {
	case string:
		return paramRef.ReplaceAllStringFunc(t, func(ref string) string {
			m := paramRef.FindStringSubmatch(ref)
			if value, ok := values[m[1]+m[2]+m[3]]; ok {
				return value
			}
			return ref
		})
	case map[string]any:
		for k, child := range t {
			t[k] = substitute(child, values)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = substitute(child, values)
		}
		return t
	}
	return v
}

// name returns the name a reference holds, or "" for a reference that is not by name (a resolver,
// a bundle), which the remote-reference check reports.
func (x *expander) name(where string, ref any) (string, error) {
	m, _ := ref.(map[string]any)
	name, _ := m["name"].(string)
	if m == nil || name == "" {
		return "", nil
	}
	if len(m) > 1 {
		return "", fmt.Errorf("%s names `%s`, so it may hold nothing but `name`: Octomaton resolves it to a file beside the PipelineRun file", where, name)
	}
	if !refName.MatchString(name) {
		return "", fmt.Errorf("%s names `%s`, which is not a file's name (lower-case letters, digits, `-` and `.`)", where, name)
	}
	return name, nil
}

// file reads the file a reference names, once per file, and checks it is one object of kind.
func (x *expander) file(where, kind, name string) (*unstructured.Unstructured, error) {
	f := name + fileSuffix[kind]
	x.changed = true
	if file, ok := x.files[f]; ok {
		return file, nil
	}
	if len(x.files) >= maxFiles {
		return nil, fmt.Errorf("the PipelineRun references more than %d files", maxFiles)
	}
	data, err := x.read(x.ctx, f)
	if errors.Is(err, ci.ErrNotFound) {
		return nil, missing("%s names %s `%s`, whose file `%s` beside the PipelineRun file does not exist", where, kind, name, f)
	}
	if err != nil {
		return nil, &readError{err: fmt.Errorf("reading `%s` beside the PipelineRun file: %w", f, err)}
	}
	file, err := parseDocument(data, kind)
	if err == nil && file.Object["spec"] == nil {
		err = errors.New("it has no spec")
	}
	if err != nil {
		return nil, fmt.Errorf("`%s` beside the PipelineRun file is not a valid %s file: %v", f, kind, err)
	}
	x.files[f] = file
	return file, nil
}

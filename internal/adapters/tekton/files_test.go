package tekton

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// The directory of the PipelineRun file, as expand reads it.
var repoFiles = map[string]string{
	"ci.pipeline.yaml": `
apiVersion: tekton.dev/v1
kind: Pipeline
metadata: {name: ci}
spec:
  params: [{name: revision, type: string}]
  tasks:
    - name: check
      taskRef: {name: check}
    - name: plan
      params: [{name: root, value: github}]
      taskRef: {name: plan}
`,
	"check.task.yaml": `
apiVersion: tekton.dev/v1
kind: Task
metadata:
  name: check
  labels: {kfirs.com/sandbox: "true"}
  annotations: {note: keep}
spec:
  params: [{name: revision}]
  steps:
    - name: clone
      ref: {name: clone}
      params: [{name: revision, value: $(params.revision)}]
    - name: check
      image: busybox
      script: make check
`,
	"plan.task.yaml": `
apiVersion: tekton.dev/v1
kind: Task
spec:
  params: [{name: root}]
  steps:
    - name: plan
      image: terraform
      args: [plan, $(params.root)]
`,
	"clone.step.yaml": `
apiVersion: tekton.dev/v1beta1
kind: StepAction
metadata: {name: clone}
spec:
  description: Clones the repository.
  image: git
  params:
    - {name: revision, type: string}
    - {name: depth, default: "1"}
    - {name: url, type: string, default: "https://example/$(params.revision)"}
  env:
    - {name: REVISION, value: $(params.revision)}
    - {name: DEPTH, value: "$(params['depth'])"}
  args: ["$(params[\"url\"])"]
  results: [{name: sha}]
`,
	"arrays.step.yaml": `
apiVersion: tekton.dev/v1alpha1
kind: StepAction
spec:
  image: busybox
  params: [{name: flags, type: array}]
  args: ["$(params.flags[*])"]
`,
	"bad.task.yaml":  "apiVersion: tekton.dev/v1\nkind: StepAction\nspec: {image: busybox}\n",
	"v1.step.yaml":   "apiVersion: tekton.dev/v1\nkind: StepAction\nspec: {image: busybox}\n",
	"bare.task.yaml": "apiVersion: tekton.dev/v1\nkind: Task\nmetadata: {labels: {a: b}}\n",
	"chain.step.yaml": `
apiVersion: tekton.dev/v1beta1
kind: StepAction
spec:
  image: busybox
  params:
    - {name: a, type: string}
    - {name: b, default: "$(params.a)/b"}
    - {name: c, default: "$(params.b)/c"}
    - {name: d, default: "$(params.c)/d"}
  args: ["$(params.d)", "$(params.a)"]
`,
	"two.task.yaml":  "apiVersion: tekton.dev/v1\nkind: Task\nspec: {steps: []}\n---\napiVersion: tekton.dev/v1\nkind: Task\n",
	"dup.step.yaml":  "apiVersion: tekton.dev/v1beta1\nkind: StepAction\nspec: {image: busybox, workingDir: /w}\n",
	"none.task.yaml": "apiVersion: tekton.dev/v1\nkind: Task\nspec:\n  steps:\n    - name: s\n      ref: {name: nowhere}\n",
}

// reader reads repoFiles, counting the reads, and fails the paths in errs.
type reader struct {
	reads []string
	errs  map[string]error
}

func (r *reader) read(_ context.Context, path string) ([]byte, error) {
	r.reads = append(r.reads, path)
	if err := r.errs[path]; err != nil {
		return nil, err
	}
	content, ok := repoFiles[path]
	if !ok {
		return nil, ci.ErrNotFound
	}
	return []byte(content), nil
}

const runHead = "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec:\n"

func TestExpand(t *testing.T) {
	tests := []struct {
		name string
		spec string
		// want is the expanded spec; "" when the definition is returned as it is.
		want  string
		reads []string
		// err is the error's text, and notFound whether it Is ci.ErrNotFound.
		err      string
		notFound bool
	}{
		{name: "nothing to expand", spec: "  pipelineSpec:\n    tasks: [{name: t, taskSpec: {steps: [{name: s, image: b}]}}]\n"},
		{name: "a resolver is left for the remote-reference check", spec: "  pipelineRef: {resolver: git, params: [{name: url, value: u}]}\n"},
		{name: "a pipeline, its tasks and their steps", spec: "  pipelineRef: {name: ci}\n",
			reads: []string{"ci.pipeline.yaml", "check.task.yaml", "clone.step.yaml", "plan.task.yaml"},
			want: `
  pipelineSpec:
    params: [{name: revision, type: string}]
    tasks:
      - name: check
        taskSpec:
          metadata:
            labels: {kfirs.com/sandbox: "true"}
            annotations: {note: keep}
          params: [{name: revision}]
          steps:
            - name: clone
              image: git
              env:
                - {name: REVISION, value: $(params.revision)}
                - {name: DEPTH, value: "1"}
              args: ["https://example/$(params.revision)"]
              results: [{name: sha}]
            - name: check
              image: busybox
              script: make check
      - name: plan
        params: [{name: root, value: github}]
        taskSpec:
          params: [{name: root}]
          steps:
            - name: plan
              image: terraform
              args: [plan, $(params.root)]
`},
		{name: "a task used twice is read once, and expanded for each use", reads: []string{"plan.task.yaml"},
			spec: "  pipelineSpec:\n    tasks:\n      - {name: a, params: [{name: root, value: a}], taskRef: {name: plan}}\n    finally:\n      - {name: b, taskRef: {name: plan}}\n",
			want: `
  pipelineSpec:
    tasks:
      - name: a
        params: [{name: root, value: a}]
        taskSpec: {params: [{name: root}], steps: [{name: plan, image: terraform, args: [plan, $(params.root)]}]}
    finally:
      - name: b
        taskSpec: {params: [{name: root}], steps: [{name: plan, image: terraform, args: [plan, $(params.root)]}]}
`},
		{name: "a step of an inline task", reads: []string{"clone.step.yaml"},
			spec: "  pipelineSpec:\n    tasks:\n      - name: t\n        taskSpec:\n          steps:\n            - {name: c, ref: {name: clone}, params: [{name: revision, value: abc}, {name: depth, value: \"5\"}], onError: continue}\n",
			want: `
  pipelineSpec:
    tasks:
      - name: t
        taskSpec:
          steps:
            - name: c
              onError: continue
              image: git
              env: [{name: REVISION, value: abc}, {name: DEPTH, value: "5"}]
              args: ["https://example/abc"]
              results: [{name: sha}]
`},
		{name: "a provided value is left for Tekton, defaults resolve through it", reads: []string{"chain.step.yaml"},
			spec: "  pipelineSpec:\n    tasks:\n      - name: t\n        taskSpec:\n          steps:\n            - {name: s, ref: {name: chain}, params: [{name: a, value: $(params.b)}]}\n",
			want: `
  pipelineSpec:
    tasks:
      - name: t
        taskSpec:
          steps:
            - {name: s, image: busybox, args: ["$(params.b)/b/c/d", "$(params.b)"]}
`},
		{name: "a StepAction at v1, which Tekton does not serve", spec: "  pipelineSpec:\n    tasks: [{name: t, taskSpec: {steps: [{name: s, ref: {name: v1}}]}}]\n", reads: []string{"v1.step.yaml"},
			err: "`v1.step.yaml` beside the PipelineRun file is not a valid StepAction file: apiVersion must be \"tekton.dev/v1beta1\" or \"tekton.dev/v1alpha1\" (got \"tekton.dev/v1\")"},
		{name: "a Task file without a spec", spec: "  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: bare}}]\n", reads: []string{"bare.task.yaml"},
			err: "`bare.task.yaml` beside the PipelineRun file is not a valid Task file: it has no spec"},
		{name: "pipelineRef and pipelineSpec", spec: "  pipelineRef: {name: ci}\n  pipelineSpec: {tasks: []}\n", err: "spec holds both pipelineRef and pipelineSpec"},
		{name: "taskRef and taskSpec", spec: "  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: check}, taskSpec: {steps: []}}]\n", err: "task `t` holds both taskRef and taskSpec"},
		{name: "a reference with more than a name", spec: "  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: check, kind: Task}}]\n",
			err: "task `t`'s taskRef names `check`, so it may hold nothing but `name`"},
		{name: "a name that is not a file's", spec: "  pipelineRef: {name: ../ci}\n", err: "spec.pipelineRef names `../ci`, which is not a file's name"},
		{name: "a missing file", spec: "  pipelineSpec:\n    finally: [{name: t, taskRef: {name: gone}}]\n", reads: []string{"gone.task.yaml"},
			err: "finally task `t`'s taskRef names Task `gone`, whose file `gone.task.yaml` beside the PipelineRun file does not exist", notFound: true},
		{name: "a file of another kind", spec: "  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: bad}}]\n", reads: []string{"bad.task.yaml"},
			err: "`bad.task.yaml` beside the PipelineRun file is not a valid Task file: kind must be \"Task\""},
		{name: "a file of two documents", spec: "  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: two}}]\n", reads: []string{"two.task.yaml"},
			err: "`two.task.yaml` beside the PipelineRun file is not a valid Task file: expected exactly one YAML document, found 2"},
		{name: "a step whose action's file is missing", spec: "  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: none}}]\n", reads: []string{"none.task.yaml", "nowhere.step.yaml"},
			err: "step `s` of task `t`'s ref names StepAction `nowhere`, whose file `nowhere.step.yaml` beside the PipelineRun file does not exist", notFound: true},
		{name: "an array param", spec: "  pipelineSpec:\n    tasks: [{name: t, taskSpec: {steps: [{name: s, ref: {name: arrays}, params: [{name: flags, value: [a]}]}]}}]\n", reads: []string{"arrays.step.yaml"},
			err: "step `s` of task `t` passes a param that is not a named string: StepAction `arrays` inlines string params only"},
		{name: "an array param with a default", spec: "  pipelineSpec:\n    tasks: [{name: t, taskSpec: {steps: [{name: s, ref: {name: arrays}}]}}]\n", reads: []string{"arrays.step.yaml"},
			err: "param `flags` of StepAction `arrays` is an array; step `s` of task `t` inlines string params only"},
		{name: "a param the action does not declare", spec: "  pipelineSpec:\n    tasks: [{name: t, taskSpec: {steps: [{name: s, ref: {name: clone}, params: [{name: revision, value: a}, {name: other, value: b}]}]}}]\n", reads: []string{"clone.step.yaml"},
			err: "step `s` of task `t` passes param `other`, which StepAction `clone` does not declare"},
		{name: "a param without a value", spec: "  pipelineSpec:\n    tasks: [{name: t, taskSpec: {steps: [{name: s, ref: {name: clone}}]}}]\n", reads: []string{"clone.step.yaml"},
			err: "step `s` of task `t` gives no value to param `revision` of StepAction `clone`, which has no default"},
		{name: "a field the step and its action both set", spec: "  pipelineSpec:\n    tasks: [{name: t, taskSpec: {steps: [{name: s, ref: {name: dup}, workingDir: /x}]}}]\n", reads: []string{"dup.step.yaml"},
			err: "step `s` of task `t` sets `workingDir`, which StepAction `dup` sets too"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &reader{}
			definition := []byte(runHead + tt.spec)
			got, err := expand(context.Background(), definition, r.read)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("Expand = %v, want an error containing %q", err, tt.err)
				}
				if errors.Is(err, ci.ErrNotFound) != tt.notFound {
					t.Fatalf("Expand = %v; Is ErrNotFound = %v, want %v", err, !tt.notFound, tt.notFound)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if &got[0] != &definition[0] {
					t.Fatalf("a definition without references must be returned as it is")
				}
			} else if want, gotObj := object(t, runHead+tt.want), object(t, string(got)); !reflect.DeepEqual(want, gotObj) {
				t.Fatalf("Expand =\n%s\nwant\n%s", got, runHead+tt.want)
			}
			if !reflect.DeepEqual(r.reads, tt.reads) {
				t.Fatalf("reads = %v, want %v", r.reads, tt.reads)
			}
		})
	}
}

func TestExpandBoundsTheFiles(t *testing.T) {
	var tasks []string
	for i := 0; i <= maxFiles; i++ {
		tasks = append(tasks, fmt.Sprintf("{name: t%d, taskRef: {name: t%d}}", i, i))
	}
	spec := "  pipelineSpec:\n    tasks: [" + strings.Join(tasks, ", ") + "]\n"
	read := func(context.Context, string) ([]byte, error) {
		return []byte("apiVersion: tekton.dev/v1\nkind: Task\nspec: {steps: []}\n"), nil
	}
	_, err := expand(context.Background(), []byte(runHead+spec), read)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("references more than %d files", maxFiles)) {
		t.Fatalf("Expand = %v, want the bound", err)
	}
}

func TestExpandReadErrors(t *testing.T) {
	boom := errors.New("boom")
	r := &reader{errs: map[string]error{"check.task.yaml": boom}}
	_, err := expand(context.Background(), []byte(runHead+"  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: check}}]\n"), r.read)
	var re *readError
	if !errors.As(err, &re) || !errors.Is(err, boom) || !strings.Contains(err.Error(), "reading `check.task.yaml` beside the PipelineRun file: boom") {
		t.Fatalf("Expand = %v, want a readError wrapping boom", err)
	}
}

func TestRunnerExpand(t *testing.T) {
	spec := func(s string) ci.RunSpec { return ci.RunSpec{Definition: []byte(runHead + s), Path: "ci.yaml"} }
	r := &reader{errs: map[string]error{"plan.task.yaml": errors.New("boom")}}
	runner := &Runner{}

	out, err := runner.Expand(context.Background(), spec("  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: check}}]\n"), r.read)
	if err != nil || !strings.Contains(string(out), "image: git") {
		t.Fatalf("Expand = %s, %v", out, err)
	}

	_, err = runner.Expand(context.Background(), spec("  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: gone}}]\n"), r.read)
	if !errors.Is(err, ci.ErrNotFound) || err.Error() != "`ci.yaml`: task `t`'s taskRef names Task `gone`, whose file `gone.task.yaml` beside the PipelineRun file does not exist" {
		t.Fatalf("a missing file = %v", err)
	}

	_, err = runner.Expand(context.Background(), spec("  pipelineSpec:\n    tasks: [{name: t, taskRef: {name: plan}}]\n"), r.read)
	var refused *ci.Refusal
	if errors.As(err, &refused) || err == nil || err.Error() != "`ci.yaml`: reading `plan.task.yaml` beside the PipelineRun file: boom" {
		t.Fatalf("a read error = %v, want a plain error", err)
	}

	_, err = runner.Expand(context.Background(), spec("  pipelineRef: {name: ci}\n  pipelineSpec: {}\n"), r.read)
	if !errors.As(err, &refused) || refused.Reason != "`ci.yaml`: spec holds both pipelineRef and pipelineSpec." {
		t.Fatalf("a reference's problem = %v, want a refusal", err)
	}

	_, err = runner.Expand(context.Background(), ci.RunSpec{Definition: []byte("kind: Task\n"), Path: "x.yaml"}, r.read)
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "apiVersion must be") {
		t.Fatalf("an invalid PipelineRun = %v, want a refusal", err)
	}
}

func TestRendererExpand(t *testing.T) {
	r := &reader{}
	out, err := Renderer{}.Expand(context.Background(), ci.RunSpec{Definition: []byte(runHead + "  pipelineRef: {name: ci}\n"), Path: "ci.yaml"}, r.read)
	if err != nil || !strings.Contains(string(out), "script: make check") {
		t.Fatalf("Expand = %s, %v", out, err)
	}
	if err := (Renderer{}).CheckDefinition(ci.RunSpec{Definition: out, TaskReports: true}); err != nil {
		t.Fatalf("the expanded run must pass the checks: %v", err)
	}
	_, err = Renderer{}.Expand(context.Background(), ci.RunSpec{Definition: []byte(runHead + "  pipelineRef: {name: gone}\n"), Path: "ci.yaml"}, r.read)
	if !errors.Is(err, ci.ErrNotFound) {
		t.Fatalf("a missing file = %v", err)
	}
}

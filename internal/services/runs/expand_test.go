package runs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// expandsCheckTask stands in for the runner's Expand: it appends the file `.tekton/check.task.yaml`
// of the definition's repository to the definition.
func expandsCheckTask(ctx context.Context, spec ci.RunSpec, read ci.FileReader) ([]byte, error) {
	task, err := read(ctx, ".tekton/check.task.yaml")
	if errors.Is(err, ci.ErrNotFound) {
		return nil, fmt.Errorf("`%s`: task `check` names a file that is %w", spec.Path, err)
	}
	if err != nil {
		return nil, fmt.Errorf("`%s`: reading `.tekton/check.task.yaml`: %w", spec.Path, err)
	}
	return append(append([]byte{}, spec.Definition...), task...), nil
}

func TestStartExpandsTheDefinition(t *testing.T) {
	h := newHarness(t)
	h.files(sha1, ciConfig, ciRun)
	h.host.SetFile(repo, sha1, ".tekton/check.task.yaml", "kind: Task\n")
	h.runner.Expander = expandsCheckTask
	h.evaluate(branchPR(sha1), EvalOptions{})
	run := h.run("demo-ci-1111111-1")
	if got := string(h.runner.Spec(run.ID).Definition); got != ciRun+"kind: Task\n" {
		t.Fatalf("the run was created from %q, want the expanded definition", got)
	}
	// The file is read where the definition was: at the pull request's head.
	want := repo.FullName + "@" + sha1 + ":.tekton/check.task.yaml"
	if reads := h.host.Reads(); !strings.Contains(strings.Join(reads, "\n"), want) {
		t.Fatalf("reads = %q, want %q", reads, want)
	}
}

func TestStartRefusesWhatExpandRefuses(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(h *harness)
		want     string
		wantDone bool
	}{
		{name: "a missing file", want: "task `check` names a file that is not found at `1111111`."},
		{name: "a reference the runner refuses", want: "task `check` holds both taskRef and taskSpec.",
			setup: func(h *harness) {
				h.runner.Expander = func(context.Context, ci.RunSpec, ci.FileReader) ([]byte, error) {
					return nil, &ci.Refusal{Title: "Could not start the pipeline", Reason: "`.tekton/ci.yaml`: task `check` holds both taskRef and taskSpec."}
				}
			}},
		{name: "a file that would not read", want: "Could not read a file the pipelineRun file `.tekton/ci.yaml` references: `.tekton/ci.yaml`: reading `.tekton/check.task.yaml`: GitHub is down",
			setup: func(h *harness) { h.host.FailFile(repo, sha1, ".tekton/check.task.yaml", errors.New("GitHub is down")) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, ciConfig, ciRun)
			h.runner.Expander = expandsCheckTask
			if tt.setup != nil {
				tt.setup(h)
			}
			h.evaluate(branchPR(sha1), EvalOptions{})
			if len(h.runner.Runs()) != 0 {
				t.Fatalf("no run may be created")
			}
			r := h.onlyReport("ci")
			if r.Conclusion != ci.Failure || r.Title != "Could not start the pipeline" {
				t.Fatalf("report = %+v", r)
			}
			mustContain(t, r.Summary, tt.want)
		})
	}
}

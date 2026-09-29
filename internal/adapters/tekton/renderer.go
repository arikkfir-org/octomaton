package tekton

import (
	"fmt"

	"octomaton.dev/internal/services/ci"
)

// Renderer renders PipelineRuns without a cluster, for octomaton-lint: in the definition's own
// namespace, without Octomaton's bookkeeping, and not held.
type Renderer struct{}

// CheckDefinition parses a pipeline definition and checks it fits the pipeline's settings.
func (Renderer) CheckDefinition(spec ci.RunSpec) error {
	pr, err := parsePipelineRun(spec.Definition)
	if err != nil {
		return err
	}
	if spec.TaskReports && len(taskNames(pr)) == 0 {
		return fmt.Errorf("pipeline %s sets taskChecks, which needs the PipelineRun's own spec.pipelineSpec to list its tasks", spec.Trigger.Pipeline)
	}
	return nil
}

// Render renders spec's first attempt the way Create would, and applies the Secret-mount guard.
func (Renderer) Render(spec ci.RunSpec) (map[string]any, error) {
	src, err := parsePipelineRun(spec.Definition)
	if err != nil {
		return nil, err
	}
	t := spec.Trigger
	name := runName(t.Repository.Name, t.Pipeline, t.Revision, 1)
	in := renderInput{Namespace: src.GetNamespace(), Name: name, Params: spec.Params, Timeout: spec.Timeout}
	allowed := ""
	if spec.Token != nil {
		in.TokenWorkspace, allowed = spec.Token.Workspace, tokenSecretName(name)
	}
	pr, err := render(src, in)
	if err != nil {
		return nil, err
	}
	if err := checkSecrets(pr, allowed); err != nil {
		return nil, err
	}
	return pr.Object, nil
}

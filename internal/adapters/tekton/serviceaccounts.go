package tekton

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"octomaton.dev/internal/services/ci"
)

// annotationBranches, on a ServiceAccount in a tenant namespace, lists the branch globs (comma-separated,
// as in on.push.branches) whose runs may use it. The cluster's owner sets it, not Octomaton: it is the
// one setting that lives outside .octomaton.yaml, on the identity a repository can't change.
const annotationBranches = "octomaton.dev/branches"

// serviceAccountNames lists the ServiceAccounts a PipelineRun names for its tasks: the template's and
// each task's override, without duplicates.
func serviceAccountNames(pr *unstructured.Unstructured) ([]string, error) {
	var names []string
	add := func(where string, v any) error {
		if v == nil {
			return nil
		}
		name, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", where)
		}
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
		return nil
	}
	spec, _ := pr.Object["spec"].(map[string]any)
	if template, ok := spec["taskRunTemplate"].(map[string]any); ok {
		if err := add("spec.taskRunTemplate.serviceAccountName", template["serviceAccountName"]); err != nil {
			return nil, err
		}
	}
	taskRunSpecs, _ := spec["taskRunSpecs"].([]any)
	for i, item := range taskRunSpecs {
		if s, ok := item.(map[string]any); ok {
			if err := add(fmt.Sprintf("spec.taskRunSpecs[%d].serviceAccountName", i), s["serviceAccountName"]); err != nil {
				return nil, err
			}
		}
	}
	return names, nil
}

// branchGlobs parses an octomaton.dev/branches value. An empty value lists no glob, so no branch
// may use the ServiceAccount.
func branchGlobs(value string) ([]string, error) {
	var globs []string
	for g := range strings.SplitSeq(value, ",") {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		if !doublestar.ValidatePattern(g) {
			return nil, fmt.Errorf("%q is not a valid glob", g)
		}
		globs = append(globs, g)
	}
	return globs, nil
}

// checkServiceAccounts refuses a run that names a ServiceAccount whose octomaton.dev/branches
// annotation has no glob matching the run's branch. A run without a branch (a tag push) matches
// none. A ServiceAccount that doesn't exist is left to Tekton, which fails the run.
func checkServiceAccounts(ctx context.Context, c *kubeClient, ns, path string, pr *unstructured.Unstructured, branch string) error {
	names, err := serviceAccountNames(pr)
	if err != nil {
		return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("`%s`: %v.", path, err)}
	}
	for _, name := range names {
		sa, err := c.ServiceAccount(ctx, ns, name)
		if err != nil {
			refused := refusal("Could not read ServiceAccount `%s/%s`, which the PipelineRun names: %v", ns, name, err)
			refused.Cause = err
			return refused
		}
		if sa == nil {
			continue
		}
		value, restricted := sa.Annotations[annotationBranches]
		if !restricted {
			continue
		}
		globs, err := branchGlobs(value)
		if err != nil {
			return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("ServiceAccount `%s/%s` has an invalid `%s` annotation: %v, so no run may use it.", ns, name, annotationBranches, err)}
		}
		if branch != "" && slices.ContainsFunc(globs, func(g string) bool { ok, _ := doublestar.Match(g, branch); return ok }) {
			continue
		}
		if len(globs) == 0 {
			return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("The PipelineRun names ServiceAccount `%s`, whose `%s` annotation lists no branch, so no run may use it.", name, annotationBranches)}
		}
		on := fmt.Sprintf("this run is on branch %q", branch)
		if branch == "" {
			on = "this run has no branch"
		}
		return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("The PipelineRun names ServiceAccount `%s`, which only runs on branches matching %s may use (its `%s` annotation), and %s.", name, quoteAll(globs), annotationBranches, on)}
	}
	return nil
}

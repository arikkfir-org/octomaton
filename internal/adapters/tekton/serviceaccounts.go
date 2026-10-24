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

// annotationTags is the same for tag pushes: the tag globs (as in on.push.tags) whose runs may use the
// ServiceAccount. A release pipeline that publishes from version tags names a ServiceAccount carrying it.
const annotationTags = "octomaton.dev/tags"

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

// refGlobs parses an octomaton.dev/branches or octomaton.dev/tags value. An empty value lists no
// glob, so no branch (or tag) may use the ServiceAccount.
func refGlobs(value string) ([]string, error) {
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

// checkServiceAccounts refuses a run that names a ServiceAccount whose octomaton.dev/branches or
// octomaton.dev/tags annotation has no glob matching the run's branch or tag. A ServiceAccount with
// either annotation is restricted: a branch's run must match a branch glob and a tag push a tag glob,
// so one annotated with branches only refuses every tag push, and the other way round. A
// ServiceAccount that doesn't exist is left to Tekton, which fails the run.
func checkServiceAccounts(ctx context.Context, c *kubeClient, ns, path string, pr *unstructured.Unstructured, branch, tag string) error {
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
		branches, onBranches := sa.Annotations[annotationBranches]
		tags, onTags := sa.Annotations[annotationTags]
		if !onBranches && !onTags {
			continue
		}
		branchGlobs, err := refGlobs(branches)
		if err != nil {
			return invalidAnnotation(ns, name, annotationBranches, err)
		}
		tagGlobs, err := refGlobs(tags)
		if err != nil {
			return invalidAnnotation(ns, name, annotationTags, err)
		}
		if matchesAny(branchGlobs, branch) || matchesAny(tagGlobs, tag) {
			continue
		}
		if len(branchGlobs) == 0 && len(tagGlobs) == 0 {
			return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("The PipelineRun names ServiceAccount `%s`, whose `%s` %s no %s, so no run may use it.", name, restriction(onBranches, onTags), lists(onBranches && onTags), refKinds(onBranches, onTags))}
		}
		return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("The PipelineRun names ServiceAccount `%s`, which only runs %s may use (its `%s` annotation%s), and %s.", name, allowed(branchGlobs, tagGlobs), restriction(onBranches, onTags), plural(onBranches && onTags), on(branch, tag))}
	}
	return nil
}

func invalidAnnotation(ns, name, annotation string, err error) error {
	return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("ServiceAccount `%s/%s` has an invalid `%s` annotation: %v, so no run may use it.", ns, name, annotation, err)}
}

// matchesAny reports whether ref (a branch or tag; "" when the run has none) matches one of globs.
func matchesAny(globs []string, ref string) bool {
	return ref != "" && slices.ContainsFunc(globs, func(g string) bool { ok, _ := doublestar.Match(g, ref); return ok })
}

// restriction names the annotations a ServiceAccount carries, for a refusal.
func restriction(onBranches, onTags bool) string {
	switch {
	case onBranches && onTags:
		return annotationBranches + "` and `" + annotationTags
	case onTags:
		return annotationTags
	default:
		return annotationBranches
	}
}

// refKinds names what the annotations a ServiceAccount carries list.
func refKinds(onBranches, onTags bool) string {
	switch {
	case onBranches && onTags:
		return "branch or tag"
	case onTags:
		return "tag"
	default:
		return "branch"
	}
}

func lists(both bool) string {
	if both {
		return "annotations list"
	}
	return "annotation lists"
}

func plural(both bool) string {
	if both {
		return "s"
	}
	return ""
}

// allowed describes the runs a ServiceAccount's globs let through.
func allowed(branchGlobs, tagGlobs []string) string {
	var parts []string
	if len(branchGlobs) > 0 {
		parts = append(parts, "on branches matching "+quoteAll(branchGlobs))
	}
	if len(tagGlobs) > 0 {
		parts = append(parts, "of tags matching "+quoteAll(tagGlobs))
	}
	return strings.Join(parts, " or ")
}

// on describes the run being refused.
func on(branch, tag string) string {
	switch {
	case tag != "":
		return fmt.Sprintf("this run is of tag %q", tag)
	case branch != "":
		return fmt.Sprintf("this run is on branch %q", branch)
	default:
		return "this run has no branch"
	}
}

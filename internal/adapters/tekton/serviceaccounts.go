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
// ServiceAccount. A pipeline that runs on version tags names a ServiceAccount carrying it.
const annotationTags = "octomaton.dev/tags"

// annotationOrganizationPipelines lists the organization pipelines (comma-separated names) whose runs
// may use the ServiceAccount whatever their branch or tag: what such a run executes is read at a
// default branch, so a branch can only bring it data. A repository pipeline never matches it.
const annotationOrganizationPipelines = "octomaton.dev/organization-pipelines"

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

// listNames parses an octomaton.dev/organization-pipelines value: the names it lists, without the
// spaces around them.
func listNames(value string) []string {
	var names []string
	for n := range strings.SplitSeq(value, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// checkServiceAccounts refuses a run that names a ServiceAccount restricted by its annotations unless
// one of them admits the run: octomaton.dev/branches by a glob matching the run's branch,
// octomaton.dev/tags by one matching its tag, octomaton.dev/organization-pipelines by naming the
// run's pipeline, when that is an organization pipeline. A ServiceAccount with any of them is
// restricted, so one annotated with branches only refuses every tag push, and the other way round.
// A ServiceAccount that doesn't exist is left to Tekton, which fails the run.
func checkServiceAccounts(ctx context.Context, c *kubeClient, ns string, spec ci.RunSpec, pr *unstructured.Unstructured) error {
	names, err := serviceAccountNames(pr)
	if err != nil {
		return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("`%s`: %v.", spec.Path, err)}
	}
	t := spec.Trigger
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
		restricted := carried(sa.Annotations)
		if len(restricted) == 0 {
			continue
		}
		branchGlobs, err := refGlobs(sa.Annotations[annotationBranches])
		if err != nil {
			return invalidAnnotation(ns, name, annotationBranches, err)
		}
		tagGlobs, err := refGlobs(sa.Annotations[annotationTags])
		if err != nil {
			return invalidAnnotation(ns, name, annotationTags, err)
		}
		orgPipelines := listNames(sa.Annotations[annotationOrganizationPipelines])
		if matchesAny(branchGlobs, t.Branch) || matchesAny(tagGlobs, t.Tag) || spec.Organization && slices.Contains(orgPipelines, t.Pipeline) {
			continue
		}
		annotations, kinds := describe(restricted)
		if len(branchGlobs) == 0 && len(tagGlobs) == 0 && len(orgPipelines) == 0 {
			return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("The PipelineRun names ServiceAccount `%s`, whose %s %s no %s, so no run may use it.", name, annotations, plural(len(restricted), "annotation lists", "annotations list"), kinds)}
		}
		_, listsPipelines := sa.Annotations[annotationOrganizationPipelines]
		return &ci.Refusal{Title: "Refused", Reason: fmt.Sprintf("The PipelineRun names ServiceAccount `%s`, which only runs %s may use (its %s %s), and %s.", name, allowed(branchGlobs, tagGlobs, orgPipelines), annotations, plural(len(restricted), "annotation", "annotations"), on(spec, listsPipelines))}
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

// restriction is an annotation that restricts a ServiceAccount, and what it lists, for a refusal.
type restriction struct{ annotation, lists string }

var restrictions = []restriction{
	{annotationBranches, "branch"}, {annotationTags, "tag"}, {annotationOrganizationPipelines, "organization pipeline"},
}

// carried returns the restrictions among a ServiceAccount's annotations, in order.
func carried(annotations map[string]string) []restriction {
	var out []restriction
	for _, r := range restrictions {
		if _, ok := annotations[r.annotation]; ok {
			out = append(out, r)
		}
	}
	return out
}

// describe names restrictions' annotations, in code, and what they list: "`a` and `b`", "branch or
// tag".
func describe(restricted []restriction) (annotations, kinds string) {
	var names, lists []string
	for _, r := range restricted {
		names, lists = append(names, "`"+r.annotation+"`"), append(lists, r.lists)
	}
	return joinList(names, "and"), joinList(lists, "or")
}

// joinList joins items as a sentence does: "a", "a and b", "a, b and c".
func joinList(items []string, conjunction string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conjunction + " " + items[len(items)-1]
}

func plural(n int, one, more string) string {
	if n > 1 {
		return more
	}
	return one
}

// on describes the run being refused: its branch or tag and, when the ServiceAccount lists
// organization pipelines, its pipeline.
func on(spec ci.RunSpec, withPipeline bool) string {
	t := spec.Trigger
	run := "this run"
	if withPipeline {
		kind := "repository pipeline"
		if spec.Organization {
			kind = "organization pipeline"
		}
		run = fmt.Sprintf("this run of %s `%s`", kind, t.Pipeline)
	}
	switch {
	case t.Tag != "":
		return fmt.Sprintf("%s is of tag %q", run, t.Tag)
	case t.Branch != "":
		return fmt.Sprintf("%s is on branch %q", run, t.Branch)
	default:
		return run + " has no branch"
	}
}

// allowed describes the runs a ServiceAccount's globs and organization pipelines let through.
func allowed(branchGlobs, tagGlobs, orgPipelines []string) string {
	var parts []string
	if len(branchGlobs) > 0 {
		parts = append(parts, "on branches matching "+quoteAll(branchGlobs))
	}
	if len(tagGlobs) > 0 {
		parts = append(parts, "of tags matching "+quoteAll(tagGlobs))
	}
	if len(orgPipelines) > 0 {
		parts = append(parts, "of organization "+plural(len(orgPipelines), "pipeline ", "pipelines ")+codeList(orgPipelines))
	}
	return strings.Join(parts, " or ")
}

// codeList lists items in code: "`a`, `b`".
func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "`" + item + "`"
	}
	return strings.Join(quoted, ", ")
}

package tekton

import (
	"fmt"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/util/validation"
	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
	"octomaton.dev/internal/system/config"
)

// Namespaces maps repositories to the namespaces their PipelineRuns are created in: an override,
// or the sanitized output of a template over the repository. OCTOMATON_NAMESPACE_TEMPLATE and
// OCTOMATON_NAMESPACE_OVERRIDES configure it.
type Namespaces struct {
	template  *template.Template
	overrides map[string]string
}

// NewNamespaces compiles and validates a namespace template and its overrides; the error lists every
// problem as a *config.Error.
func NewNamespaces(text string, overrides map[string]string) (*Namespaces, error) {
	n := &Namespaces{overrides: make(map[string]string, len(overrides))}
	var problems []string
	if strings.TrimSpace(text) == "" {
		problems = append(problems, "OCTOMATON_NAMESPACE_TEMPLATE is required")
	} else if t, err := pipelines.ParseTemplate("OCTOMATON_NAMESPACE_TEMPLATE", text); err != nil {
		problems = append(problems, fmt.Sprintf("OCTOMATON_NAMESPACE_TEMPLATE: %v", err))
	} else {
		n.template = t
		sample := pipelines.Sample().Repository
		if out, err := pipelines.ExecuteTemplate(t, pipelines.NamespaceContext{Repository: sample}); err != nil {
			problems = append(problems, fmt.Sprintf("OCTOMATON_NAMESPACE_TEMPLATE: %v", err))
		} else if SanitizeDNSLabel(out) == "" {
			problems = append(problems, fmt.Sprintf("OCTOMATON_NAMESPACE_TEMPLATE: renders %q for repository %s, which is not usable as a namespace", out, sample.FullName))
		}
	}
	for repo, ns := range overrides {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			problems = append(problems, fmt.Sprintf("OCTOMATON_NAMESPACE_OVERRIDES: key %q must be \"owner/name\"", repo))
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			problems = append(problems, fmt.Sprintf("OCTOMATON_NAMESPACE_OVERRIDES: %s: %q is not a valid namespace name: %s", repo, ns, strings.Join(errs, "; ")))
			continue
		}
		n.overrides[strings.ToLower(repo)] = ns
	}
	if len(problems) > 0 {
		return nil, &config.Error{Problems: problems}
	}
	return n, nil
}

// Resolve returns the namespace of a repository's runs: its override, or the rendered and
// sanitized template.
func (n *Namespaces) Resolve(repo ci.Repository) (string, error) {
	if ns, ok := n.overrides[strings.ToLower(repo.FullName)]; ok {
		return ns, nil
	}
	out, err := pipelines.ExecuteTemplate(n.template, pipelines.NamespaceContext{Repository: pipelines.RepositoryOf(repo)})
	if err != nil {
		return "", fmt.Errorf("rendering the namespace template: %w", err)
	}
	ns := SanitizeDNSLabel(out)
	if ns == "" {
		return "", fmt.Errorf("the namespace template rendered %q for %s, which does not sanitize to a valid namespace name", out, repo.FullName)
	}
	return ns, nil
}

// SanitizeDNSLabel turns s into a DNS-1123 label: it lowercases s, strips leading dots, replaces
// every run of characters outside [a-z0-9-] with a single "-", trims leading and trailing "-" and
// truncates to 63 characters.
func SanitizeDNSLabel(s string) string {
	s = strings.TrimLeft(strings.ToLower(s), ".")
	var b strings.Builder
	inRun := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			inRun = false
			continue
		}
		if !inRun {
			b.WriteByte('-')
			inRun = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > validation.DNS1123LabelMaxLength {
		out = strings.TrimRight(out[:validation.DNS1123LabelMaxLength], "-")
	}
	return out
}

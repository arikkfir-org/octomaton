package config

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/arikkfir-org/octomatron/internal/tmpl"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Namespaces maps repositories to the Kubernetes namespaces their PipelineRuns run in.
type Namespaces struct {
	// Template is a Go template over .Repository whose output is sanitized into a DNS label.
	Template string `yaml:"template"`
	// Overrides maps "owner/name" to a namespace, bypassing the template.
	Overrides map[string]string `yaml:"overrides"`

	compiled  *template.Template
	overrides map[string]string
}

func (n *Namespaces) init() []string {
	var problems []string
	if strings.TrimSpace(n.Template) == "" {
		problems = append(problems, "namespaces.template is required")
	} else if t, err := tmpl.Parse("namespaces.template", n.Template); err != nil {
		problems = append(problems, fmt.Sprintf("namespaces.template: %v", err))
	} else {
		n.compiled = t
		sample := tmpl.Sample().Repository
		if out, err := tmpl.Execute(t, tmpl.NamespaceContext{Repository: sample}); err != nil {
			problems = append(problems, fmt.Sprintf("namespaces.template: %v", err))
		} else if SanitizeDNSLabel(out) == "" {
			problems = append(problems, fmt.Sprintf("namespaces.template: renders %q for repository %s, which is not usable as a namespace", out, sample.FullName))
		}
	}
	n.overrides = make(map[string]string, len(n.Overrides))
	for repo, ns := range n.Overrides {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			problems = append(problems, fmt.Sprintf("namespaces.overrides: key %q must be \"owner/name\"", repo))
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			problems = append(problems, fmt.Sprintf("namespaces.overrides[%s]: %q is not a valid namespace name: %s", repo, ns, strings.Join(errs, "; ")))
			continue
		}
		n.overrides[strings.ToLower(repo)] = ns
	}
	return problems
}

// Resolve returns the namespace for repo: an explicit override, or the rendered
// and sanitized template.
func (n *Namespaces) Resolve(repo tmpl.Repository) (string, error) {
	if ns, ok := n.overrides[strings.ToLower(repo.FullName)]; ok {
		return ns, nil
	}
	if n.compiled == nil {
		return "", fmt.Errorf("namespace template is not configured")
	}
	out, err := tmpl.Execute(n.compiled, tmpl.NamespaceContext{Repository: repo})
	if err != nil {
		return "", fmt.Errorf("rendering namespace template: %w", err)
	}
	ns := SanitizeDNSLabel(out)
	if ns == "" {
		return "", fmt.Errorf("namespace template rendered %q for %s, which does not sanitize to a valid namespace name", out, repo.FullName)
	}
	return ns, nil
}

// SanitizeDNSLabel turns s into a DNS-1123 label: it lowercases s, strips leading
// dots, replaces every run of characters outside [a-z0-9-] with a single "-",
// trims leading and trailing "-" and truncates to 63 characters.
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

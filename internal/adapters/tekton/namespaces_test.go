package tekton

import (
	"errors"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/system/config"
)

func TestNewNamespacesProblems(t *testing.T) {
	tests := []struct {
		name      string
		template  string
		overrides map[string]string
		want      string
	}{
		{name: "no template", template: " ", want: "OCTOMATON_NAMESPACE_TEMPLATE is required"},
		{name: "bad template", template: "{{ .Nope", want: "OCTOMATON_NAMESPACE_TEMPLATE"},
		{name: "unknown field", template: "{{ .Repository.Nope }}", want: "can't evaluate field Nope"},
		{name: "unusable template", template: "___", want: "not usable as a namespace"},
		{name: "override key without owner", template: "ci", overrides: map[string]string{"docs": "ci-docs"}, want: `key "docs" must be`},
		{name: "override with bad namespace", template: "ci", overrides: map[string]string{"a/b": "Not_OK"}, want: "not a valid namespace name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewNamespaces(tt.template, tt.overrides)
			var ce *config.Error
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewNamespaces error = %v, want a *config.Error mentioning %q", err, tt.want)
			}
		})
	}
}

func TestResolveNamespace(t *testing.T) {
	n, err := NewNamespaces("ci-{{ .Repository.Name }}", map[string]string{"arikkfir-org/.github": "ci-github"})
	if err != nil {
		t.Fatalf("NewNamespaces: %v", err)
	}
	tests := []struct {
		repo ci.Repository
		want string
	}{
		{ci.Repository{Owner: "arikkfir-org", Name: "octomaton", FullName: "arikkfir-org/octomaton"}, "ci-octomaton"},
		{ci.Repository{Owner: "arikkfir-org", Name: ".github", FullName: "arikkfir-org/.github"}, "ci-github"},
		{ci.Repository{Owner: "Arikkfir-Org", Name: ".GitHub", FullName: "Arikkfir-Org/.GitHub"}, "ci-github"},
		{ci.Repository{Owner: "arikkfir-org", Name: "My_Repo", FullName: "arikkfir-org/My_Repo"}, "ci-my-repo"},
	}
	for _, tt := range tests {
		got, err := n.Resolve(tt.repo)
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%s) = %q, %v; want %q", tt.repo.FullName, got, err, tt.want)
		}
	}
	unusable, err := NewNamespaces("{{ .Repository.Owner }}", nil)
	if err != nil {
		t.Fatalf("NewNamespaces: %v", err)
	}
	if _, err := unusable.Resolve(ci.Repository{Owner: "___", FullName: "___/x"}); err == nil {
		t.Fatalf("Resolve must fail when the template renders no usable namespace")
	}
}

func TestSanitizeDNSLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ci-octomaton", "ci-octomaton"},
		{"CI-Docs", "ci-docs"},
		{".github", "github"},
		{"...dots", "dots"},
		{"ci-.github", "ci--github"},
		{"ci-my_repo.name", "ci-my-repo-name"},
		{"ci-a__..b", "ci-a-b"},
		{"-leading-and-trailing-", "leading-and-trailing"},
		{"ci-" + strings.Repeat("x", 80), "ci-" + strings.Repeat("x", 60)},
		{"ci-" + strings.Repeat("x", 59) + "-y", "ci-" + strings.Repeat("x", 59)},
		{"ünïcode", "n-code"},
		{"___", ""},
	}
	for _, tt := range tests {
		got := sanitizeDNSLabel(tt.in)
		if got != tt.want || len(got) > 63 {
			t.Errorf("SanitizeDNSLabel(%q) = %q, want %q (at most 63 characters)", tt.in, got, tt.want)
		}
	}
}

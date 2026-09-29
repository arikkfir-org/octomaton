// Package architecture checks the layering of Octomaton's packages. Services hold the CI logic in
// their own terms. Adapters implement the services' ports over GitHub, Tekton, Kubernetes and HTTP.
// System configures the process. Dependencies point inward, and cmd wires the layers together (see
// the architecture design in the hub docs).
package architecture

import (
	"bufio"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "octomaton.dev"

// rule says what a package may import.
type rule struct {
	// pkg is a package path relative to the module, or, ending in "/", a prefix of such paths.
	pkg string
	// stdlibOnly allows the standard library only.
	stdlibOnly bool
	// allow lists the module's packages the package may import, with the packages under them
	// (entries ending in "/" are prefixes); nil allows the whole module. A package may always import
	// the packages under it.
	allow []string
	// deny lists prefixes of the external packages the package must not import.
	deny []string
}

// providerSDKs are the SDKs of the technologies the adapters drive.
var providerSDKs = []string{"k8s.io/", "github.com/google/go-github/", "github.com/bradleyfalzon/ghinstallation/", "github.com/tektoncd/"}

// rules are tried in order; the first whose pkg matches applies.
var rules = []rule{
	{pkg: "internal/services/ci", stdlibOnly: true},
	{pkg: "internal/services/", allow: []string{"internal/services/", "internal/system/metrics"}, deny: providerSDKs},
	{pkg: "internal/adapters/", allow: []string{"internal/services/ci", "internal/services/pipelines", "internal/adapters/kube", "internal/system/"}},
	{pkg: "internal/system/", allow: []string{"internal/system/"}},
	{pkg: "internal/architecture", stdlibOnly: true},
	{pkg: "internal/e2e"},
	{pkg: "cmd/"},
}

func matches(pattern, pkg string) bool {
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(pkg, pattern)
	}
	return pkg == pattern
}

func ruleFor(pkg string) (rule, bool) {
	for _, r := range rules {
		if matches(r.pkg, pkg) {
			return r, true
		}
	}
	return rule{}, false
}

func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// check returns why pkg may not import path, or "" when it may.
func (r rule) check(pkg, path string) string {
	internal, inModule := strings.CutPrefix(path, module+"/")
	switch {
	case isStdlib(path):
		return ""
	case r.stdlibOnly:
		return "only the standard library is allowed"
	case inModule:
		if r.allow == nil || strings.HasPrefix(internal, pkg+"/") || slices.ContainsFunc(r.allow, func(a string) bool {
			return matches(a, internal) || strings.HasPrefix(internal, a+"/")
		}) {
			return ""
		}
		return "allowed from the module: " + strings.Join(r.allow, ", ")
	default:
		for _, d := range r.deny {
			if strings.HasPrefix(path, d) {
				return "provider SDKs are for adapters"
			}
		}
		return ""
	}
}

func TestRules(t *testing.T) {
	tests := []struct {
		pkg, path string
		want      bool
	}{
		{pkg: "internal/services/ci", path: "time", want: true},
		{pkg: "internal/services/ci", path: "octomaton.dev/internal/system/metrics"},
		{pkg: "internal/services/ci", path: "github.com/robfig/cron/v3"},
		{pkg: "internal/services/ci/citest", path: "octomaton.dev/internal/services/ci", want: true},
		{pkg: "internal/services/runs", path: "octomaton.dev/internal/services/pipelines", want: true},
		{pkg: "internal/services/runs", path: "octomaton.dev/internal/system/metrics", want: true},
		{pkg: "internal/services/runs", path: "octomaton.dev/internal/system/config"},
		{pkg: "internal/services/runs", path: "octomaton.dev/internal/adapters/tekton"},
		{pkg: "internal/services/runs", path: "k8s.io/client-go/dynamic"},
		{pkg: "internal/services/reports", path: "github.com/google/go-github/v92/github"},
		{pkg: "internal/services/lint", path: "sigs.k8s.io/yaml", want: true},
		{pkg: "internal/adapters/tekton", path: "octomaton.dev/internal/adapters/kube", want: true},
		{pkg: "internal/adapters/tekton", path: "octomaton.dev/internal/services/pipelines", want: true},
		{pkg: "internal/adapters/tekton", path: "k8s.io/client-go/dynamic", want: true},
		{pkg: "internal/adapters/tekton", path: "octomaton.dev/internal/adapters/github"},
		{pkg: "internal/adapters/github", path: "octomaton.dev/internal/adapters/github/githubtest", want: true},
		{pkg: "internal/adapters/github", path: "octomaton.dev/internal/services/runs"},
		{pkg: "internal/system/config", path: "octomaton.dev/internal/services/ci"},
		{pkg: "internal/system/config", path: "octomaton.dev/internal/adapters/kube"},
		{pkg: "internal/system/metrics/metricstest", path: "octomaton.dev/internal/system/metrics", want: true},
		{pkg: "cmd/octomaton", path: "octomaton.dev/internal/adapters/tekton", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.pkg+" imports "+tt.path, func(t *testing.T) {
			r, ok := ruleFor(tt.pkg)
			if !ok {
				t.Fatalf("no rule for %s", tt.pkg)
			}
			if why := r.check(tt.pkg, tt.path); (why == "") != tt.want {
				t.Fatalf("check = %q, want allowed: %v", why, tt.want)
			}
		})
	}
}

// TestLayers checks every import of every package of the module, tests included.
func TestLayers(t *testing.T) {
	imports := moduleImports(t, moduleRoot(t))
	if len(imports) < 10 {
		t.Fatalf("found %d packages; is the walk broken?", len(imports))
	}
	for pkg, paths := range imports {
		r, ok := ruleFor(pkg)
		if !ok {
			t.Errorf("%s: no layering rule; add one to rules", pkg)
			continue
		}
		for _, path := range paths {
			if why := r.check(pkg, path); why != "" {
				t.Errorf("%s must not import %s: %s", pkg, path, why)
			}
		}
	}
}

// moduleRoot finds the directory of the module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if f, err := os.Open(filepath.Join(dir, "go.mod")); err == nil {
			defer f.Close()
			line, _ := bufio.NewReader(f).ReadString('\n')
			if strings.TrimSpace(line) != "module "+module {
				t.Fatalf("%s/go.mod declares %q, want module %s", dir, strings.TrimSpace(line), module)
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// moduleImports maps each package directory under root, relative to it, to what its files import.
// It skips what the go command ignores: testdata, and directories starting with "." or "_".
func moduleImports(t *testing.T, root string) map[string][]string {
	t.Helper()
	imports := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel)
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if !slices.Contains(imports[pkg], p) {
				imports[pkg] = append(imports[pkg], p)
			}
		}
		if _, ok := imports[pkg]; !ok {
			imports[pkg] = nil
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return imports
}

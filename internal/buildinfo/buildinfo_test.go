package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestModuleVersion(t *testing.T) {
	tests := []struct {
		name, set, module, want string
	}{
		{name: "linker flag wins", set: "v1.0.0", module: "v2.0.0", want: "v1.0.0"},
		{name: "go install", set: "dev", module: "v0.1.0", want: "v0.1.0"},
		{name: "vcs stamped build", set: "dev", module: "v0.1.1-0.20260929001555-5ac9b26eefa8+dirty", want: "v0.1.1-0.20260929001555-5ac9b26eefa8+dirty"},
		{name: "no vcs data", set: "dev", module: "(devel)", want: "dev"},
		{name: "no module version", set: "dev", module: "", want: "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &debug.BuildInfo{Main: debug.Module{Path: "octomaton.dev", Version: tt.module}}
			if got := moduleVersion(tt.set, info); got != tt.want {
				t.Fatalf("moduleVersion(%q, %q) = %q, want %q", tt.set, tt.module, got, tt.want)
			}
		})
	}
}

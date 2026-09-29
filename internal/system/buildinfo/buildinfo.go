// Package buildinfo reports the version Octomaton was built as.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

// version is set at build time with -ldflags "-X octomaton.dev/internal/buildinfo.version=...".
var version = "dev"

// Version returns the version this binary was built as. See moduleVersion for builds that did not
// set it through the linker.
var Version = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	return moduleVersion(version, info)
})

// moduleVersion returns set, unless the linker left the default: then the main module's version
// from the build info, which "go install octomaton.dev/cmd/...@v1.2.3" records as v1.2.3 and a
// build in a git checkout records as its tag or pseudo-version ("(devel)" without VCS data).
func moduleVersion(set string, info *debug.BuildInfo) string {
	if set != "dev" || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return set
	}
	return info.Main.Version
}

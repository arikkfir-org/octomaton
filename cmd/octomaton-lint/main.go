// Command octomaton-lint validates .octomaton.yaml files and the PipelineRun files they reference,
// as Octomaton would read them.
//
// Usage:
//
//	octomaton-lint [-render] PATH...
//	octomaton-lint -version
//
// PATH is a .octomaton.yaml file or the directory holding it. With -render, it prints every
// PipelineRun rendered with placeholder values, as a YAML stream.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"octomaton.dev/internal/adapters/github"
	"octomaton.dev/internal/adapters/tekton"
	"octomaton.dev/internal/services/lint"
	"octomaton.dev/internal/system/buildinfo"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("octomaton-lint", flag.ContinueOnError)
	flags.SetOutput(stderr)
	render := flags.Bool("render", false, "print every PipelineRun rendered with placeholder values, as a YAML stream")
	version := flags.Bool("version", false, "print the version and exit")
	flags.Usage = func() {
		fmt.Fprint(stderr, "Usage: octomaton-lint [-render] PATH...\n       octomaton-lint -version\n\n"+
			"PATH is a .octomaton.yaml file or the directory holding it.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return lint.ExitUsage
	}
	if *version {
		fmt.Fprintln(stdout, buildinfo.Version())
		return 0
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return lint.ExitUsage
	}
	linter := &lint.Linter{Renderer: tekton.Renderer{}, CheckPermissions: github.CheckPermissions}
	return linter.Run(flags.Args(), *render, stdout, stderr)
}

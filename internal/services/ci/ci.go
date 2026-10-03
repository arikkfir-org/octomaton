// Package ci is Octomaton's vocabulary: the terms services and adapters share (repositories,
// triggers, events, runs, reports) and the ports adapters implement (CodeHost for GitHub, Runner for
// Tekton). Services speak only these terms; adapters translate them to their technology. It
// imports nothing but the standard library.
package ci

import "errors"

var (
	// ErrNotFound is returned when a file, pull request, branch or run does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists is returned by Runner.Create when the attempt it creates exists already.
	ErrExists = errors.New("already exists")
)

// Refusal is a run Octomaton will not start, with a reason people can act on (Markdown).
type Refusal struct {
	// Title heads the report that shows the refusal.
	Title  string
	Reason string
	// Cause is the failed call that refused the run (a file or the cluster that would not answer),
	// which a retry may get past; nil when the run itself is refused.
	Cause error
}

func (r *Refusal) Error() string { return r.Title + ": " + r.Reason }

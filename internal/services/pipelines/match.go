package pipelines

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"octomaton.dev/internal/services/ci"
)

// Event is the part of an event that decides which pipelines it triggers.
type Event struct {
	// Name is push, pull_request or merge_group.
	Name string
	// Action is the pull_request action.
	Action string
	// Branch is the pushed branch (push) or the base branch (pull_request, merge_group).
	Branch string
	// Tag is the pushed tag (push).
	Tag string
	// Draft is set for draft pull requests.
	Draft bool
}

// PathFilter restricts a matched pipeline to events that change relevant files.
type PathFilter struct {
	Paths       []string
	PathsIgnore []string
}

// Active reports whether the filter restricts anything.
func (f PathFilter) Active() bool {
	return len(f.Paths) > 0 || len(f.PathsIgnore) > 0
}

// Matches reports whether any of the changed files is relevant: it matches one of
// Paths (when set) and none of PathsIgnore. Callers must not apply the filter to
// an incomplete file list (fail open).
func (f PathFilter) Matches(files []string) bool {
	if !f.Active() {
		return true
	}
	for _, file := range files {
		if len(f.Paths) > 0 && !anyGlob(f.Paths, file) {
			continue
		}
		if anyGlob(f.PathsIgnore, file) {
			continue
		}
		return true
	}
	return false
}

// Match reports whether the pipeline is triggered by ev, and the path filter that
// applies to it.
//
// Branch and tag semantics follow GitHub Actions: for push, omitting both
// branches and tags matches every push; setting only branches ignores tag pushes
// and setting only tags ignores branch pushes. For pull_request and merge_group,
// branches filters the base branch and omitting it matches every base branch.
func (p *Pipeline) Match(ev Event) (PathFilter, bool) {
	switch ev.Name {
	case ci.EventPush:
		t := p.On.Push
		if t == nil {
			return PathFilter{}, false
		}
		var ok bool
		switch {
		case ev.Tag != "":
			if len(t.Tags) == 0 {
				ok = len(t.Branches) == 0
			} else {
				ok = anyGlob(t.Tags, ev.Tag)
			}
		case ev.Branch != "":
			if len(t.Branches) == 0 {
				ok = len(t.Tags) == 0
			} else {
				ok = anyGlob(t.Branches, ev.Branch)
			}
		}
		return PathFilter{Paths: t.Paths, PathsIgnore: t.PathsIgnore}, ok

	case ci.EventPullRequest:
		t := p.On.PullRequest
		if t == nil {
			return PathFilter{}, false
		}
		types := t.Types
		if len(types) == 0 {
			types = DefaultPullRequestTypes
		}
		if !slices.Contains(types, ev.Action) {
			return PathFilter{}, false
		}
		if ev.Draft && t.Drafts != nil && !*t.Drafts {
			return PathFilter{}, false
		}
		if len(t.Branches) > 0 && !anyGlob(t.Branches, ev.Branch) {
			return PathFilter{}, false
		}
		return PathFilter{Paths: t.Paths, PathsIgnore: t.PathsIgnore}, true

	case ci.EventMergeGroup:
		t := p.On.MergeGroup
		if t == nil {
			return PathFilter{}, false
		}
		if len(t.Branches) > 0 && !anyGlob(t.Branches, ev.Branch) {
			return PathFilter{}, false
		}
		return PathFilter{Paths: t.Paths, PathsIgnore: t.PathsIgnore}, true
	}
	return PathFilter{}, false
}

// IsCommand reports whether line (a comment's first line) is this pipeline's comment command.
func (p *Pipeline) IsCommand(line string) bool {
	t := p.On.Comment
	return t != nil && t.pattern != nil && t.pattern.MatchString(line)
}

// MatchComment reports whether the pipeline runs for a comment whose first line
// is line, on a pull request into baseBranch. matched is false when the comment
// is not this pipeline's command; declined explains why a matching command may
// not run on this pull request.
func (p *Pipeline) MatchComment(line, baseBranch string) (matched bool, declined string) {
	if !p.IsCommand(line) {
		return false, ""
	}
	if t := p.On.Comment; len(t.Branches) > 0 && !anyGlob(t.Branches, baseBranch) {
		return true, fmt.Sprintf("%s runs only on pull requests into %s, and this one is into %s", p.Name, strings.Join(t.Branches, " or "), baseBranch)
	}
	return true, ""
}

func anyGlob(patterns []string, name string) bool {
	for _, p := range patterns {
		// Patterns are validated when the configuration is parsed.
		if ok, _ := doublestar.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Events lists the events that trigger the pipeline, in a fixed order.
func (p *Pipeline) Events() []string {
	var out []string
	if p.On.Push != nil {
		out = append(out, ci.EventPush)
	}
	if p.On.PullRequest != nil {
		out = append(out, ci.EventPullRequest)
	}
	if p.On.MergeGroup != nil {
		out = append(out, ci.EventMergeGroup)
	}
	if p.On.Comment != nil {
		out = append(out, ci.EventComment)
	}
	if len(p.On.Schedule) > 0 {
		out = append(out, ci.EventSchedule)
	}
	return out
}

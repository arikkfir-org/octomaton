package pipelines

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// The template context is the data Go templates see: pipeline params and concurrency groups in
// .octomaton.yaml, and the namespace template of the server configuration.

// Repository describes the repository an event belongs to.
type Repository struct {
	Owner         string
	Name          string
	FullName      string
	CloneURL      string
	HTMLURL       string
	DefaultBranch string
	Private       bool
}

// Push holds push-specific values; it is nil for other events.
type Push struct {
	Before string
	After  string
}

// PullRequest holds pull_request-specific values; it is nil for other events.
// HeadRef and BaseRef are branch names, as GitHub sends them.
type PullRequest struct {
	Number  int
	HeadRef string
	HeadSHA string
	BaseRef string
	BaseSHA string
}

// MergeGroup holds merge_group-specific values; it is nil for other events.
// HeadRef and BaseRef are full refs (refs/heads/...), as GitHub sends them.
type MergeGroup struct {
	HeadRef string
	HeadSHA string
	BaseRef string
	BaseSHA string
}

// Comment holds the comment command that started a run; it is nil for other events.
type Comment struct {
	ID        int64
	Author    string
	Command   string // the first word of the comment's first line, e.g. "/deploy"
	Arguments string // the rest of the comment's first line, trimmed
}

// ReviewRequest holds the review request that started a run; it is nil for other events.
type ReviewRequest struct {
	Reviewer string // login of the user the review was requested from
}

// Schedule holds the schedule that started a run; it is nil for other events.
type Schedule struct {
	Cron string // the cron expression, as configured
	Slot string // the scheduled time the run was fired for, RFC 3339 in UTC
}

// TemplateContext is the data passed to pipeline param and concurrency group templates.
type TemplateContext struct {
	Event       string // push, pull_request, merge_group, comment, review_request or schedule
	Action      string // event action, empty for push and schedule
	Repository  Repository
	Revision    string // the commit SHA under test
	Ref         string // full ref under test
	Branch      string // branch under test (push: pushed branch; pull_request, comment: head branch; merge_group: merge group branch; schedule: default branch)
	Tag         string // pushed tag (push events only)
	Sender      string // login of the user that triggered the event
	Pipeline    string // pipeline name from .octomaton.yaml
	Push        *Push
	PullRequest *PullRequest
	MergeGroup  *MergeGroup
	Comment     *Comment
	// ReviewRequest is set for review_request events.
	ReviewRequest *ReviewRequest
	Schedule      *Schedule
}

// NamespaceContext is the data passed to the namespace template.
type NamespaceContext struct {
	Repository Repository
}

// ParseTemplate parses text as a template in which references to missing map keys are errors.
func ParseTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Parse(text)
}

// ExecuteTemplate renders t with data, adding hints to errors caused by event-specific
// objects that are nil for the current event.
func ExecuteTemplate(t *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", explain(err)
	}
	return buf.String(), nil
}

var nilHints = []struct{ typeName, hint string }{
	{"*pipelines.Push", ".Push is only set for push events"},
	{"*pipelines.PullRequest", ".PullRequest is only set for pull_request, comment and review_request events"},
	{"*pipelines.MergeGroup", ".MergeGroup is only set for merge_group events"},
	{"*pipelines.Comment", ".Comment is only set for comment events"},
	{"*pipelines.ReviewRequest", ".ReviewRequest is only set for review_request events"},
	{"*pipelines.Schedule", ".Schedule is only set for schedule events"},
}

func explain(err error) error {
	msg := err.Error()
	for _, h := range nilHints {
		if strings.Contains(msg, "nil pointer evaluating "+h.typeName) {
			return fmt.Errorf("%w (%s)", err, h.hint)
		}
	}
	return err
}

// Sample returns a context in which every field, including every event-specific
// object, is populated. It is used to validate templates when configuration is loaded.
func Sample() TemplateContext {
	return TemplateContext{
		Event:  "pull_request",
		Action: "synchronize",
		Repository: Repository{
			Owner:         "octo-org",
			Name:          "octo-repo",
			FullName:      "octo-org/octo-repo",
			CloneURL:      "https://github.com/octo-org/octo-repo.git",
			HTMLURL:       "https://github.com/octo-org/octo-repo",
			DefaultBranch: "main",
			Private:       false,
		},
		Revision:      "0123456789abcdef0123456789abcdef01234567",
		Ref:           "refs/pull/1/head",
		Branch:        "feature",
		Tag:           "v1.0.0",
		Sender:        "octocat",
		Pipeline:      "ci",
		Push:          &Push{Before: "1111111111111111111111111111111111111111", After: "0123456789abcdef0123456789abcdef01234567"},
		PullRequest:   &PullRequest{Number: 1, HeadRef: "feature", HeadSHA: "0123456789abcdef0123456789abcdef01234567", BaseRef: "main", BaseSHA: "2222222222222222222222222222222222222222"},
		MergeGroup:    &MergeGroup{HeadRef: "refs/heads/gh-readonly-queue/main/pr-1-2222222222222222222222222222222222222222", HeadSHA: "0123456789abcdef0123456789abcdef01234567", BaseRef: "refs/heads/main", BaseSHA: "2222222222222222222222222222222222222222"},
		Comment:       &Comment{ID: 1, Author: "octocat", Command: "/deploy", Arguments: "staging"},
		ReviewRequest: &ReviewRequest{Reviewer: "octo-reviewer"},
		Schedule:      &Schedule{Cron: "0 3 * * *", Slot: "2026-01-01T03:00:00Z"},
	}
}

// SampleFor returns a placeholder context for one event: only that event's
// objects are set, as they would be at run time.
func SampleFor(event string) TemplateContext {
	c := Sample()
	c.Event = event
	c.Push, c.PullRequest, c.MergeGroup, c.Comment, c.ReviewRequest, c.Schedule = nil, nil, nil, nil, nil, nil
	full := Sample()
	switch event {
	case "push":
		c.Action, c.Ref, c.Branch, c.Tag = "", "refs/heads/main", "main", ""
		c.Push = full.Push
	case "pull_request":
		c.Action, c.Tag = "synchronize", ""
		c.PullRequest = full.PullRequest
	case "merge_group":
		c.Action, c.Tag = "checks_requested", ""
		c.Ref = full.MergeGroup.HeadRef
		c.Branch = "gh-readonly-queue/main/pr-1-2222222222222222222222222222222222222222"
		c.MergeGroup = full.MergeGroup
	case "comment":
		c.Action, c.Tag = "created", ""
		c.PullRequest, c.Comment = full.PullRequest, full.Comment
	case "review_request":
		c.Action, c.Tag = "review_requested", ""
		c.PullRequest, c.ReviewRequest = full.PullRequest, full.ReviewRequest
	case "schedule":
		c.Action, c.Ref, c.Branch, c.Tag, c.Sender = "", "refs/heads/main", "main", "", ""
		c.Schedule = full.Schedule
	}
	return c
}

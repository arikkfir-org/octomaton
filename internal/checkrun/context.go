// Package checkrun holds what Octomaton stores on GitHub check runs: the trigger
// context (serialized into a hidden marker so a check can be re-run after its
// PipelineRun is gone), output size limits and Tekton Dashboard links.
package checkrun

import (
	"fmt"
	"strings"

	"octomaton.dev/internal/tmpl"
)

// ConfigCheckName is the name of the check run that reports problems which are not
// specific to one pipeline, such as an invalid .octomaton.yaml.
const ConfigCheckName = "octomaton"

// Event names Octomaton triggers pipelines for.
const (
	EventPush        = "push"
	EventPullRequest = "pull_request"
	EventMergeGroup  = "merge_group"
	EventComment     = "comment"
	EventSchedule    = "schedule"
)

// ContextVersion is the version of the serialized Context format.
const ContextVersion = 1

// Repository identifies the repository an event belongs to.
type Repository struct {
	ID            int64  `json:"id"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	FullName      string `json:"fullName"`
	CloneURL      string `json:"cloneURL,omitempty"`
	HTMLURL       string `json:"htmlURL,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	Private       bool   `json:"private,omitempty"`
}

// Push holds the push-specific part of a Context.
type Push struct {
	Before  string `json:"before"`
	After   string `json:"after"`
	Created bool   `json:"created,omitempty"`
}

// PullRequest holds the pull_request-specific part of a Context.
type PullRequest struct {
	Number            int    `json:"number"`
	HeadRef           string `json:"headRef"`
	HeadSHA           string `json:"headSHA"`
	BaseRef           string `json:"baseRef"`
	BaseSHA           string `json:"baseSHA"`
	HeadRepo          string `json:"headRepo,omitempty"`
	Author            string `json:"author,omitempty"`
	AuthorAssociation string `json:"authorAssociation,omitempty"`
	HTMLURL           string `json:"htmlURL,omitempty"`
}

// MergeGroup holds the merge_group-specific part of a Context.
type MergeGroup struct {
	HeadRef string `json:"headRef"`
	HeadSHA string `json:"headSHA"`
	BaseRef string `json:"baseRef"`
	BaseSHA string `json:"baseSHA"`
}

// Comment is the comment command that started a run.
type Comment struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	Command   string `json:"command"`
	Arguments string `json:"arguments,omitempty"`
}

// Schedule is the schedule slot that started a run.
type Schedule struct {
	Cron string `json:"cron"`
	Slot string `json:"slot"`
}

// Context is everything needed to evaluate (or re-evaluate) an event for a
// repository: it is stored on every check run and PipelineRun Octomaton creates.
type Context struct {
	Version        int          `json:"v"`
	Event          string       `json:"event"`
	Action         string       `json:"action,omitempty"`
	DeliveryID     string       `json:"deliveryID,omitempty"`
	InstallationID int64        `json:"installationID"`
	Repository     Repository   `json:"repository"`
	Revision       string       `json:"revision"`
	Ref            string       `json:"ref,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	Tag            string       `json:"tag,omitempty"`
	Sender         string       `json:"sender,omitempty"`
	Pipeline       string       `json:"pipeline,omitempty"`
	Push           *Push        `json:"push,omitempty"`
	PullRequest    *PullRequest `json:"pullRequest,omitempty"`
	MergeGroup     *MergeGroup  `json:"mergeGroup,omitempty"`
	Comment        *Comment     `json:"comment,omitempty"`
	Schedule       *Schedule    `json:"schedule,omitempty"`
	// ConfigRef is the ref .octomaton.yaml and PipelineRun files are read at:
	// the default branch for comment commands, the revision otherwise.
	ConfigRef  string `json:"configRef,omitempty"`
	ApprovedBy string `json:"approvedBy,omitempty"`
	RerunBy    string `json:"rerunBy,omitempty"`
}

// ConfigAt is the ref configuration is read at.
func (c Context) ConfigAt() string {
	if c.ConfigRef != "" {
		return c.ConfigRef
	}
	return c.Revision
}

// Head identifies the line of work a run belongs to: runs of the same pipeline
// at the same commit and head are the same run (a redelivery, or a push and a
// pull request event for the same commit on the same branch).
func (c Context) Head() string {
	if c.Tag != "" {
		return "tag:" + c.Tag
	}
	return c.Branch
}

// TemplateRepository returns the repository as exposed to templates.
func (c Context) TemplateRepository() tmpl.Repository {
	return tmpl.Repository{
		Owner:         c.Repository.Owner,
		Name:          c.Repository.Name,
		FullName:      c.Repository.FullName,
		CloneURL:      c.Repository.CloneURL,
		HTMLURL:       c.Repository.HTMLURL,
		DefaultBranch: c.Repository.DefaultBranch,
		Private:       c.Repository.Private,
	}
}

// Template returns the data exposed to pipeline param templates.
func (c Context) Template() tmpl.Context {
	t := tmpl.Context{
		Event:      c.Event,
		Action:     c.Action,
		Repository: c.TemplateRepository(),
		Revision:   c.Revision,
		Ref:        c.Ref,
		Branch:     c.Branch,
		Tag:        c.Tag,
		Sender:     c.Sender,
		Pipeline:   c.Pipeline,
	}
	if c.Push != nil {
		t.Push = &tmpl.Push{Before: c.Push.Before, After: c.Push.After}
	}
	if c.PullRequest != nil {
		p := c.PullRequest
		t.PullRequest = &tmpl.PullRequest{Number: p.Number, HeadRef: p.HeadRef, HeadSHA: p.HeadSHA, BaseRef: p.BaseRef, BaseSHA: p.BaseSHA}
	}
	if c.MergeGroup != nil {
		m := c.MergeGroup
		t.MergeGroup = &tmpl.MergeGroup{HeadRef: m.HeadRef, HeadSHA: m.HeadSHA, BaseRef: m.BaseRef, BaseSHA: m.BaseSHA}
	}
	if c.Comment != nil {
		t.Comment = &tmpl.Comment{ID: c.Comment.ID, Author: c.Comment.Author, Command: c.Comment.Command, Arguments: c.Comment.Arguments}
	}
	if c.Schedule != nil {
		t.Schedule = &tmpl.Schedule{Cron: c.Schedule.Cron, Slot: c.Schedule.Slot}
	}
	return t
}

// ShortSHA abbreviates a commit SHA for display.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Describe returns a one-line, Markdown description of what triggered the run.
func (c Context) Describe() string {
	var b strings.Builder
	switch {
	case c.Comment != nil && c.PullRequest != nil:
		fmt.Fprintf(&b, "`%s` on pull request #%d", c.Comment.Command, c.PullRequest.Number)
	case c.Schedule != nil:
		fmt.Fprintf(&b, "Schedule `%s` (slot %s) on `%s`", c.Schedule.Cron, c.Schedule.Slot, c.Branch)
	case c.PullRequest != nil:
		fmt.Fprintf(&b, "Pull request #%d (`%s` → `%s`)", c.PullRequest.Number, c.PullRequest.HeadRef, c.PullRequest.BaseRef)
		if c.Action != "" {
			fmt.Fprintf(&b, ", %s", c.Action)
		}
	case c.MergeGroup != nil:
		fmt.Fprintf(&b, "Merge queue into `%s`", strings.TrimPrefix(c.MergeGroup.BaseRef, "refs/heads/"))
	case c.Tag != "":
		fmt.Fprintf(&b, "Push of tag `%s`", c.Tag)
	case c.Event != "":
		fmt.Fprintf(&b, "Push to `%s`", c.Branch)
	}
	if c.Revision != "" {
		fmt.Fprintf(&b, " at `%s`", ShortSHA(c.Revision))
	}
	if c.Sender != "" {
		fmt.Fprintf(&b, " by @%s", c.Sender)
	}
	if c.ApprovedBy != "" {
		fmt.Fprintf(&b, "; approved by @%s", c.ApprovedBy)
	}
	if c.RerunBy != "" && c.RerunBy != c.ApprovedBy {
		fmt.Fprintf(&b, "; re-run by @%s", c.RerunBy)
	}
	return b.String()
}

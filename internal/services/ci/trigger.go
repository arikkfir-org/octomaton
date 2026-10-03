package ci

import (
	"fmt"
	"strings"
)

// Events Octomaton runs pipelines for.
const (
	EventPush          = "push"
	EventPullRequest   = "pull_request"
	EventMergeGroup    = "merge_group"
	EventComment       = "comment"
	EventReviewRequest = "review_request"
	EventSchedule      = "schedule"
)

// TriggerVersion is the version of the serialized Trigger. Runs and reports store their trigger
// serialized, so a report can be re-run after its run is gone.
const TriggerVersion = 1

// ConfigReportName names the report of problems that concern no single pipeline, such as an
// invalid .octomaton.yaml. Re-running it evaluates the event again.
const ConfigReportName = "octomaton"

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

// Push is the push-specific part of a Trigger.
type Push struct {
	Before  string `json:"before"`
	After   string `json:"after"`
	Created bool   `json:"created,omitempty"`
}

// PullRequest is the pull-request-specific part of a Trigger. HeadRef and BaseRef are branch names.
type PullRequest struct {
	Number   int    `json:"number"`
	HeadRef  string `json:"headRef"`
	HeadSHA  string `json:"headSHA"`
	BaseRef  string `json:"baseRef"`
	BaseSHA  string `json:"baseSHA"`
	HeadRepo string `json:"headRepo,omitempty"`
	Author   string `json:"author,omitempty"`
	HTMLURL  string `json:"htmlURL,omitempty"`
}

// FromFork reports whether the pull request's head branch lives outside repository (the full name
// of its base repository): in a fork, or in a repository that no longer exists. Octomaton never acts
// on those.
func (pr *PullRequest) FromFork(repository string) bool {
	return pr != nil && !strings.EqualFold(pr.HeadRepo, repository)
}

// MergeGroup is the merge-queue-specific part of a Trigger. HeadRef and BaseRef are full refs.
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

// ReviewRequest is the review request that started a run.
type ReviewRequest struct {
	// Reviewer is the login of the user the review was requested from.
	Reviewer string `json:"reviewer"`
	// Pending is set when new commits on the pull request run the request again: the review was still
	// requested when the head moved.
	Pending bool `json:"pending,omitempty"`
}

// Schedule is the schedule slot that started a run.
type Schedule struct {
	Cron string `json:"cron"`
	// Slot is the time the run was fired for, RFC 3339 in UTC.
	Slot string `json:"slot"`
}

// Trigger is why pipelines are considered: everything needed to evaluate an event for a repository,
// or to evaluate it again. Every run and report stores it.
type Trigger struct {
	Version        int            `json:"v"`
	Event          string         `json:"event"`
	Action         string         `json:"action,omitempty"`
	DeliveryID     string         `json:"deliveryID,omitempty"`
	InstallationID int64          `json:"installationID"`
	Repository     Repository     `json:"repository"`
	Revision       string         `json:"revision"`
	Ref            string         `json:"ref,omitempty"`
	Branch         string         `json:"branch,omitempty"`
	Tag            string         `json:"tag,omitempty"`
	Sender         string         `json:"sender,omitempty"`
	Pipeline       string         `json:"pipeline,omitempty"`
	DisplayName    string         `json:"displayName,omitempty"` // the pipeline's displayName, when set: its reports' name
	Push           *Push          `json:"push,omitempty"`
	PullRequest    *PullRequest   `json:"pullRequest,omitempty"`
	MergeGroup     *MergeGroup    `json:"mergeGroup,omitempty"`
	Comment        *Comment       `json:"comment,omitempty"`
	ReviewRequest  *ReviewRequest `json:"reviewRequest,omitempty"`
	Schedule       *Schedule      `json:"schedule,omitempty"`
	// ConfigRef is the ref .octomaton.yaml and pipeline definitions are read at: the default branch
	// for comment commands and review requests, the revision otherwise.
	ConfigRef string `json:"configRef,omitempty"`
	RerunBy   string `json:"rerunBy,omitempty"`
}

// ReportName is the name of the pipeline's report on the code host: its display name, or its name.
func (t Trigger) ReportName() string {
	if t.DisplayName != "" {
		return t.DisplayName
	}
	return t.Pipeline
}

// FromFork reports whether the trigger is a pull request's from a fork, which Octomaton never acts on.
func (t Trigger) FromFork() bool {
	return t.PullRequest.FromFork(t.Repository.FullName)
}

// ConfigAt is the ref configuration is read at.
func (t Trigger) ConfigAt() string {
	if t.ConfigRef != "" {
		return t.ConfigRef
	}
	return t.Revision
}

// Head identifies the line of work a run belongs to: runs of one pipeline at one commit and head
// are the same run (a redelivery, or a push and a pull request event for the same commit).
func (t Trigger) Head() string {
	if t.Tag != "" {
		return "tag:" + t.Tag
	}
	return t.Branch
}

// ShortSHA abbreviates a commit SHA for display.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Describe returns a one-line, Markdown description of the trigger.
func (t Trigger) Describe() string {
	var b strings.Builder
	switch {
	case t.Comment != nil && t.PullRequest != nil:
		fmt.Fprintf(&b, "`%s` on pull request #%d", t.Comment.Command, t.PullRequest.Number)
	case t.ReviewRequest != nil && t.PullRequest != nil && t.ReviewRequest.Pending:
		fmt.Fprintf(&b, "Review still requested from @%s on pull request #%d, after new commits", t.ReviewRequest.Reviewer, t.PullRequest.Number)
	case t.ReviewRequest != nil && t.PullRequest != nil:
		fmt.Fprintf(&b, "Review requested from @%s on pull request #%d", t.ReviewRequest.Reviewer, t.PullRequest.Number)
	case t.Schedule != nil:
		fmt.Fprintf(&b, "Schedule `%s` (slot %s) on `%s`", t.Schedule.Cron, t.Schedule.Slot, t.Branch)
	case t.PullRequest != nil:
		fmt.Fprintf(&b, "Pull request #%d (`%s` → `%s`)", t.PullRequest.Number, t.PullRequest.HeadRef, t.PullRequest.BaseRef)
		if t.Action != "" {
			fmt.Fprintf(&b, ", %s", t.Action)
		}
	case t.MergeGroup != nil:
		fmt.Fprintf(&b, "Merge queue into `%s`", strings.TrimPrefix(t.MergeGroup.BaseRef, "refs/heads/"))
	case t.Tag != "":
		fmt.Fprintf(&b, "Push of tag `%s`", t.Tag)
	case t.Event != "":
		fmt.Fprintf(&b, "Push to `%s`", t.Branch)
	}
	if t.Revision != "" {
		fmt.Fprintf(&b, " at `%s`", ShortSHA(t.Revision))
	}
	if t.Sender != "" {
		fmt.Fprintf(&b, " by @%s", t.Sender)
	}
	if t.RerunBy != "" {
		fmt.Fprintf(&b, "; re-run by @%s", t.RerunBy)
	}
	return b.String()
}

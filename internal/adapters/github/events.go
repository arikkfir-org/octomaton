package github

import (
	"fmt"
	"strings"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/services/ci"
)

// handledEvents are the webhook events Decode turns into ci.Events.
var handledEvents = map[string]bool{
	"push":          true,
	"pull_request":  true,
	"merge_group":   true,
	"check_run":     true,
	"check_suite":   true,
	"issue_comment": true,
}

// Handles reports whether Decode handles an event, by its X-GitHub-Event name.
func (a *App) Handles(event string) bool { return handledEvents[event] }

// Decode turns a verified webhook delivery into a ci.Event. It returns no event, and the reason,
// when the delivery is not for Octomaton: an event or action it ignores, a repository that is a fork
// or a pull request from one, a repository owner it does not serve, another App's check run, an
// incomplete payload. It returns an error when the payload cannot be parsed.
func (a *App) Decode(event, delivery string, body []byte) (ci.Event, string, error) {
	payload, err := github.ParseWebHook(event, body)
	if err != nil {
		return nil, "", fmt.Errorf("parsing the %s payload: %w", event, err)
	}
	var (
		ev     ci.Event
		reason string
		owner  string
		fork   bool
	)
	switch p := payload.(type) {
	case *github.PushEvent:
		var t ci.Trigger
		t, reason = pushTrigger(p, delivery)
		ev, owner, fork = &ci.TriggerEvent{Trigger: t}, t.Repository.Owner, p.GetRepo().GetFork()
	case *github.PullRequestEvent:
		var (
			t     ci.Trigger
			draft bool
		)
		t, draft, reason = pullRequestTrigger(p, delivery)
		ev, owner, fork = &ci.TriggerEvent{Trigger: t, Draft: draft}, t.Repository.Owner, p.GetRepo().GetFork()
	case *github.MergeGroupEvent:
		ev, reason = mergeGroupEvent(p, delivery)
		owner, fork = p.GetRepo().GetOwner().GetLogin(), p.GetRepo().GetFork()
	case *github.IssueCommentEvent:
		var c *ci.CommandEvent
		c, reason = commandEvent(p, delivery)
		ev, owner, fork = c, c.Repository.Owner, p.GetRepo().GetFork()
	case *github.CheckRunEvent:
		var r *ci.RerunEvent
		r, reason = a.checkRunRerun(p, delivery)
		ev, owner, fork = r, r.Repository.Owner, p.GetRepo().GetFork()
	case *github.CheckSuiteEvent:
		var r *ci.RerunEvent
		r, reason = a.checkSuiteRerun(p, delivery)
		ev, owner, fork = r, r.Repository.Owner, p.GetRepo().GetFork()
	default:
		reason = "unhandled event " + event
	}
	switch {
	case fork:
		return nil, "repository is a fork", nil
	case reason != "":
		return nil, reason, nil
	case !a.serves(owner):
		return nil, "repository owner is not allowed", nil
	}
	return ev, "", nil
}

func isZeroSHA(sha string) bool {
	return sha == "" || strings.Trim(sha, "0") == ""
}

func repository(r *github.Repository) ci.Repository {
	return ci.Repository{
		ID:            r.GetID(),
		Owner:         r.GetOwner().GetLogin(),
		Name:          r.GetName(),
		FullName:      r.GetFullName(),
		CloneURL:      r.GetCloneURL(),
		HTMLURL:       r.GetHTMLURL(),
		DefaultBranch: r.GetDefaultBranch(),
		Private:       r.GetPrivate(),
	}
}

// pushRepository converts a push payload's repository, whose owner may carry only a name.
func pushRepository(r *github.PushEventRepository) ci.Repository {
	owner := r.GetOwner().GetLogin()
	if owner == "" {
		owner = r.GetOwner().GetName()
	}
	return ci.Repository{
		ID:            r.GetID(),
		Owner:         owner,
		Name:          r.GetName(),
		FullName:      r.GetFullName(),
		CloneURL:      r.GetCloneURL(),
		HTMLURL:       r.GetHTMLURL(),
		DefaultBranch: r.GetDefaultBranch(),
		Private:       r.GetPrivate(),
	}
}

// incomplete says what a trigger lacks, or "".
func incomplete(t ci.Trigger) string {
	switch {
	case t.InstallationID == 0:
		return "no installation in payload"
	case t.Repository.FullName == "" || t.Repository.ID == 0:
		return "no repository in payload"
	case t.Revision == "":
		return "no commit in payload"
	}
	return ""
}

// pushTrigger converts a push. Deleted refs, merge queue branches and refs other than branches and
// tags are ignored. The revision is the pushed commit (head_commit, which for annotated tags
// differs from "after", the tag object).
func pushTrigger(ev *github.PushEvent, delivery string) (ci.Trigger, string) {
	t := ci.Trigger{
		Version:        ci.TriggerVersion,
		Event:          ci.EventPush,
		DeliveryID:     delivery,
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     pushRepository(ev.GetRepo()),
		Ref:            ev.GetRef(),
		Sender:         ev.GetSender().GetLogin(),
	}
	if ev.GetDeleted() || isZeroSHA(ev.GetAfter()) {
		return t, "ref was deleted"
	}
	switch {
	case strings.HasPrefix(t.Ref, "refs/heads/gh-readonly-queue/"):
		return t, "push to a merge queue branch (merge_group events cover it)"
	case strings.HasPrefix(t.Ref, "refs/heads/"):
		t.Branch = strings.TrimPrefix(t.Ref, "refs/heads/")
	case strings.HasPrefix(t.Ref, "refs/tags/"):
		t.Tag = strings.TrimPrefix(t.Ref, "refs/tags/")
	default:
		return t, "ref is neither a branch nor a tag"
	}
	t.Revision = ev.GetAfter()
	if id := ev.GetHeadCommit().GetID(); id != "" {
		t.Revision = id
	}
	t.Push = &ci.Push{Before: ev.GetBefore(), After: ev.GetAfter(), Created: ev.GetCreated()}
	return t, incomplete(t)
}

func pullRequestTrigger(ev *github.PullRequestEvent, delivery string) (ci.Trigger, bool, string) {
	pr := ev.GetPullRequest()
	if pr == nil {
		return ci.Trigger{}, false, "no pull request in payload"
	}
	number := ev.GetNumber()
	if number == 0 {
		number = pr.GetNumber()
	}
	t := ci.Trigger{
		Version:        ci.TriggerVersion,
		Event:          ci.EventPullRequest,
		Action:         ev.GetAction(),
		DeliveryID:     delivery,
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     repository(ev.GetRepo()),
		Revision:       pr.GetHead().GetSHA(),
		Ref:            fmt.Sprintf("refs/pull/%d/head", number),
		Branch:         pr.GetHead().GetRef(),
		Sender:         ev.GetSender().GetLogin(),
		PullRequest: &ci.PullRequest{
			Number:   number,
			HeadRef:  pr.GetHead().GetRef(),
			HeadSHA:  pr.GetHead().GetSHA(),
			BaseRef:  pr.GetBase().GetRef(),
			BaseSHA:  pr.GetBase().GetSHA(),
			HeadRepo: pr.GetHead().GetRepo().GetFullName(),
			Author:   pr.GetUser().GetLogin(),
			HTMLURL:  pr.GetHTMLURL(),
		},
	}
	if t.FromFork() {
		return t, pr.GetDraft(), "pull request from a fork"
	}
	if ev.GetAction() == "review_requested" {
		t, reason := reviewRequestTrigger(t, ev)
		return t, pr.GetDraft(), reason
	}
	return t, pr.GetDraft(), incomplete(t)
}

// reviewRequestTrigger turns a review requested from a user on an open pull request into a
// review_request trigger, read at the default branch like a comment command. GitHub sends one
// delivery per requested reviewer; team requests are ignored.
func reviewRequestTrigger(t ci.Trigger, ev *github.PullRequestEvent) (ci.Trigger, string) {
	reviewer := ev.GetRequestedReviewer().GetLogin()
	switch {
	case ev.GetRequestedTeam() != nil && reviewer == "":
		return t, "review requested from a team"
	case reviewer == "":
		return t, "no requested reviewer in payload"
	case ev.GetPullRequest().GetState() != "open":
		return t, "review requested on a pull request that is not open"
	case t.Repository.DefaultBranch == "":
		return t, "no default branch in payload"
	}
	t.Event = ci.EventReviewRequest
	t.ReviewRequest = &ci.ReviewRequest{Reviewer: reviewer}
	t.ConfigRef = t.Repository.DefaultBranch
	return t, incomplete(t)
}

// mergeGroupEvent converts a merge group ready for checks, or one the queue destroyed.
func mergeGroupEvent(ev *github.MergeGroupEvent, delivery string) (ci.Event, string) {
	mg := ev.GetMergeGroup()
	if mg == nil {
		return nil, "no merge group in payload"
	}
	t := ci.Trigger{
		Version:        ci.TriggerVersion,
		Event:          ci.EventMergeGroup,
		Action:         ev.GetAction(),
		DeliveryID:     delivery,
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     repository(ev.GetRepo()),
		Revision:       mg.GetHeadSHA(),
		Ref:            mg.GetHeadRef(),
		Branch:         strings.TrimPrefix(mg.GetHeadRef(), "refs/heads/"),
		Sender:         ev.GetSender().GetLogin(),
		MergeGroup: &ci.MergeGroup{
			HeadRef: mg.GetHeadRef(),
			HeadSHA: mg.GetHeadSHA(),
			BaseRef: mg.GetBaseRef(),
			BaseSHA: mg.GetBaseSHA(),
		},
	}
	if reason := incomplete(t); reason != "" {
		return nil, reason
	}
	switch ev.GetAction() {
	case "checks_requested":
		return &ci.TriggerEvent{Trigger: t}, ""
	case "destroyed":
		return &ci.MergeGroupDestroyed{Trigger: t, Reason: ev.GetReason()}, ""
	default:
		return nil, "unhandled merge_group action " + ev.GetAction()
	}
}

// commandEvent accepts pull request comments created with a first line starting with "/".
func commandEvent(ev *github.IssueCommentEvent, delivery string) (*ci.CommandEvent, string) {
	c := &ci.CommandEvent{
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     repository(ev.GetRepo()),
		Number:         ev.GetIssue().GetNumber(),
		CommentID:      ev.GetComment().GetID(),
		Author:         ev.GetComment().GetUser().GetLogin(),
		Line:           firstLine(ev.GetComment().GetBody()),
		DeliveryID:     delivery,
	}
	if c.Author == "" {
		c.Author = ev.GetSender().GetLogin()
	}
	switch {
	case ev.GetAction() != "created":
		return c, "unhandled issue_comment action " + ev.GetAction()
	case ev.GetIssue() == nil || !ev.GetIssue().IsPullRequest():
		return c, "comment is not on a pull request"
	case !strings.HasPrefix(c.Line, "/"):
		return c, "comment is not a command"
	case c.InstallationID == 0:
		return c, "no installation in payload"
	case c.Repository.FullName == "" || c.CommentID == 0 || c.Number == 0 || c.Author == "":
		return c, "incomplete payload"
	}
	return c, ""
}

// firstLine returns the first line of a comment, trimmed.
func firstLine(body string) string {
	line, _, _ := strings.Cut(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	return strings.TrimSpace(line)
}

// checkRunRerun accepts a re-run of one of the App's check runs.
func (a *App) checkRunRerun(ev *github.CheckRunEvent, delivery string) (*ci.RerunEvent, string) {
	cr := ev.GetCheckRun()
	r := &ci.RerunEvent{
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     repository(ev.GetRepo()),
		Reports:        []ci.ReportRef{reportRef(cr)},
		Requester:      ev.GetSender().GetLogin(),
		DeliveryID:     delivery,
	}
	if ev.GetAction() != "rerequested" {
		return r, "unhandled check_run action " + ev.GetAction()
	}
	if cr.GetApp().GetID() != a.id {
		return r, "check run belongs to another app"
	}
	return r, incompleteRerun(r)
}

// checkSuiteRerun accepts a re-run of all of the App's check runs in a suite.
func (a *App) checkSuiteRerun(ev *github.CheckSuiteEvent, delivery string) (*ci.RerunEvent, string) {
	suite := ev.GetCheckSuite()
	r := &ci.RerunEvent{
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     repository(ev.GetRepo()),
		SuiteID:        suite.GetID(),
		Requester:      ev.GetSender().GetLogin(),
		DeliveryID:     delivery,
	}
	if ev.GetAction() != "rerequested" {
		return r, "unhandled check_suite action " + ev.GetAction()
	}
	if suite.GetApp().GetID() != a.id {
		return r, "check suite belongs to another app"
	}
	return r, incompleteRerun(r)
}

func incompleteRerun(r *ci.RerunEvent) string {
	switch {
	case r.InstallationID == 0:
		return "no installation in payload"
	case r.Repository.FullName == "":
		return "no repository in payload"
	case r.Requester == "":
		return "no sender in payload"
	}
	return ""
}

package trigger

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/services/pipelines"
)

func isZeroSHA(sha string) bool {
	return sha == "" || strings.Trim(sha, "0") == ""
}

func repository(r *github.Repository) checkrun.Repository {
	return checkrun.Repository{
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

func pushRepository(r *github.PushEventRepository) checkrun.Repository {
	owner := r.GetOwner().GetLogin()
	if owner == "" {
		owner = r.GetOwner().GetName()
	}
	return checkrun.Repository{
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

func fromRepository(r githubapp.Repository) checkrun.Repository {
	return checkrun.Repository{
		ID:            r.ID,
		Owner:         r.Owner,
		Name:          r.Name,
		FullName:      r.FullName,
		CloneURL:      r.CloneURL,
		HTMLURL:       r.HTMLURL,
		DefaultBranch: r.DefaultBranch,
		Private:       r.Private,
	}
}

func validate(c checkrun.Context) string {
	switch {
	case c.InstallationID == 0:
		return "no installation in payload"
	case c.Repository.FullName == "" || c.Repository.ID == 0:
		return "no repository in payload"
	case c.Revision == "":
		return "no commit in payload"
	}
	return ""
}

// pushContext converts a push event. Deleted refs and refs other than branches
// and tags are ignored. The revision is the pushed commit (head_commit, which for
// annotated tags differs from "after", the tag object).
func pushContext(ev *github.PushEvent, delivery string) (checkrun.Context, string) {
	c := checkrun.Context{
		Version:        checkrun.ContextVersion,
		Event:          checkrun.EventPush,
		DeliveryID:     delivery,
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     pushRepository(ev.GetRepo()),
		Ref:            ev.GetRef(),
		Sender:         ev.GetSender().GetLogin(),
	}
	if ev.GetDeleted() || isZeroSHA(ev.GetAfter()) {
		return c, "ref was deleted"
	}
	switch {
	case strings.HasPrefix(c.Ref, "refs/heads/gh-readonly-queue/"):
		return c, "push to a merge queue branch (merge_group events cover it)"
	case strings.HasPrefix(c.Ref, "refs/heads/"):
		c.Branch = strings.TrimPrefix(c.Ref, "refs/heads/")
	case strings.HasPrefix(c.Ref, "refs/tags/"):
		c.Tag = strings.TrimPrefix(c.Ref, "refs/tags/")
	default:
		return c, "ref is neither a branch nor a tag"
	}
	c.Revision = ev.GetAfter()
	if id := ev.GetHeadCommit().GetID(); id != "" {
		c.Revision = id
	}
	c.Push = &checkrun.Push{Before: ev.GetBefore(), After: ev.GetAfter(), Created: ev.GetCreated()}
	return c, validate(c)
}

func pullRequestContext(ev *github.PullRequestEvent, delivery string) (checkrun.Context, bool, string) {
	pr := ev.GetPullRequest()
	if pr == nil {
		return checkrun.Context{}, false, "no pull request in payload"
	}
	number := ev.GetNumber()
	if number == 0 {
		number = pr.GetNumber()
	}
	c := checkrun.Context{
		Version:        checkrun.ContextVersion,
		Event:          checkrun.EventPullRequest,
		Action:         ev.GetAction(),
		DeliveryID:     delivery,
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     repository(ev.GetRepo()),
		Revision:       pr.GetHead().GetSHA(),
		Ref:            fmt.Sprintf("refs/pull/%d/head", number),
		Branch:         pr.GetHead().GetRef(),
		Sender:         ev.GetSender().GetLogin(),
		PullRequest: &checkrun.PullRequest{
			Number:            number,
			HeadRef:           pr.GetHead().GetRef(),
			HeadSHA:           pr.GetHead().GetSHA(),
			BaseRef:           pr.GetBase().GetRef(),
			BaseSHA:           pr.GetBase().GetSHA(),
			HeadRepo:          pr.GetHead().GetRepo().GetFullName(),
			Author:            pr.GetUser().GetLogin(),
			AuthorAssociation: pr.GetAuthorAssociation(),
			HTMLURL:           pr.GetHTMLURL(),
		},
	}
	return c, pr.GetDraft(), validate(c)
}

func mergeGroupContext(ev *github.MergeGroupEvent, delivery string) (checkrun.Context, string) {
	mg := ev.GetMergeGroup()
	if mg == nil {
		return checkrun.Context{}, "no merge group in payload"
	}
	c := checkrun.Context{
		Version:        checkrun.ContextVersion,
		Event:          checkrun.EventMergeGroup,
		Action:         ev.GetAction(),
		DeliveryID:     delivery,
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     repository(ev.GetRepo()),
		Revision:       mg.GetHeadSHA(),
		Ref:            mg.GetHeadRef(),
		Branch:         strings.TrimPrefix(mg.GetHeadRef(), "refs/heads/"),
		Sender:         ev.GetSender().GetLogin(),
		MergeGroup: &checkrun.MergeGroup{
			HeadRef: mg.GetHeadRef(),
			HeadSHA: mg.GetHeadSHA(),
			BaseRef: mg.GetBaseRef(),
			BaseSHA: mg.GetBaseSHA(),
		},
	}
	return c, validate(c)
}

// CommentRequest is a pull request comment that may be a command.
type CommentRequest struct {
	InstallationID int64
	Repository     checkrun.Repository
	Number         int
	CommentID      int64
	Author         string
	// Line is the comment's first line, trimmed.
	Line       string
	DeliveryID string
}

// commentRequest accepts issue_comment "created" events on pull requests whose
// first line starts with "/".
func commentRequest(ev *github.IssueCommentEvent, delivery string) (CommentRequest, string) {
	req := CommentRequest{
		InstallationID: ev.GetInstallation().GetID(),
		Repository:     repository(ev.GetRepo()),
		Number:         ev.GetIssue().GetNumber(),
		CommentID:      ev.GetComment().GetID(),
		Author:         ev.GetComment().GetUser().GetLogin(),
		Line:           firstLine(ev.GetComment().GetBody()),
		DeliveryID:     delivery,
	}
	if req.Author == "" {
		req.Author = ev.GetSender().GetLogin()
	}
	switch {
	case ev.GetAction() != "created":
		return req, "unhandled issue_comment action " + ev.GetAction()
	case ev.GetIssue() == nil || !ev.GetIssue().IsPullRequest():
		return req, "comment is not on a pull request"
	case !strings.HasPrefix(req.Line, "/"):
		return req, "comment is not a command"
	case req.InstallationID == 0:
		return req, "no installation in payload"
	case req.Repository.FullName == "" || req.CommentID == 0 || req.Number == 0 || req.Author == "":
		return req, "incomplete payload"
	}
	return req, ""
}

// matchEvent extracts what pipeline matching looks at.
func matchEvent(c checkrun.Context, draft bool) pipelines.Event {
	ev := pipelines.Event{Name: c.Event, Action: c.Action, Draft: draft}
	switch {
	case c.PullRequest != nil:
		ev.Branch = c.PullRequest.BaseRef
	case c.MergeGroup != nil:
		ev.Branch = strings.TrimPrefix(c.MergeGroup.BaseRef, "refs/heads/")
	default:
		ev.Branch, ev.Tag = c.Branch, c.Tag
	}
	return ev
}

// trustedAssociations may run pipelines on their pull requests without approval.
var trustedAssociations = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

// Trusted reports whether a pull request's pipelines may run without approval:
// its author is an owner, member or collaborator of the repository, or its head
// branch lives in the base repository itself (which requires write access).
func Trusted(pr *checkrun.PullRequest, baseRepoFullName string) bool {
	if pr == nil {
		return true
	}
	if trustedAssociations[strings.ToUpper(pr.AuthorAssociation)] {
		return true
	}
	return pr.HeadRepo != "" && strings.EqualFold(pr.HeadRepo, baseRepoFullName)
}

// changedFiles lists the files an event changes. On errors, and for new refs
// (where every path counts as changed), the result is marked incomplete so that
// path filters fail open.
func (s *Service) changedFiles(ctx context.Context, gh githubapp.Client, c checkrun.Context) githubapp.ChangedFiles {
	owner, repo := c.Repository.Owner, c.Repository.Name
	var (
		files githubapp.ChangedFiles
		err   error
	)
	switch {
	case c.PullRequest != nil:
		files, err = gh.PullRequestFiles(ctx, owner, repo, c.PullRequest.Number)
	case c.MergeGroup != nil:
		files, err = gh.CompareFiles(ctx, owner, repo, c.MergeGroup.BaseSHA, c.MergeGroup.HeadSHA)
	case c.Push != nil:
		if c.Push.Created || isZeroSHA(c.Push.Before) {
			return githubapp.ChangedFiles{Complete: false}
		}
		files, err = gh.CompareFiles(ctx, owner, repo, c.Push.Before, c.Push.After)
	default:
		return githubapp.ChangedFiles{Complete: false}
	}
	if err != nil {
		s.logFor(c).Warn("Could not determine changed files; path filters are ignored", "error", err)
		return githubapp.ChangedFiles{Complete: false}
	}
	return files
}

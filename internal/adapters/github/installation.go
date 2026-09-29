package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/services/ci"
)

const (
	// maxPullRequestFiles is the most files the pull request files API returns.
	maxPullRequestFiles = 3000
	// maxCompareFiles is the most files the compare API returns.
	maxCompareFiles = 300
)

// installation is GitHub as one installation of the App sees it.
type installation struct {
	app *App
	gh  *github.Client
}

var _ ci.Installation = (*installation)(nil)

func isNotFound(resp *github.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}

// Repositories lists the installation's repositories that are served and not archived.
func (c *installation) Repositories(ctx context.Context) ([]ci.Repository, error) {
	var out []ci.Repository
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.gh.Apps.ListRepos(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing installation repositories: %w", err)
		}
		for _, r := range page.Repositories {
			if repo := repository(r); !r.GetArchived() && c.app.serves(repo.Owner) {
				out = append(out, repo)
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *installation) ReadFile(ctx context.Context, repo ci.Repository, path, ref string) ([]byte, error) {
	file, _, resp, err := c.gh.Repositories.GetContents(ctx, repo.Owner, repo.Name, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if isNotFound(resp) {
			return nil, ci.ErrNotFound
		}
		return nil, fmt.Errorf("fetching %s@%s: %w", path, ref, err)
	}
	if file == nil {
		return nil, fmt.Errorf("fetching %s@%s: path is a directory", path, ref)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decoding %s@%s: %w", path, ref, err)
	}
	return []byte(content), nil
}

func (c *installation) PullRequestFiles(ctx context.Context, repo ci.Repository, number int) (ci.ChangedFiles, error) {
	var files []string
	count := 0
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.gh.PullRequests.ListFiles(ctx, repo.Owner, repo.Name, number, opts)
		if err != nil {
			return ci.ChangedFiles{}, fmt.Errorf("listing files of pull request #%d: %w", number, err)
		}
		count += len(page)
		files = appendFileNames(files, page)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return ci.ChangedFiles{Files: files, Complete: count < maxPullRequestFiles}, nil
}

func (c *installation) CompareFiles(ctx context.Context, repo ci.Repository, base, head string) (ci.ChangedFiles, error) {
	cmp, _, err := c.gh.Repositories.CompareCommits(ctx, repo.Owner, repo.Name, base, head, &github.ListOptions{PerPage: 1})
	if err != nil {
		return ci.ChangedFiles{}, fmt.Errorf("comparing %s...%s: %w", base, head, err)
	}
	return ci.ChangedFiles{Files: appendFileNames(nil, cmp.Files), Complete: len(cmp.Files) < maxCompareFiles}, nil
}

// appendFileNames adds the paths a change touches, including the old path of a renamed file.
func appendFileNames(dst []string, files []*github.CommitFile) []string {
	for _, f := range files {
		if name := f.GetFilename(); name != "" {
			dst = append(dst, name)
		}
		if prev := f.GetPreviousFilename(); prev != "" {
			dst = append(dst, prev)
		}
	}
	return dst
}

func (c *installation) PullRequest(ctx context.Context, repo ci.Repository, number int) (ci.PullRequestState, error) {
	pr, resp, err := c.gh.PullRequests.Get(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		if isNotFound(resp) {
			return ci.PullRequestState{}, ci.ErrNotFound
		}
		return ci.PullRequestState{}, fmt.Errorf("fetching pull request #%d: %w", number, err)
	}
	return ci.PullRequestState{
		PullRequest: ci.PullRequest{
			Number:            pr.GetNumber(),
			HeadRef:           pr.GetHead().GetRef(),
			HeadSHA:           pr.GetHead().GetSHA(),
			BaseRef:           pr.GetBase().GetRef(),
			BaseSHA:           pr.GetBase().GetSHA(),
			HeadRepo:          pr.GetHead().GetRepo().GetFullName(),
			Author:            pr.GetUser().GetLogin(),
			AuthorAssociation: pr.GetAuthorAssociation(),
			HTMLURL:           pr.GetHTMLURL(),
		},
		State: pr.GetState(),
		Draft: pr.GetDraft(),
	}, nil
}

func (c *installation) BranchHead(ctx context.Context, repo ci.Repository, branch string) (string, error) {
	ref, resp, err := c.gh.Git.GetRef(ctx, repo.Owner, repo.Name, "heads/"+branch)
	if err != nil {
		if isNotFound(resp) {
			return "", ci.ErrNotFound
		}
		return "", fmt.Errorf("fetching branch %s: %w", branch, err)
	}
	return ref.GetObject().GetSHA(), nil
}

// Permission returns a user's permission on a repository; "none" when they are not a collaborator.
func (c *installation) Permission(ctx context.Context, repo ci.Repository, user string) (ci.Permission, error) {
	level, resp, err := c.gh.Repositories.GetPermissionLevel(ctx, repo.Owner, repo.Name, user)
	if err != nil {
		if isNotFound(resp) {
			return "none", nil
		}
		return "", fmt.Errorf("fetching %s's permission on %s: %w", user, repo.FullName, err)
	}
	return ci.Permission(level.GetPermission()), nil
}

func (c *installation) React(ctx context.Context, repo ci.Repository, commentID int64, reaction string) error {
	if _, _, err := c.gh.Reactions.CreateIssueCommentReaction(ctx, repo.Owner, repo.Name, commentID, reaction); err != nil {
		return fmt.Errorf("reacting %s to comment %d: %w", reaction, commentID, err)
	}
	return nil
}

// Comment posts a comment, shortened to MaxSummaryLength.
func (c *installation) Comment(ctx context.Context, repo ci.Repository, number int, body string) error {
	req := github.IssueCommentRequest{Body: Truncate(body, MaxSummaryLength)}
	if _, _, err := c.gh.Issues.CreateComment(ctx, repo.Owner, repo.Name, number, req); err != nil {
		return fmt.Errorf("commenting on #%d: %w", number, err)
	}
	return nil
}

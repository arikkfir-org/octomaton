// Package githubapp authenticates as the Octomaton GitHub App and exposes the
// small set of GitHub API operations Octomaton needs.
package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v92/github"
)

// ErrNotFound is returned when a requested file or object does not exist.
var ErrNotFound = errors.New("not found")

const (
	// DefaultBaseURL is the GitHub REST API endpoint.
	DefaultBaseURL = "https://api.github.com/"
	requestTimeout = 30 * time.Second
	// maxPullRequestFiles is the most files the pull request files API returns.
	maxPullRequestFiles = 3000
	// maxCompareFiles is the most files the compare API returns.
	maxCompareFiles = 300
)

// ChangedFiles is the set of paths changed by an event. Complete is false when
// GitHub truncated the list, in which case the list must not be used to skip work.
type ChangedFiles struct {
	Files    []string
	Complete bool
}

// CheckRunUpdate is the body of a check-run update. Unlike go-github's
// UpdateCheckRunOptions it can set started_at.
type CheckRunUpdate struct {
	Name        string                   `json:"name,omitempty"`
	DetailsURL  *string                  `json:"details_url,omitempty"`
	ExternalID  *string                  `json:"external_id,omitempty"`
	Status      *string                  `json:"status,omitempty"`
	Conclusion  *string                  `json:"conclusion,omitempty"`
	StartedAt   *github.Timestamp        `json:"started_at,omitempty"`
	CompletedAt *github.Timestamp        `json:"completed_at,omitempty"`
	Output      *github.CheckRunOutput   `json:"output,omitempty"`
	Actions     []*github.CheckRunAction `json:"actions,omitempty"`
}

// Client is the set of GitHub operations Octomaton performs on behalf of one installation.
type Client interface {
	// GetFile returns the content of a file at ref, or ErrNotFound.
	GetFile(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
	// PullRequestFiles lists the files changed by a pull request.
	PullRequestFiles(ctx context.Context, owner, repo string, number int) (ChangedFiles, error)
	// CompareFiles lists the files changed between two commits.
	CompareFiles(ctx context.Context, owner, repo, base, head string) (ChangedFiles, error)
	CreateCheckRun(ctx context.Context, owner, repo string, opts github.CreateCheckRunOptions) (*github.CheckRun, error)
	UpdateCheckRun(ctx context.Context, owner, repo string, id int64, opts CheckRunUpdate) (*github.CheckRun, error)
	GetCheckRun(ctx context.Context, owner, repo string, id int64) (*github.CheckRun, error)
	// SuiteCheckRuns lists the latest check run of each name in a check suite.
	SuiteCheckRuns(ctx context.Context, owner, repo string, suiteID int64) ([]*github.CheckRun, error)
	// PermissionLevel returns a user's permission on a repository: admin, write, read or none.
	PermissionLevel(ctx context.Context, owner, repo, user string) (string, error)
	// PullRequest returns a pull request, or ErrNotFound.
	PullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error)
	// BranchHead returns the commit a branch points at, or ErrNotFound.
	BranchHead(ctx context.Context, owner, repo, branch string) (string, error)
	// FindCheckRun returns the newest of this App's check runs on sha with the
	// given name and external ID, or 0 when there is none.
	FindCheckRun(ctx context.Context, owner, repo, sha, name, externalID string) (int64, error)
	// React adds a reaction ("eyes", "-1", ...) to an issue or pull request comment.
	React(ctx context.Context, owner, repo string, commentID int64, content string) error
	// Comment posts a comment on an issue or pull request.
	Comment(ctx context.Context, owner, repo string, number int, body string) error
	// Repositories lists the repositories the installation can access.
	Repositories(ctx context.Context) ([]Repository, error)
}

// PullRequest is the state of a pull request.
type PullRequest struct {
	Number            int
	State             string
	Draft             bool
	HeadRef           string
	HeadSHA           string
	HeadRepo          string
	BaseRef           string
	BaseSHA           string
	Author            string
	AuthorAssociation string
	HTMLURL           string
}

// Repository is a repository an installation can access.
type Repository struct {
	ID            int64
	Owner         string
	Name          string
	FullName      string
	CloneURL      string
	HTMLURL       string
	DefaultBranch string
	Private       bool
	Archived      bool
}

// Installation is an installation of the App.
type Installation struct {
	ID      int64
	Account string
}

// Token is a GitHub installation access token.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Provider hands out installation clients and repository-scoped tokens.
type Provider interface {
	// AppID is the GitHub App's ID.
	AppID() int64
	// Installation returns a client authenticated as the given installation.
	Installation(id int64) Client
	// RepositoryToken mints an installation token restricted to one repository and the given permissions.
	RepositoryToken(ctx context.Context, installationID, repositoryID int64, permissions map[string]string) (Token, error)
	// Installations lists the App's installations.
	Installations(ctx context.Context) ([]Installation, error)
}

// App authenticates as a GitHub App. Installation tokens are cached (and refreshed
// shortly before they expire) by ghinstallation, one transport per installation.
type App struct {
	id        int64
	baseURL   string
	transport http.RoundTripper
	apps      *ghinstallation.AppsTransport
	appClient *github.Client

	mu      sync.Mutex
	clients map[int64]*client
}

// Option customizes an App.
type Option func(*App)

// WithBaseURL points the App at another GitHub API endpoint (used by tests).
func WithBaseURL(u string) Option {
	return func(a *App) { a.baseURL = u }
}

// WithTransport sets the HTTP transport used for every GitHub request.
func WithTransport(rt http.RoundTripper) Option {
	return func(a *App) { a.transport = rt }
}

// New creates an App for the given App ID and private key.
func New(appID int64, key *rsa.PrivateKey, opts ...Option) (*App, error) {
	a := &App{id: appID, baseURL: DefaultBaseURL, transport: http.DefaultTransport, clients: map[int64]*client{}}
	for _, opt := range opts {
		opt(a)
	}
	if !strings.HasSuffix(a.baseURL, "/") {
		a.baseURL += "/"
	}
	a.apps = ghinstallation.NewAppsTransportFromPrivateKey(a.transport, appID, key)
	a.apps.BaseURL = strings.TrimRight(a.baseURL, "/")
	gh, err := newGitHubClient(a.apps, a.baseURL)
	if err != nil {
		return nil, err
	}
	a.appClient = gh
	return a, nil
}

func newGitHubClient(rt http.RoundTripper, baseURL string) (*github.Client, error) {
	return github.NewClient(
		github.WithHTTPClient(&http.Client{Transport: rt, Timeout: requestTimeout}),
		github.WithURLs(&baseURL, &baseURL),
		github.WithUserAgent("octomaton"),
	)
}

// AppID returns the GitHub App ID.
func (a *App) AppID() int64 { return a.id }

// Installation returns the (cached) client for an installation.
func (a *App) Installation(id int64) Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[id]; ok {
		return c
	}
	tr := ghinstallation.NewFromAppsTransport(a.apps, id)
	gh, err := newGitHubClient(tr, a.baseURL)
	if err != nil {
		// The base URL was already validated in New, so this cannot happen.
		panic(fmt.Sprintf("creating GitHub client: %v", err))
	}
	c := &client{gh: gh, appID: a.id}
	a.clients[id] = c
	return c
}

// Installations lists the App's installations.
func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	var out []Installation
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := a.appClient.Apps.ListInstallations(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing installations: %w", err)
		}
		for _, inst := range page {
			out = append(out, Installation{ID: inst.GetID(), Account: inst.GetAccount().GetLogin()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// RepositoryToken mints a short-lived installation token restricted to one
// repository. With no permissions it grants contents:read only.
func (a *App) RepositoryToken(ctx context.Context, installationID, repositoryID int64, permissions map[string]string) (Token, error) {
	if len(permissions) == 0 {
		permissions = map[string]string{"contents": "read"}
	}
	perms, err := ParsePermissions(permissions)
	if err != nil {
		return Token{}, err
	}
	tok, _, err := a.appClient.Apps.CreateInstallationToken(ctx, installationID, &github.InstallationTokenOptions{
		RepositoryIDs: []int64{repositoryID},
		Permissions:   perms,
	})
	if err != nil {
		return Token{}, fmt.Errorf("creating installation token: %w", err)
	}
	return Token{Value: tok.GetToken(), ExpiresAt: tok.GetExpiresAt().Time}, nil
}

// ParsePermissions converts a permission map such as {"contents": "read"} into
// the GitHub API type, rejecting unknown permission names and access levels.
func ParsePermissions(m map[string]string) (*github.InstallationPermissions, error) {
	names := make([]string, 0, len(m))
	for name, level := range m {
		switch level {
		case "read", "write", "admin":
		default:
			return nil, fmt.Errorf("permission %q: access level must be read, write or admin (got %q)", name, level)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var perms github.InstallationPermissions
	if err := dec.Decode(&perms); err != nil {
		return nil, fmt.Errorf("invalid permissions %v: %w", names, err)
	}
	return &perms, nil
}

// CheckPermissions reports permissions ParsePermissions rejects.
func CheckPermissions(m map[string]string) error {
	_, err := ParsePermissions(m)
	return err
}

type client struct {
	gh    *github.Client
	appID int64
}

var _ Client = (*client)(nil)

func isNotFound(resp *github.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}

func (c *client) GetFile(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	file, _, resp, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if isNotFound(resp) {
			return nil, ErrNotFound
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

func (c *client) PullRequestFiles(ctx context.Context, owner, repo string, number int) (ChangedFiles, error) {
	var files []string
	count := 0
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.gh.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return ChangedFiles{}, fmt.Errorf("listing files of pull request #%d: %w", number, err)
		}
		count += len(page)
		files = appendFileNames(files, page)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return ChangedFiles{Files: files, Complete: count < maxPullRequestFiles}, nil
}

func (c *client) CompareFiles(ctx context.Context, owner, repo, base, head string) (ChangedFiles, error) {
	cmp, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, base, head, &github.ListOptions{PerPage: 1})
	if err != nil {
		return ChangedFiles{}, fmt.Errorf("comparing %s...%s: %w", base, head, err)
	}
	return ChangedFiles{Files: appendFileNames(nil, cmp.Files), Complete: len(cmp.Files) < maxCompareFiles}, nil
}

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

func (c *client) CreateCheckRun(ctx context.Context, owner, repo string, opts github.CreateCheckRunOptions) (*github.CheckRun, error) {
	cr, _, err := c.gh.Checks.CreateCheckRun(ctx, owner, repo, opts)
	if err != nil {
		return nil, fmt.Errorf("creating check run %q: %w", opts.Name, err)
	}
	return cr, nil
}

func (c *client) UpdateCheckRun(ctx context.Context, owner, repo string, id int64, opts CheckRunUpdate) (*github.CheckRun, error) {
	req, err := c.gh.NewRequest(ctx, http.MethodPatch, fmt.Sprintf("repos/%v/%v/check-runs/%v", owner, repo, id), opts)
	if err != nil {
		return nil, err
	}
	cr := new(github.CheckRun)
	if _, err := c.gh.Do(req, cr); err != nil {
		return nil, fmt.Errorf("updating check run %d: %w", id, err)
	}
	return cr, nil
}

func (c *client) GetCheckRun(ctx context.Context, owner, repo string, id int64) (*github.CheckRun, error) {
	cr, resp, err := c.gh.Checks.GetCheckRun(ctx, owner, repo, id)
	if err != nil {
		if isNotFound(resp) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("fetching check run %d: %w", id, err)
	}
	return cr, nil
}

func (c *client) SuiteCheckRuns(ctx context.Context, owner, repo string, suiteID int64) ([]*github.CheckRun, error) {
	var runs []*github.CheckRun
	opts := &github.ListCheckRunsOptions{Filter: new("latest"), ListOptions: github.ListOptions{PerPage: 100}}
	for {
		res, resp, err := c.gh.Checks.ListCheckRunsCheckSuite(ctx, owner, repo, suiteID, opts)
		if err != nil {
			return nil, fmt.Errorf("listing check runs of suite %d: %w", suiteID, err)
		}
		runs = append(runs, res.CheckRuns...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return runs, nil
}

func (c *client) PermissionLevel(ctx context.Context, owner, repo, user string) (string, error) {
	level, resp, err := c.gh.Repositories.GetPermissionLevel(ctx, owner, repo, user)
	if err != nil {
		if isNotFound(resp) {
			return "none", nil
		}
		return "", fmt.Errorf("fetching %s's permission on %s/%s: %w", user, owner, repo, err)
	}
	return level.GetPermission(), nil
}

// CanWrite reports whether a permission level (as returned by PermissionLevel)
// allows pushing to the repository.
func CanWrite(level string) bool {
	return level == "admin" || level == "write" || level == "maintain"
}

func (c *client) PullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	pr, resp, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if isNotFound(resp) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("fetching pull request #%d: %w", number, err)
	}
	return &PullRequest{
		Number:            pr.GetNumber(),
		State:             pr.GetState(),
		Draft:             pr.GetDraft(),
		HeadRef:           pr.GetHead().GetRef(),
		HeadSHA:           pr.GetHead().GetSHA(),
		HeadRepo:          pr.GetHead().GetRepo().GetFullName(),
		BaseRef:           pr.GetBase().GetRef(),
		BaseSHA:           pr.GetBase().GetSHA(),
		Author:            pr.GetUser().GetLogin(),
		AuthorAssociation: pr.GetAuthorAssociation(),
		HTMLURL:           pr.GetHTMLURL(),
	}, nil
}

func (c *client) BranchHead(ctx context.Context, owner, repo, branch string) (string, error) {
	ref, resp, err := c.gh.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		if isNotFound(resp) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("fetching branch %s: %w", branch, err)
	}
	return ref.GetObject().GetSHA(), nil
}

func (c *client) FindCheckRun(ctx context.Context, owner, repo, sha, name, externalID string) (int64, error) {
	opts := &github.ListCheckRunsOptions{CheckName: new(name), Filter: new("all"), ListOptions: github.ListOptions{PerPage: 100}}
	if c.appID != 0 {
		opts.AppID = new(c.appID)
	}
	var found int64
	for {
		res, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return 0, fmt.Errorf("listing check runs of %s: %w", sha, err)
		}
		for _, cr := range res.CheckRuns {
			if cr.GetName() == name && cr.GetExternalID() == externalID && cr.GetID() > found {
				found = cr.GetID()
			}
		}
		if resp.NextPage == 0 {
			return found, nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *client) React(ctx context.Context, owner, repo string, commentID int64, content string) error {
	if _, _, err := c.gh.Reactions.CreateIssueCommentReaction(ctx, owner, repo, commentID, content); err != nil {
		return fmt.Errorf("reacting %s to comment %d: %w", content, commentID, err)
	}
	return nil
}

func (c *client) Comment(ctx context.Context, owner, repo string, number int, body string) error {
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, github.IssueCommentRequest{Body: body}); err != nil {
		return fmt.Errorf("commenting on #%d: %w", number, err)
	}
	return nil
}

func (c *client) Repositories(ctx context.Context) ([]Repository, error) {
	var out []Repository
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.gh.Apps.ListRepos(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing installation repositories: %w", err)
		}
		for _, r := range page.Repositories {
			out = append(out, Repository{
				ID:            r.GetID(),
				Owner:         r.GetOwner().GetLogin(),
				Name:          r.GetName(),
				FullName:      r.GetFullName(),
				CloneURL:      r.GetCloneURL(),
				HTMLURL:       r.GetHTMLURL(),
				DefaultBranch: r.GetDefaultBranch(),
				Private:       r.GetPrivate(),
				Archived:      r.GetArchived(),
			})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

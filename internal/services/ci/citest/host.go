// Package citest holds in-memory implementations of the ci ports for services' tests: a code host
// that stores files, pull requests and reports, and a runner whose runs tests move along by hand.
package citest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"octomaton.dev/internal/services/ci"
)

// Report is a report the Host stores.
type Report struct {
	ci.Report
	ID ci.ReportID
	// Repository is the repository's full name.
	Repository string
	SuiteID    int64
	// Updates counts the updates the report received.
	Updates int
}

// TokenRequest is a token the Host minted.
type TokenRequest struct {
	InstallationID int64
	// RepositoryID is zero for a token for every repository of the installation.
	RepositoryID int64
	Permissions  map[string]string
}

// Reaction is a reaction added to a comment.
type Reaction struct {
	Repository string
	CommentID  int64
	Reaction   string
}

// Comment is a comment posted on a pull request.
type Comment struct {
	Repository string
	Number     int
	Body       string
}

// Host is an in-memory ci.CodeHost. Every installation sees every repository's contents.
type Host struct {
	// PermissionsError is what CheckPermissions returns.
	PermissionsError error

	mu          sync.Mutex
	now         func() time.Time
	accounts    []ci.Account
	repos       map[int64][]ci.Repository
	files       map[string]string
	changed     map[string]ci.ChangedFiles
	pulls       map[string]ci.PullRequestState
	branches    map[string]string
	permissions map[string]ci.Permission
	reports     map[ci.ReportID]*Report
	suites      map[string]int64
	nextID      int64
	tokens      []TokenRequest
	reactions   []Reaction
	comments    []Comment
	errs        map[string]error
	errTimes    map[string]int
	fileErrs    map[string]error
	reads       []string
}

var (
	_ ci.CodeHost     = (*Host)(nil)
	_ ci.Installation = (*installation)(nil)
)

// NewHost returns an empty code host whose tokens expire an hour after now.
func NewHost(now func() time.Time) *Host {
	return &Host{
		now: now, repos: map[int64][]ci.Repository{}, files: map[string]string{}, changed: map[string]ci.ChangedFiles{},
		pulls: map[string]ci.PullRequestState{}, branches: map[string]string{}, permissions: map[string]ci.Permission{},
		reports: map[ci.ReportID]*Report{}, suites: map[string]int64{}, errs: map[string]error{}, errTimes: map[string]int{}, fileErrs: map[string]error{},
		nextID: 1000,
	}
}

// Fail makes every later call of the named method (e.g. "ReadFile", "OpenReport") fail with err;
// a nil err makes it succeed again.
func (h *Host) Fail(method string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs[method] = err
	delete(h.errTimes, method)
}

// FailNext makes the next n calls of the named method fail with err, and the later ones succeed.
func (h *Host) FailNext(method string, err error, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs[method], h.errTimes[method] = err, n
}

func (h *Host) failure(method string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, counted := h.errTimes[method]
	if !counted {
		return h.errs[method]
	}
	if n == 0 {
		return nil
	}
	h.errTimes[method] = n - 1
	return h.errs[method]
}

// AddAccount installs the App on an account with its repositories.
func (h *Host) AddAccount(installationID int64, login string, repos ...ci.Repository) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.accounts = append(h.accounts, ci.Account{InstallationID: installationID, Login: login})
	h.repos[installationID] = append(h.repos[installationID], repos...)
}

// SetFile stores a file of a repository at ref.
func (h *Host) SetFile(repo ci.Repository, ref, path, content string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files[repo.FullName+"@"+ref+":"+path] = content
}

// FailFile makes reading one file at ref fail with err; a nil err makes it succeed again.
func (h *Host) FailFile(repo ci.Repository, ref, path string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fileErrs[repo.FullName+"@"+ref+":"+path] = err
}

// Reads lists every file read so far, as "owner/name@ref:path", in order.
func (h *Host) Reads() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reads)
}

// SetPullRequest stores a pull request.
func (h *Host) SetPullRequest(repo ci.Repository, pr ci.PullRequestState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pulls[fmt.Sprintf("%s#%d", repo.FullName, pr.Number)] = pr
}

// SetBranch points a branch at a commit; "" deletes it.
func (h *Host) SetBranch(repo ci.Repository, branch, sha string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sha == "" {
		delete(h.branches, repo.FullName+":"+branch)
		return
	}
	h.branches[repo.FullName+":"+branch] = sha
}

// SetPermission sets a user's permission on a repository.
func (h *Host) SetPermission(repo ci.Repository, user string, p ci.Permission) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.permissions[repo.FullName+":"+user] = p
}

// SetPullRequestFiles sets the files a pull request changes.
func (h *Host) SetPullRequestFiles(repo ci.Repository, number int, files ci.ChangedFiles) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changed[fmt.Sprintf("%s#%d", repo.FullName, number)] = files
}

// SetComparison sets the files changed between two commits.
func (h *Host) SetComparison(repo ci.Repository, base, head string, files ci.ChangedFiles) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changed[repo.FullName+":"+base+"..."+head] = files
}

// AddReport stores an existing report and returns its ID.
func (h *Host) AddReport(repo ci.Repository, r ci.Report) ci.ReportID {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addLocked(repo, r)
}

func (h *Host) addLocked(repo ci.Repository, r ci.Report) ci.ReportID {
	h.nextID++
	id := ci.ReportID(h.nextID)
	if r.Status == "" {
		r.Status = ci.StatusQueued
	}
	if r.Conclusion != "" {
		r.Status = ci.StatusCompleted
	}
	h.reports[id] = &Report{Report: r, ID: id, Repository: repo.FullName, SuiteID: h.suiteLocked(repo.FullName, r.Revision)}
	return id
}

func (h *Host) suiteLocked(repo, revision string) int64 {
	key := repo + "@" + revision
	if id, ok := h.suites[key]; ok {
		return id
	}
	h.nextID++
	h.suites[key] = h.nextID
	return h.nextID
}

// SuiteID returns the suite of a repository's commit.
func (h *Host) SuiteID(repo ci.Repository, revision string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.suiteLocked(repo.FullName, revision)
}

// Reports returns every report, oldest first.
func (h *Host) Reports() []Report {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Report, 0, len(h.reports))
	for _, r := range h.reports {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Report) int { return int(a.ID - b.ID) })
	return out
}

// ReportsNamed returns the reports with a name, oldest first.
func (h *Host) ReportsNamed(name string) []Report {
	var out []Report
	for _, r := range h.Reports() {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

// Report returns one report.
func (h *Host) Report(id ci.ReportID) (Report, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.reports[id]
	if !ok {
		return Report{}, false
	}
	return *r, true
}

// TokenRequests returns the tokens minted.
func (h *Host) TokenRequests() []TokenRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.tokens)
}

// Reactions returns the reactions added.
func (h *Host) Reactions() []Reaction {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reactions)
}

// Comments returns the comments posted.
func (h *Host) Comments() []Comment {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.comments)
}

// Accounts lists the accounts the App is installed on.
func (h *Host) Accounts(context.Context) ([]ci.Account, error) {
	if err := h.failure("Accounts"); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.accounts), nil
}

// Installation returns the host as an installation sees it.
func (h *Host) Installation(id int64) ci.Installation { return &installation{h: h, id: id} }

// RepositoryToken mints a token and records the request.
func (h *Host) RepositoryToken(_ context.Context, installationID, repositoryID int64, permissions map[string]string) (ci.Token, error) {
	if err := h.failure("RepositoryToken"); err != nil {
		return ci.Token{}, err
	}
	return h.mint(installationID, repositoryID, permissions), nil
}

// InstallationToken mints a token for every repository and records the request (RepositoryID 0).
func (h *Host) InstallationToken(_ context.Context, installationID int64, permissions map[string]string) (ci.Token, error) {
	if err := h.failure("InstallationToken"); err != nil {
		return ci.Token{}, err
	}
	return h.mint(installationID, 0, permissions), nil
}

func (h *Host) mint(installationID, repositoryID int64, permissions map[string]string) ci.Token {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens = append(h.tokens, TokenRequest{InstallationID: installationID, RepositoryID: repositoryID, Permissions: permissions})
	return ci.Token{Value: fmt.Sprintf("token-%d", len(h.tokens)), ExpiresAt: h.now().Add(time.Hour), Permissions: permissions}
}

// CheckPermissions returns PermissionsError.
func (h *Host) CheckPermissions(map[string]string) error { return h.PermissionsError }

type installation struct {
	h  *Host
	id int64
}

func (c *installation) Repositories(context.Context) ([]ci.Repository, error) {
	if err := c.h.failure("Repositories"); err != nil {
		return nil, err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	return slices.Clone(c.h.repos[c.id]), nil
}

func (c *installation) ReadFile(_ context.Context, repo ci.Repository, path, ref string) ([]byte, error) {
	if err := c.h.failure("ReadFile"); err != nil {
		return nil, err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	c.h.reads = append(c.h.reads, repo.FullName+"@"+ref+":"+path)
	if err := c.h.fileErrs[repo.FullName+"@"+ref+":"+path]; err != nil {
		return nil, err
	}
	content, ok := c.h.files[repo.FullName+"@"+ref+":"+path]
	if !ok {
		return nil, ci.ErrNotFound
	}
	return []byte(content), nil
}

func (c *installation) changed(key string) (ci.ChangedFiles, error) {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	files, ok := c.h.changed[key]
	if !ok {
		return ci.ChangedFiles{}, fmt.Errorf("no changes recorded for %s", key)
	}
	return files, nil
}

func (c *installation) PullRequestFiles(_ context.Context, repo ci.Repository, number int) (ci.ChangedFiles, error) {
	return c.changed(fmt.Sprintf("%s#%d", repo.FullName, number))
}

func (c *installation) CompareFiles(_ context.Context, repo ci.Repository, base, head string) (ci.ChangedFiles, error) {
	return c.changed(repo.FullName + ":" + base + "..." + head)
}

func (c *installation) PullRequest(_ context.Context, repo ci.Repository, number int) (ci.PullRequestState, error) {
	if err := c.h.failure("PullRequest"); err != nil {
		return ci.PullRequestState{}, err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	pr, ok := c.h.pulls[fmt.Sprintf("%s#%d", repo.FullName, number)]
	if !ok {
		return ci.PullRequestState{}, ci.ErrNotFound
	}
	return pr, nil
}

func (c *installation) BranchHead(_ context.Context, repo ci.Repository, branch string) (string, error) {
	if err := c.h.failure("BranchHead"); err != nil {
		return "", err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	sha, ok := c.h.branches[repo.FullName+":"+branch]
	if !ok {
		return "", ci.ErrNotFound
	}
	return sha, nil
}

func (c *installation) Permission(_ context.Context, repo ci.Repository, user string) (ci.Permission, error) {
	if err := c.h.failure("Permission"); err != nil {
		return "", err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if p, ok := c.h.permissions[repo.FullName+":"+user]; ok {
		return p, nil
	}
	return "none", nil
}

func (c *installation) OpenReport(ctx context.Context, repo ci.Repository, r ci.Report) (ci.ReportID, error) {
	// As GitHub's client does, a report fails with its context.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := c.h.failure("OpenReport"); err != nil {
		return 0, err
	}
	if r.Name == "" || r.Revision == "" {
		return 0, errors.New("a report needs a name and a revision")
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	return c.h.addLocked(repo, r), nil
}

// UpdateReport changes the report's non-zero fields, as GitHub does: its output (title, summary,
// text and trigger) is replaced as a whole when any of it is set.
func (c *installation) UpdateReport(ctx context.Context, repo ci.Repository, id ci.ReportID, u ci.Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.h.failure("UpdateReport"); err != nil {
		return err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	r, ok := c.h.reports[id]
	if !ok || r.Repository != repo.FullName {
		return ci.ErrNotFound
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&r.Name, u.Name)
	set(&r.URL, u.URL)
	set(&r.ExternalID, u.ExternalID)
	if u.Status != "" {
		r.Status = u.Status
	}
	if u.Conclusion != "" {
		r.Status, r.Conclusion = ci.StatusCompleted, u.Conclusion
	}
	if !u.Started.IsZero() {
		r.Started = u.Started
	}
	if !u.Completed.IsZero() {
		r.Completed = u.Completed
	}
	if u.Actions != nil {
		r.Actions = u.Actions
	}
	if u.Title != "" || u.Summary != "" || u.Text != "" || u.Trigger != nil {
		r.Title, r.Summary, r.Text, r.Trigger = u.Title, u.Summary, u.Text, u.Trigger
	}
	r.Updates++
	return nil
}

func (c *installation) FindReport(_ context.Context, repo ci.Repository, revision, name, externalID string) (ci.ReportID, error) {
	if err := c.h.failure("FindReport"); err != nil {
		return 0, err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	var found ci.ReportID
	for id, r := range c.h.reports {
		if r.Repository == repo.FullName && r.Revision == revision && r.Name == name && r.ExternalID == externalID && id > found {
			found = id
		}
	}
	return found, nil
}

func (c *installation) ReportTrigger(_ context.Context, repo ci.Repository, id ci.ReportID) (*ci.Trigger, error) {
	if err := c.h.failure("ReportTrigger"); err != nil {
		return nil, err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	r, ok := c.h.reports[id]
	if !ok || r.Repository != repo.FullName {
		return nil, ci.ErrNotFound
	}
	if r.Trigger == nil {
		return nil, nil
	}
	t := *r.Trigger
	return &t, nil
}

// SuiteReports lists the latest report of each name in a suite, without their triggers: the
// services must read those with ReportTrigger.
func (c *installation) SuiteReports(_ context.Context, repo ci.Repository, suiteID int64) ([]ci.ReportRef, error) {
	if err := c.h.failure("SuiteReports"); err != nil {
		return nil, err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	latest := map[string]*Report{}
	for _, r := range c.h.reports {
		if r.SuiteID == suiteID && r.Repository == repo.FullName && (latest[r.Name] == nil || r.ID > latest[r.Name].ID) {
			latest[r.Name] = r
		}
	}
	var refs []ci.ReportRef
	for _, r := range latest {
		refs = append(refs, ci.ReportRef{ID: r.ID, Name: r.Name, Revision: r.Revision, Conclusion: r.Conclusion})
	}
	slices.SortFunc(refs, func(a, b ci.ReportRef) int { return int(a.ID - b.ID) })
	return refs, nil
}

func (c *installation) React(_ context.Context, repo ci.Repository, commentID int64, reaction string) error {
	if err := c.h.failure("React"); err != nil {
		return err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	c.h.reactions = append(c.h.reactions, Reaction{Repository: repo.FullName, CommentID: commentID, Reaction: reaction})
	return nil
}

func (c *installation) Comment(_ context.Context, repo ci.Repository, number int, body string) error {
	if err := c.h.failure("Comment"); err != nil {
		return err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	c.h.comments = append(c.h.comments, Comment{Repository: repo.FullName, Number: number, Body: body})
	return nil
}

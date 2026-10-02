package ci

import (
	"context"
	"time"
)

// CodeHost is where repositories live and runs are reported: GitHub, through the Octomaton App.
type CodeHost interface {
	// Accounts lists the accounts the App is installed on and serves.
	Accounts(ctx context.Context) ([]Account, error)
	// Installation returns the code host as one installation of the App sees it.
	Installation(id int64) Installation
	// RepositoryToken mints a short-lived token for one repository with the given permissions.
	RepositoryToken(ctx context.Context, installationID, repositoryID int64, permissions map[string]string) (Token, error)
	// InstallationToken mints a short-lived token for every repository of the installation with the
	// given permissions.
	InstallationToken(ctx context.Context, installationID int64, permissions map[string]string) (Token, error)
	// CheckPermissions reports token permissions the code host cannot grant.
	CheckPermissions(permissions map[string]string) error
}

// Account is an account the App is installed on.
type Account struct {
	InstallationID int64
	Login          string
}

// Installation is the code host as one installation of the App sees it.
type Installation interface {
	// Repositories lists the installation's repositories that can run pipelines (not archived).
	Repositories(ctx context.Context) ([]Repository, error)
	// ReadFile returns a file at ref (the repository's default branch when empty), or ErrNotFound.
	ReadFile(ctx context.Context, repo Repository, path, ref string) ([]byte, error)
	// PullRequestFiles lists the files a pull request changes.
	PullRequestFiles(ctx context.Context, repo Repository, number int) (ChangedFiles, error)
	// CompareFiles lists the files changed between two commits.
	CompareFiles(ctx context.Context, repo Repository, base, head string) (ChangedFiles, error)
	// PullRequest returns a pull request as it is now, or ErrNotFound.
	PullRequest(ctx context.Context, repo Repository, number int) (PullRequestState, error)
	// BranchHead returns the commit a branch points at, or ErrNotFound.
	BranchHead(ctx context.Context, repo Repository, branch string) (string, error)
	// Permission returns a user's permission on a repository.
	Permission(ctx context.Context, repo Repository, user string) (Permission, error)

	// OpenReport creates a report.
	OpenReport(ctx context.Context, repo Repository, r Report) (ReportID, error)
	// UpdateReport changes a report's non-zero fields.
	UpdateReport(ctx context.Context, repo Repository, id ReportID, r Report) error
	// FindReport returns the newest of the App's reports on revision with the given name and
	// external ID, or 0 when there is none.
	FindReport(ctx context.Context, repo Repository, revision, name, externalID string) (ReportID, error)
	// ReportTrigger returns the trigger stored with a report; nil when it has none.
	ReportTrigger(ctx context.Context, repo Repository, id ReportID) (*Trigger, error)
	// SuiteReports lists the latest report of each name in a suite of reports.
	SuiteReports(ctx context.Context, repo Repository, suiteID int64) ([]ReportRef, error)

	// React adds a reaction ("eyes", "-1", …) to a pull request comment.
	React(ctx context.Context, repo Repository, commentID int64, reaction string) error
	// Comment posts a comment on a pull request.
	Comment(ctx context.Context, repo Repository, number int, body string) error
}

// ChangedFiles are the paths an event changes. Complete is false when the code host cut the list
// short; an incomplete list must not be used to skip work.
type ChangedFiles struct {
	Files    []string
	Complete bool
}

// PullRequestState is a pull request as the code host has it now.
type PullRequestState struct {
	PullRequest
	// State is "open" or "closed".
	State string
	Draft bool
}

// Permission is a user's access to a repository: admin, maintain, write, triage, read or none.
type Permission string

// CanWrite reports whether the permission allows pushing to the repository.
func (p Permission) CanWrite() bool {
	return p == "admin" || p == "maintain" || p == "write"
}

// Runner is the CI system that runs pipelines: Tekton, on Kubernetes. A run is created held and
// starts when it is released.
type Runner interface {
	// Check reports why spec cannot be run, as a *Refusal, before anything is created.
	Check(ctx context.Context, spec RunSpec) error
	// Create creates attempt of spec's run, held. It returns ErrExists, with the run holding the
	// attempt's name when it can read it, when the attempt exists; a *Refusal when the runner
	// refuses the run.
	Create(ctx context.Context, spec RunSpec, attempt int) (Run, error)
	// Get returns a run, or ErrNotFound.
	Get(ctx context.Context, id RunID) (Run, error)
	// List returns the runs q selects.
	List(ctx context.Context, q RunQuery) ([]Run, error)
	// Release lets a held run start.
	Release(ctx context.Context, id RunID) error
	// Cancel stops a run, recording why.
	Cancel(ctx context.Context, id RunID, why Cancellation) error
	// Record writes down what was reported about a run.
	Record(ctx context.Context, id RunID, r Record) error

	// SetToken stores a run's code-host token where the run reads it, replacing any older one.
	SetToken(ctx context.Context, run Run, t Token) error
	// TokenExpiry returns when a run's stored token expires; ok is false when it has none.
	TokenExpiry(ctx context.Context, id RunID) (expires time.Time, ok bool, err error)

	// Details returns what a run's tasks did.
	Details(ctx context.Context, id RunID) (Details, error)
	// StepLogs returns the last lines of a step's logs, at most limitBytes of them.
	StepLogs(ctx context.Context, id RunID, step Step, tailLines, limitBytes int64) (string, error)
	// Link tells where people see a run.
	Link(id RunID) RunLink
	// TaskURL is where people see one task of a run; "" when the runner has no dashboard.
	TaskURL(id RunID, task string) string

	// FreeResources releases what runs that finished before the given time still hold (their
	// volumes), and returns how many resources were freed.
	FreeResources(ctx context.Context, finishedBefore time.Time) (int, error)
	// Watch hands w every live run, again whenever it changes and after the delay w asks for,
	// until ctx ends.
	Watch(ctx context.Context, w Watcher) error
}

// Watcher follows runs: Runner.Watch calls it.
type Watcher interface {
	// Reconcile brings everything up to date with a run. It returns when to look at the run again
	// (zero: when it changes).
	Reconcile(ctx context.Context, run Run) (again time.Duration, err error)
	// Deleted is told about a run deleted before it was let go.
	Deleted(ctx context.Context, run Run) error
}

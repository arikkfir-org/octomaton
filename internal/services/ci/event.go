package ci

// Event is a code-host delivery decoded into what Octomaton acts on: a *TriggerEvent, a
// *MergeGroupDestroyed, a *CommandEvent or a *RerunEvent. Its String describes it in logs.
type Event interface {
	String() string
	event()
}

// TriggerEvent is a push, a pull request event, a review request or a merge group ready for checks:
// it runs the pipelines it matches.
type TriggerEvent struct {
	Trigger Trigger
	// Draft is set for events of draft pull requests.
	Draft bool
	// PendingReviewers, for new commits on a pull request (synchronize), are the users a review is still
	// requested from: each of their requests runs again at the new head.
	PendingReviewers []string
}

// MergeGroupDestroyed cancels the unfinished runs of a merge group the queue dropped.
type MergeGroupDestroyed struct {
	Trigger Trigger
	// Reason is why the queue dropped the group, e.g. "dequeued" or "merged".
	Reason string
}

// CommandEvent is a pull request comment that may be a command, such as "/deploy staging".
type CommandEvent struct {
	InstallationID int64
	Repository     Repository
	// Number is the pull request's.
	Number    int
	CommentID int64
	Author    string
	// Line is the comment's first line, trimmed.
	Line       string
	DeliveryID string
}

// RerunEvent asks to run reports again: a report's re-run, or the re-run of a whole suite of reports.
type RerunEvent struct {
	InstallationID int64
	Repository     Repository
	// Reports to run again; ignored when SuiteID is set.
	Reports []ReportRef
	// SuiteID runs the latest report of every name in the suite again.
	SuiteID int64
	// Requester asked for it; they need write access to the repository.
	Requester  string
	DeliveryID string
}

func (e *TriggerEvent) String() string {
	t := e.Trigger
	return t.Event + " " + t.Repository.FullName + "@" + ShortSHA(t.Revision)
}

func (e *MergeGroupDestroyed) String() string {
	return "merge_group destroyed " + e.Trigger.Repository.FullName
}

func (e *CommandEvent) String() string { return "comment " + e.Repository.FullName }

func (e *RerunEvent) String() string { return "re-run " + e.Repository.FullName }

func (*TriggerEvent) event()        {}
func (*MergeGroupDestroyed) event() {}
func (*CommandEvent) event()        {}
func (*RerunEvent) event()          {}

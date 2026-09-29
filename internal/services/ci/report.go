package ci

import "time"

// ReportID identifies a report on the code host.
type ReportID int64

// Status is where a report is: queued, in progress or completed.
type Status string

// Report statuses.
const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// Conclusion is how a completed report, or a finished run, ended.
type Conclusion string

// Conclusions.
const (
	Success        Conclusion = "success"
	Failure        Conclusion = "failure"
	Cancelled      Conclusion = "cancelled"
	TimedOut       Conclusion = "timed_out"
	Skipped        Conclusion = "skipped"
	ActionRequired Conclusion = "action_required"
)

// ApproveAction identifies the report action that approves running an untrusted pull request's
// pipelines.
const ApproveAction = "approve"

// Report is the status of a pipeline (or of one of its tasks) shown on the code host, next to the
// commit it ran for. Zero fields are left as they are when a report is updated.
type Report struct {
	Name     string
	Revision string
	Status   Status
	// Conclusion is set when Status is StatusCompleted.
	Conclusion Conclusion
	Title      string
	Summary    string
	Text       string
	// URL is where people follow the run.
	URL string
	// ExternalID ties the report to a run (RunID.String()).
	ExternalID string
	Started    time.Time
	Completed  time.Time
	Actions    []Action
	// Trigger is stored with the report, so it can be run again after its run is gone; nil stores
	// nothing.
	Trigger *Trigger
}

// Action is a button on a report.
type Action struct {
	Label       string
	Description string
	// ID is what the code host sends back when someone presses it.
	ID string
}

// ReportRef is a report an event names, such as one someone asked to re-run.
type ReportRef struct {
	ID         ReportID
	Name       string
	Revision   string
	Conclusion Conclusion
	// Trigger is the trigger stored with the report, when the event carried it; when nil, read it
	// with Installation.ReportTrigger.
	Trigger *Trigger
}

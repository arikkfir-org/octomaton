package ci

import "time"

// RunID identifies a run.
type RunID struct {
	// Tenant is where the runner keeps the repository's runs.
	Tenant string
	Name   string
}

func (id RunID) String() string { return id.Tenant + "/" + id.Name }

// Phase is where a run is in its life.
type Phase string

// Phases.
const (
	// Held runs wait to be released: they are being started, or wait for their turn.
	Held Phase = "held"
	// Released runs were let go; the runner has not started them yet.
	Released Phase = "released"
	Running  Phase = "running"
	Finished Phase = "finished"
)

// Policy decides how runs of one concurrency group run together.
type Policy string

// Concurrency policies.
const (
	// Supersede keeps only the newest run of the group going.
	Supersede Policy = "supersede"
	// Queue runs the group's runs one at a time, oldest first.
	Queue Policy = "queue"
	// Latest runs one at a time, and of the waiting runs only the newest.
	Latest Policy = "latest"
)

// Concurrency is the concurrency group a run belongs to.
type Concurrency struct {
	// Group is the group's name ("" = the run is unconstrained).
	Group string
	// Key identifies the group within the repository.
	Key    string
	Policy Policy
}

// TokenSettings asks for a short-lived code-host token for the run's repository.
type TokenSettings struct {
	// Workspace is where the run reads the token.
	Workspace   string            `json:"workspace"`
	Permissions map[string]string `json:"permissions"`
}

// Token is a short-lived code-host token for one repository.
type Token struct {
	Value       string
	ExpiresAt   time.Time
	Permissions map[string]string
}

// Cancellation is why Octomaton stopped a run. The run's report says it.
type Cancellation struct {
	// Reason is why the run was cancelled, e.g. "merge group destroyed".
	Reason string
	// SupersededBy names the newer run of the same tenant that replaced it.
	SupersededBy string
	// NewerCommit is the commit that replaced the run's at the head of its branch or pull request.
	NewerCommit string
}

// Superseded reports whether a newer run or commit replaced the run.
func (c Cancellation) Superseded() bool { return c.SupersededBy != "" || c.NewerCommit != "" }

// Outcome is how a finished run ended, as the runner tells it.
type Outcome struct {
	// Conclusion is Success, Failure, Cancelled or TimedOut.
	Conclusion Conclusion
	// Message explains a conclusion other than Success.
	Message string
}

// Reported is how far a run's report has been brought.
type Reported string

// What was reported about a run. A run is created with nothing reported.
const (
	ReportedQueued     Reported = "queued"
	ReportedInProgress Reported = "in_progress"
	// ReportedConcluded runs have their report completed; a comment command is still to be answered.
	ReportedConcluded Reported = "concluded"
	// ReportedCompleted runs are fully reported.
	ReportedCompleted Reported = "completed"
)

// Run is one attempt at running a pipeline for a trigger.
type Run struct {
	ID RunID
	// Trigger is what the run was started for; Trigger.Pipeline names its pipeline.
	Trigger Trigger
	// Attempt counts the runs of the pipeline at the trigger's commit.
	Attempt int
	// Group identifies the run's concurrency group among the repository's runs; "" when the run
	// is unconstrained.
	Group  string
	Policy Policy
	// Token, when set, has the run read a code-host token.
	Token *TokenSettings
	// TaskReports is set when each task of the run is reported on its own.
	TaskReports bool
	// Tasks lists the pipeline's tasks in order, when the runner knows them.
	Tasks []string

	Phase Phase
	// CancelRequested is set when the run was asked to stop; it may still be finishing.
	CancelRequested bool
	// Deleting is set when the run is being deleted.
	Deleting     bool
	Cancellation Cancellation
	// Outcome is set when the run finished.
	Outcome  Outcome
	Created  time.Time
	Started  time.Time
	Finished time.Time

	// What was reported, as recorded with Runner.Record.
	ReportID         ReportID
	Reported         Reported
	Progress         string
	TaskReportIDs    map[string]ReportID
	TaskReportStates map[string]Status
	// WaitingFor names a run of the same commit this held run waits for.
	WaitingFor string
	// Done is set when Octomaton let the run go: it has nothing more to do with it.
	Done bool
}

// RunSpec is a run to create.
type RunSpec struct {
	// Trigger is what the run is for; Trigger.Pipeline names the pipeline.
	Trigger Trigger
	// Definition is the pipeline's definition as read from the repository, at Path.
	Definition []byte
	Path       string
	// Params set or override the definition's parameters.
	Params map[string]string
	// Timeout, when positive, bounds the run.
	Timeout time.Duration
	Token   *TokenSettings
	// Secrets name the Secrets in the run's namespace it may mount besides its token.
	Secrets     []string
	TaskReports bool
	Concurrency Concurrency
}

// RunQuery selects runs. Zero fields select everything.
type RunQuery struct {
	// Repository limits the query to one repository's runs.
	Repository *Repository
	Pipeline   string
	Revision   string
	Event      string
	// Group is a concurrency group, as in Run.Group; it needs Repository.
	Group string
	// Slot limits the query to the runs of one schedule slot.
	Slot time.Time
	// Live limits the query to runs not let go yet.
	Live bool
}

// Record is what Octomaton writes down about a run as it reports it. Nil fields are left as they are.
type Record struct {
	ReportID *ReportID
	Reported *Reported
	Progress *string
	// TaskReportIDs replaces the recorded map when not nil.
	TaskReportIDs map[string]ReportID
	// TaskReportStates replaces the recorded map when not nil.
	TaskReportStates map[string]Status
	WaitingFor       *string
	// Done lets the run go.
	Done bool
}

// TaskState is where one task of a run is.
type TaskState string

// Task states.
const (
	TaskPending   TaskState = "pending"
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
	TaskCancelled TaskState = "cancelled"
	TaskTimedOut  TaskState = "timed_out"
	TaskSkipped   TaskState = "skipped"
)

// Finished reports whether a task in this state is over.
func (s TaskState) Finished() bool { return s != TaskPending && s != TaskRunning }

// Task is one task of a run.
type Task struct {
	Name  string
	State TaskState
	// Note says why a task was skipped.
	Note     string
	Started  time.Time
	Finished time.Time
	// Message says why a task failed or timed out.
	Message string
	// FailedSteps are the task's steps that exited with an error.
	FailedSteps []Step
	Results     map[string]string
}

// Step is one step of a task.
type Step struct {
	Name     string
	ExitCode int32
	// Logs locates the step's logs for Runner.StepLogs; "" when there are none.
	Logs string
}

// Details are what a run's tasks did.
type Details struct {
	// Tasks are in pipeline order, followed by tasks outside it (such as finally tasks).
	Tasks []Task
	// Results are the run's results, completed with the results of the tasks they name.
	Results map[string]string
}

// RunLink is how people find a run.
type RunLink struct {
	// Kind is what the runner calls a run, e.g. "PipelineRun".
	Kind string
	// Name identifies the run to people.
	Name string
	// URL shows the run; "" when the runner has no dashboard.
	URL string
}

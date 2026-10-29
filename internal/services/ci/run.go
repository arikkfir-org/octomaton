package ci

import (
	"context"
	"time"
)

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

// TokenSettings asks for a short-lived code-host token for the run's repository, or for every
// repository of its installation.
type TokenSettings struct {
	// Workspace is where the run reads the token.
	Workspace   string            `json:"workspace"`
	Permissions map[string]string `json:"permissions"`
	// AllRepositories asks for every repository of the installation, not just the run's.
	AllRepositories bool `json:"allRepositories,omitempty"`
}

// Mint mints the token s asks for, for a run of t.
func (s *TokenSettings) Mint(ctx context.Context, host CodeHost, t Trigger) (Token, error) {
	if s.AllRepositories {
		return host.InstallationToken(ctx, t.InstallationID, s.Permissions)
	}
	return host.RepositoryToken(ctx, t.InstallationID, t.Repository.ID, s.Permissions)
}

// Token is a short-lived code-host token.
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
	// Secrets, when set, are Secrets of another namespace the run mounts as copies of its own.
	Secrets *SecretSource
	// TaskReports is set when each task of the run is reported on its own.
	TaskReports bool
	// LiveTaskReports is set when a running task's report shows the title and summary the task
	// prints (Runner.TaskOutput).
	LiveTaskReports bool
	// Reruns, when set, has the run rerun when it fails only because tasks lost their pods to
	// disruptions (Task.Disruption).
	Reruns *Reruns
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
	// Definition is the pipeline's definition as read from the repository, at Path, self-contained
	// once Runner.Expand inlined the files it references.
	Definition []byte
	Path       string
	// Params set or override the definition's parameters.
	Params map[string]string
	// Timeout, when positive, bounds the run.
	Timeout time.Duration
	Token   *TokenSettings
	// Secrets name the Secrets the run may mount besides its token: Secrets of the run's namespace,
	// or, with SecretsFrom, of that repository's namespace.
	Secrets []string
	// SecretsFrom, when set, is the repository whose namespace holds the Secrets: an organization
	// pipeline's organization repository. The runner copies them into the run's namespace, owned by
	// the run, and the run mounts the copies.
	SecretsFrom     *Repository
	TaskReports     bool
	LiveTaskReports bool
	Concurrency     Concurrency
	// Reruns, when set, has the run rerun when it fails only because tasks lost their pods to
	// disruptions.
	Reruns *Reruns
	// ReportID and TaskReportIDs are reports of an earlier attempt the run takes over (a rerun's),
	// rather than open its own.
	ReportID      ReportID
	TaskReportIDs map[string]ReportID
}

// Reruns is how a run reruns when it fails only because tasks lost their pods to disruptions:
// the next attempt takes over its reports, which stay open.
type Reruns struct {
	// Left is how many more times the run may be rerun.
	Left int `json:"left"`
	// Of names the run this one reruns; "" for the first attempt.
	Of string `json:"of,omitempty"`
	// Lost lists the earlier attempts that lost tasks, oldest first.
	Lost []LostAttempt `json:"lost,omitempty"`
}

// LostAttempt is an attempt that failed only because tasks lost their pods to disruptions.
type LostAttempt struct {
	Attempt int          `json:"attempt"`
	Run     string       `json:"run"`
	Tasks   []Disruption `json:"tasks"`
}

// Disruption is how the cluster took a task's pod away while the task ran: its machine shut down
// or went away, or the pod was evicted or preempted. The task did not fail by itself.
type Disruption struct {
	Task string `json:"task"`
	// Node is the machine the pod ran on.
	Node string `json:"node,omitempty"`
	// Reason is the cluster's, e.g. TerminationByKubelet.
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// SecretSource returns the Secrets a run of the spec gets copies of, or nil when its Secrets are its
// own namespace's.
func (s RunSpec) SecretSource() *SecretSource {
	if s.SecretsFrom == nil || len(s.Secrets) == 0 {
		return nil
	}
	return &SecretSource{Repository: *s.SecretsFrom, Names: s.Secrets}
}

// SecretSource is where a run's Secrets come from when they are not its own namespace's: the runner
// copies each into the run's namespace as a Secret owned by the run, named after the run, the
// Secret and Nonce, and the run mounts the copies.
type SecretSource struct {
	// Repository is the repository whose runs' namespace holds the Secrets.
	Repository Repository `json:"repository"`
	// Names are the Secrets.
	Names []string `json:"names"`
	// Nonce is a random string in the copies' names, set by the runner when it creates the run, so
	// that no other pipeline of the namespace can name the copies: a run's name is public, and
	// a copy lives as long as its run.
	Nonce string `json:"nonce,omitempty"`
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
	// Steps are a running task's steps that started, in order.
	Steps   []Step
	Results map[string]string
	// Disruption, for a failed task of a run with Reruns, is how the cluster took its pod away; nil
	// when the task failed by itself.
	Disruption *Disruption
}

// Step is one step of a task.
type Step struct {
	Name     string
	ExitCode int32
	// Logs locates the step's logs for Runner.StepLogs and Runner.TaskOutput; "" when there are none.
	Logs string
	// Finished is when the step ended; zero while it runs.
	Finished time.Time
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

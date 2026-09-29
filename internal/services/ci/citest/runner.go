package citest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"octomaton.dev/internal/services/ci"
)

// Runner is an in-memory ci.Runner. Runs are created held in tenant "ci-<repository name>"; tests
// move them along with Start and Finish.
type Runner struct {
	// CheckError, when set, is what Check returns for a spec.
	CheckError func(ci.RunSpec) error
	// Tasks, when set, lists the tasks of the run a spec creates.
	Tasks func(ci.RunSpec) []string
	// Freed is what FreeResources reports.
	Freed int

	mu      sync.Mutex
	now     func() time.Time
	runs    map[ci.RunID]*ci.Run
	specs   map[ci.RunID]ci.RunSpec
	tokens  map[ci.RunID]ci.Token
	details map[ci.RunID]ci.Details
	logs    map[string]string
	errs    map[string]error
	created int
	freed   []time.Time
}

var _ ci.Runner = (*Runner)(nil)

// NewRunner returns a runner without runs.
func NewRunner(now func() time.Time) *Runner {
	return &Runner{
		now: now, runs: map[ci.RunID]*ci.Run{}, specs: map[ci.RunID]ci.RunSpec{}, tokens: map[ci.RunID]ci.Token{},
		details: map[ci.RunID]ci.Details{}, logs: map[string]string{}, errs: map[string]error{},
	}
}

// Fail makes every later call of the named method (e.g. "Create", "Release") fail with err; a nil
// err makes it succeed again.
func (r *Runner) Fail(method string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs[method] = err
}

func (r *Runner) failure(method string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.errs[method]
}

// Tenant is where a repository's runs are.
func Tenant(repo ci.Repository) string { return "ci-" + repo.Name }

// RunName names an attempt.
func RunName(t ci.Trigger, attempt int) string {
	return fmt.Sprintf("%s-%s-%s-%d", t.Repository.Name, t.Pipeline, ci.ShortSHA(t.Revision), attempt)
}

// Runs returns every run, in the order they were created.
func (r *Runner) Runs() []ci.Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ci.Run, 0, len(r.runs))
	for _, run := range r.runs {
		out = append(out, copyRun(run))
	}
	slices.SortFunc(out, func(a, b ci.Run) int { return a.Created.Compare(b.Created) })
	return out
}

// Named returns the run with a name.
func (r *Runner) Named(name string) (ci.Run, bool) {
	for _, run := range r.Runs() {
		if run.ID.Name == name {
			return run, true
		}
	}
	return ci.Run{}, false
}

// Spec returns the spec a run was created from.
func (r *Runner) Spec(id ci.RunID) ci.RunSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.specs[id]
}

// Token returns the token stored for a run.
func (r *Runner) Token(id ci.RunID) (ci.Token, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[id]
	return t, ok
}

// FreedBefore returns the times FreeResources was called with.
func (r *Runner) FreedBefore() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.freed)
}

// Update changes a run in place.
func (r *Runner) Update(id ci.RunID, change func(*ci.Run)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.runs[id]; ok {
		change(run)
	}
}

// Start makes a run run.
func (r *Runner) Start(id ci.RunID, at time.Time) {
	r.Update(id, func(run *ci.Run) { run.Phase, run.Started = ci.Running, at })
}

// Finish ends a run.
func (r *Runner) Finish(id ci.RunID, o ci.Outcome, at time.Time) {
	r.Update(id, func(run *ci.Run) {
		if run.Started.IsZero() {
			run.Started = at
		}
		run.Phase, run.Outcome, run.Finished = ci.Finished, o, at
	})
}

// SetDetails sets what Details returns for a run.
func (r *Runner) SetDetails(id ci.RunID, d ci.Details) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.details[id] = d
}

// SetLogs sets the logs StepLogs returns for a step's Logs locator.
func (r *Runner) SetLogs(locator, logs string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs[locator] = logs
}

// SetToken stores a token for a run (as a run's start would).
func (r *Runner) SetToken(_ context.Context, run ci.Run, t ci.Token) error {
	if err := r.failure("SetToken"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.runs[run.ID]; !ok {
		return ci.ErrNotFound
	}
	r.tokens[run.ID] = t
	return nil
}

func copyRun(run *ci.Run) ci.Run {
	c := *run
	c.TaskReportIDs = maps.Clone(run.TaskReportIDs)
	c.TaskReportStates = maps.Clone(run.TaskReportStates)
	c.Tasks = slices.Clone(run.Tasks)
	return c
}

// Check returns CheckError's verdict.
func (r *Runner) Check(_ context.Context, spec ci.RunSpec) error {
	if err := r.failure("Check"); err != nil {
		return err
	}
	if r.CheckError != nil {
		return r.CheckError(spec)
	}
	return nil
}

// Create stores a held run.
func (r *Runner) Create(_ context.Context, spec ci.RunSpec, attempt int) (ci.Run, error) {
	if err := r.failure("Create"); err != nil {
		return ci.Run{}, err
	}
	t := spec.Trigger
	id := ci.RunID{Tenant: Tenant(t.Repository), Name: RunName(t, attempt)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.runs[id]; ok {
		return copyRun(existing), ci.ErrExists
	}
	r.created++
	run := &ci.Run{
		ID: id, Trigger: t, Attempt: attempt, Policy: spec.Concurrency.Policy, Token: spec.Token, TaskReports: spec.TaskReports,
		Phase: ci.Held, Created: r.now().Add(time.Duration(r.created) * time.Millisecond),
	}
	if spec.Concurrency.Key != "" {
		run.Group = t.Repository.FullName + ":" + spec.Concurrency.Key
	}
	if r.Tasks != nil {
		run.Tasks = r.Tasks(spec)
	}
	r.runs[id], r.specs[id] = run, spec
	return copyRun(run), nil
}

// Get returns a run.
func (r *Runner) Get(_ context.Context, id ci.RunID) (ci.Run, error) {
	if err := r.failure("Get"); err != nil {
		return ci.Run{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[id]
	if !ok {
		return ci.Run{}, ci.ErrNotFound
	}
	return copyRun(run), nil
}

// List returns the runs q selects, in the order they were created.
func (r *Runner) List(_ context.Context, q ci.RunQuery) ([]ci.Run, error) {
	if err := r.failure("List"); err != nil {
		return nil, err
	}
	var out []ci.Run
	for _, run := range r.Runs() {
		t := run.Trigger
		switch {
		case q.Repository != nil && t.Repository.ID != q.Repository.ID:
		case q.Pipeline != "" && t.Pipeline != q.Pipeline:
		case q.Revision != "" && t.Revision != q.Revision:
		case q.Event != "" && t.Event != q.Event:
		case q.Group != "" && run.Group != q.Group:
		case !q.Slot.IsZero() && (t.Schedule == nil || t.Schedule.Slot != q.Slot.UTC().Format(time.RFC3339)):
		case q.Live && run.Done:
		default:
			out = append(out, run)
		}
	}
	return out, nil
}

// Release lets a held run start. Like Tekton's spec.status, it also clears a cancellation request.
func (r *Runner) Release(_ context.Context, id ci.RunID) error {
	if err := r.failure("Release"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[id]
	if !ok {
		return ci.ErrNotFound
	}
	if run.Phase == ci.Held {
		run.Phase = ci.Released
	}
	run.CancelRequested = false
	return nil
}

// Cancel asks a run to stop and records why, keeping what an earlier cancellation recorded.
func (r *Runner) Cancel(_ context.Context, id ci.RunID, why ci.Cancellation) error {
	if err := r.failure("Cancel"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[id]
	if !ok {
		return ci.ErrNotFound
	}
	run.CancelRequested = true
	if run.Phase == ci.Held {
		run.Phase = ci.Released
	}
	if why.Reason != "" {
		run.Cancellation.Reason = why.Reason
	}
	if why.SupersededBy != "" || why.NewerCommit != "" {
		run.Cancellation.SupersededBy, run.Cancellation.NewerCommit = why.SupersededBy, why.NewerCommit
	}
	return nil
}

// Record applies what was reported.
func (r *Runner) Record(_ context.Context, id ci.RunID, rec ci.Record) error {
	if err := r.failure("Record"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[id]
	if !ok {
		return ci.ErrNotFound
	}
	if rec.ReportID != nil {
		run.ReportID = *rec.ReportID
	}
	if rec.Reported != nil {
		run.Reported = *rec.Reported
	}
	if rec.Progress != nil {
		run.Progress = *rec.Progress
	}
	if rec.WaitingFor != nil {
		run.WaitingFor = *rec.WaitingFor
	}
	if rec.TaskReportIDs != nil {
		run.TaskReportIDs = maps.Clone(rec.TaskReportIDs)
	}
	if rec.TaskReportStates != nil {
		run.TaskReportStates = maps.Clone(rec.TaskReportStates)
	}
	if rec.Done {
		run.Done = true
	}
	return nil
}

// TokenExpiry returns when a run's token expires.
func (r *Runner) TokenExpiry(_ context.Context, id ci.RunID) (time.Time, bool, error) {
	if err := r.failure("TokenExpiry"); err != nil {
		return time.Time{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[id]
	return t.ExpiresAt, ok, nil
}

// Details returns what SetDetails set.
func (r *Runner) Details(_ context.Context, id ci.RunID) (ci.Details, error) {
	if err := r.failure("Details"); err != nil {
		return ci.Details{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.runs[id]; !ok {
		return ci.Details{}, ci.ErrNotFound
	}
	return r.details[id], nil
}

// StepLogs returns what SetLogs set.
func (r *Runner) StepLogs(_ context.Context, _ ci.RunID, step ci.Step, _, _ int64) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	logs, ok := r.logs[step.Logs]
	if !ok {
		return "", fmt.Errorf("no logs for step %s", step.Name)
	}
	return logs, nil
}

// Link points at runs.example.
func (r *Runner) Link(id ci.RunID) ci.RunLink {
	return ci.RunLink{Kind: "PipelineRun", Name: id.String(), URL: "https://runs.example/" + id.Tenant + "/" + id.Name}
}

// TaskURL points at runs.example.
func (r *Runner) TaskURL(id ci.RunID, task string) string { return r.Link(id).URL + "/" + task }

// FreeResources records when it was called and returns Freed.
func (r *Runner) FreeResources(_ context.Context, finishedBefore time.Time) (int, error) {
	if err := r.failure("FreeResources"); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.freed = append(r.freed, finishedBefore)
	return r.Freed, nil
}

// Watch hands w every live run once, then waits for ctx to end.
func (r *Runner) Watch(ctx context.Context, w ci.Watcher) error {
	runs, _ := r.List(ctx, ci.RunQuery{Live: true})
	for _, run := range runs {
		if _, err := w.Reconcile(ctx, run); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return nil
}

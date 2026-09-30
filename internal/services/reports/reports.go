// Package reports mirrors runs onto the code host: a run's report goes in progress when the run
// starts, shows a task table while it runs, and completes with the run's conclusion; each task's
// report follows its task; a comment command is answered. Runs held for too long are resumed, and a
// finished run lets the next one of its concurrency group go. What was reported is recorded on each
// run, so another replica continues rather than repeats. Only the leader reports.
package reports

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"octomaton.dev/internal/services/ci"
)

// defaultHeldTooLong is how long a run may be held before it is resumed: a start takes seconds.
const defaultHeldTooLong = 5 * time.Minute

// Service reports runs: it is the ci.Watcher of the Runner.
type Service struct {
	Host   ci.CodeHost
	Runner ci.Runner
	Logger *slog.Logger
	// Resume finishes starting a run held for longer than HeldTooLong (runs.Service.Resume).
	Resume func(ctx context.Context, run ci.Run) error
	// ReleaseNext lets the next held run of a finished run's group go (runs.Service.ReleaseNext).
	ReleaseNext func(ctx context.Context, run ci.Run) error
	// HeldTooLong is how long a run may be held before it is resumed (default 5m).
	HeldTooLong time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

var _ ci.Watcher = (*Service)(nil)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) heldTooLong() time.Duration {
	if s.HeldTooLong > 0 {
		return s.HeldTooLong
	}
	return defaultHeldTooLong
}

// Header links a run and describes its trigger, when the run knows it, in Markdown.
func Header(link ci.RunLink, t ci.Trigger) string {
	ref := "`" + link.Name + "`"
	if link.URL != "" {
		ref = "[" + ref + "](" + link.URL + ")"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s:** %s\n", link.Kind, ref)
	if known(t) {
		fmt.Fprintf(&b, "\n**Trigger:** %s\n", t.Describe())
	}
	return b.String()
}

// TaskName names the report of one task of a pipeline.
func TaskName(check, task string) string { return check + " / " + task }

// known reports whether a run stored its trigger, rather than just what its bookkeeping names.
func known(t ci.Trigger) bool { return t.Version == ci.TriggerVersion }

// storedTrigger is the trigger reports store, so they can be re-run; nil when the run lost it.
func storedTrigger(run ci.Run) *ci.Trigger {
	if !known(run.Trigger) {
		return nil
	}
	t := run.Trigger
	return &t
}

// reportable reports whether a run's report can be written: it was opened, and the run knows where.
func reportable(run ci.Run) bool {
	return run.ReportID != 0 && run.Trigger.InstallationID != 0 && run.Trigger.Repository.FullName != ""
}

func (s *Service) installation(run ci.Run) ci.Installation {
	return s.Host.Installation(run.Trigger.InstallationID)
}

func (s *Service) header(run ci.Run) string { return Header(s.Runner.Link(run.ID), run.Trigger) }

// Reconcile brings a run's reports up to date. It returns when to look at the run again: for held
// runs, when they will have been held too long.
func (s *Service) Reconcile(ctx context.Context, run ci.Run) (time.Duration, error) {
	switch {
	case run.Done:
		return 0, nil
	case run.Phase == ci.Finished:
		return 0, s.finish(ctx, run.ID)
	case run.Phase == ci.Held && !run.Deleting:
		// A start takes seconds. A run held longer lost the rest of its start to a restart, or waits
		// its turn in a queue: resuming does what is left and releases it when it is next.
		held := s.now().Sub(run.Created)
		if held < s.heldTooLong() {
			return s.heldTooLong() - held, nil
		}
		if s.Resume == nil {
			return 0, nil
		}
		return s.heldTooLong(), s.Resume(ctx, run)
	case run.Phase == ci.Running && run.Reported == ci.ReportedQueued && reportable(run):
		if err := s.markInProgress(ctx, run); err != nil {
			return 0, err
		}
		return 0, s.taskReports(ctx, run, nil, nil)
	case run.Phase == ci.Running && run.Reported == ci.ReportedInProgress && reportable(run):
		details, err := s.Runner.Details(ctx, run.ID)
		if err != nil {
			return 0, err
		}
		if err := s.progress(ctx, run, details); err != nil {
			return 0, err
		}
		return 0, s.taskReports(ctx, run, &details, nil)
	}
	return 0, nil
}

// markInProgress reports that a run started.
func (s *Service) markInProgress(ctx context.Context, run ci.Run) error {
	r := ci.Report{Status: ci.StatusInProgress, Started: run.Started, Title: "Running", Summary: s.header(run), Trigger: storedTrigger(run)}
	if err := s.installation(run).UpdateReport(ctx, run.Trigger.Repository, run.ReportID, r); err != nil {
		return err
	}
	return s.Runner.Record(ctx, run.ID, ci.Record{Reported: new(ci.ReportedInProgress)})
}

// progress keeps a running run's report saying what it does: for runs of two or more tasks, a
// title naming the done count, the running task and the elapsed time, and a task table. It writes
// only when the table changes.
func (s *Service) progress(ctx context.Context, run ci.Run, details ci.Details) error {
	tasks := details.Tasks
	if len(tasks) < 2 {
		return nil
	}
	done, running := 0, ""
	for _, t := range tasks {
		switch {
		case t.State.Finished():
			done++
		case t.State == ci.TaskRunning && running == "":
			running = t.Name
		}
	}
	key := fmt.Sprintf("%d/%d %s", done, len(tasks), running)
	for _, t := range tasks {
		key += " " + string(t.State)
	}
	if run.Progress == key {
		return nil
	}
	title := fmt.Sprintf("%d of %d", done, len(tasks))
	if running != "" {
		title += " · " + running
	}
	title += " · " + formatDuration(elapsed(run.Started, time.Time{}, s.now()))
	r := ci.Report{Status: ci.StatusInProgress, Title: title, Summary: s.header(run) + "\n" + table(tasks, s.now()), Trigger: storedTrigger(run)}
	if err := s.installation(run).UpdateReport(ctx, run.Trigger.Repository, run.ReportID, r); err != nil {
		return err
	}
	return s.Runner.Record(ctx, run.ID, ci.Record{Progress: &key})
}

// finish reports a finished run, in two recorded steps: its report (and task reports) completed
// and the next queued run released ("concluded"), then a comment command answered and the run let
// go ("completed"). The run is read again first: the watched copy may predate what was already
// written.
func (s *Service) finish(ctx context.Context, id ci.RunID) error {
	run, err := s.Runner.Get(ctx, id)
	if errors.Is(err, ci.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if run.Reported == ci.ReportedCompleted || !reportable(run) {
		// Fully reported, or never fully started: nothing to report.
		return s.letGo(ctx, run)
	}
	details, err := s.Runner.Details(ctx, run.ID)
	if err != nil {
		return err
	}
	o := s.outcome(ctx, run, details)
	if run.Reported != ci.ReportedConcluded {
		if err := s.conclude(ctx, run, details, o); err != nil {
			return err
		}
	}
	if err := s.reply(ctx, run, o); err != nil {
		return err
	}
	return s.letGo(ctx, run)
}

func (s *Service) conclude(ctx context.Context, run ci.Run, details ci.Details, o outcome) error {
	if err := s.taskReports(ctx, run, &details, &o); err != nil {
		return err
	}
	completed := run.Finished
	if completed.IsZero() {
		completed = s.now()
	}
	r := ci.Report{
		Status: ci.StatusCompleted, Conclusion: o.conclusion, Started: run.Started, Completed: completed,
		Title: o.title, Summary: o.summary, Text: o.text, Trigger: storedTrigger(run),
	}
	if err := s.installation(run).UpdateReport(ctx, run.Trigger.Repository, run.ReportID, r); err != nil {
		return err
	}
	if s.ReleaseNext != nil {
		if err := s.ReleaseNext(ctx, run); err != nil {
			return err
		}
	}
	if err := s.Runner.Record(ctx, run.ID, ci.Record{Reported: new(ci.ReportedConcluded)}); err != nil {
		return err
	}
	t := run.Trigger
	s.Logger.InfoContext(ctx, "Run finished", "run", run.ID.String(), "repository", t.Repository.FullName, "pipeline", t.Pipeline,
		"event", t.Event, "sha", t.Revision, "branch", t.Branch, "conclusion", o.conclusion, "url", s.Runner.Link(run.ID).URL)
	return nil
}

// letGo records a run as fully reported: Octomaton has nothing more to do with it.
func (s *Service) letGo(ctx context.Context, run ci.Run) error {
	return s.Runner.Record(ctx, run.ID, ci.Record{Reported: new(ci.ReportedCompleted), Done: true})
}

// marks show a conclusion in a comment reply.
var marks = map[ci.Conclusion]string{
	ci.Success:   "✅",
	ci.Failure:   "❌",
	ci.Cancelled: "⏹️",
	ci.Skipped:   "⏹️",
	ci.TimedOut:  "⌛",
}

// reply answers the comment command that started a run with how it ended.
func (s *Service) reply(ctx context.Context, run ci.Run, o outcome) error {
	t := run.Trigger
	if !known(t) || t.Comment == nil || t.PullRequest == nil {
		return nil
	}
	command := strings.TrimSpace(t.Comment.Command + " " + t.Comment.Arguments)
	mark := marks[o.conclusion]
	if mark == "" {
		mark = string(o.conclusion)
	}
	body := fmt.Sprintf("@%s %s `%s`: %s\n\n%s", t.Comment.Author, mark, strings.ReplaceAll(command, "`", "'"), o.title, o.summary)
	if err := s.installation(run).Comment(ctx, t.Repository, t.PullRequest.Number, body); err != nil {
		return fmt.Errorf("replying to the comment: %w", err)
	}
	return nil
}

// Deleted reports a run deleted before it was reported: its report is concluded as cancelled.
func (s *Service) Deleted(ctx context.Context, run ci.Run) error {
	if run.Reported == ci.ReportedCompleted || !reportable(run) {
		return nil
	}
	r := ci.Report{
		Status: ci.StatusCompleted, Conclusion: ci.Cancelled, Completed: s.now(), Title: "Cancelled",
		Summary: s.header(run) + fmt.Sprintf("\nThe %s was deleted before it finished.", s.Runner.Link(run.ID).Kind),
		Trigger: storedTrigger(run),
	}
	if err := s.installation(run).UpdateReport(ctx, run.Trigger.Repository, run.ReportID, r); err != nil {
		return err
	}
	s.Logger.InfoContext(ctx, "Reported a deleted run as cancelled", "run", run.ID.String())
	return nil
}

// taskReports moves each task's report with its task: in progress when it starts, completed with
// its own conclusion (and check-title / check-summary results) when it ends. At the run's end
// (final), every task report still open is concluded the way the run was. What was written is
// recorded on the run. details is read when nil and needed.
func (s *Service) taskReports(ctx context.Context, run ci.Run, details *ci.Details, final *outcome) error {
	if len(run.TaskReportIDs) == 0 {
		return nil
	}
	if details == nil {
		d, err := s.Runner.Details(ctx, run.ID)
		if err != nil {
			return err
		}
		details = &d
	}
	byName := map[string]*ci.Task{}
	for i := range details.Tasks {
		byName[details.Tasks[i].Name] = &details.Tasks[i]
	}
	states := maps.Clone(run.TaskReportStates)
	if states == nil {
		states = map[string]ci.Status{}
	}
	gh := s.installation(run)
	var errs []error
	written := false
	for _, task := range run.Tasks {
		id := run.TaskReportIDs[task]
		if id == 0 || states[task] == ci.StatusCompleted {
			continue
		}
		r := s.taskReport(run, task, byName[task], final)
		if r.Status == "" || r.Status == states[task] {
			continue
		}
		if err := gh.UpdateReport(ctx, run.Trigger.Repository, id, r); err != nil {
			errs = append(errs, err)
			continue
		}
		states[task], written = r.Status, true
	}
	if written {
		errs = append(errs, s.Runner.Record(ctx, run.ID, ci.Record{TaskReportStates: states}))
	}
	return errors.Join(errs...)
}

// taskReport is what one task's report should say now; no status when there is nothing to write.
func (s *Service) taskReport(run ci.Run, task string, t *ci.Task, final *outcome) ci.Report {
	link := ""
	if u := s.Runner.TaskURL(run.ID, task); u != "" {
		link = fmt.Sprintf("[The task on the Dashboard](%s).", u)
	}
	var start time.Time
	if t != nil {
		start = t.Started
		conclusion, title := ci.Success, "Succeeded"
		switch t.State {
		case ci.TaskSucceeded:
		case ci.TaskTimedOut:
			conclusion, title = ci.TimedOut, "Timed out"
		case ci.TaskFailed:
			conclusion, title = ci.Failure, "Failed"
		default:
			conclusion = ""
		}
		if conclusion != "" {
			var b strings.Builder
			if summary := strings.TrimSpace(t.Results[ResultSummary]); summary != "" {
				b.WriteString(summary + "\n\n")
			}
			if t.State != ci.TaskSucceeded && t.Message != "" {
				fmt.Fprintf(&b, "%s.\n\n", strings.TrimSuffix(oneLine(t.Message), "."))
			}
			b.WriteString(link)
			if custom := strings.TrimSpace(t.Results[ResultTitle]); custom != "" {
				title = custom
			}
			completed := t.Finished
			if completed.IsZero() {
				completed = s.now()
			}
			return taskReportOf(run, ci.StatusCompleted, conclusion, title, b.String(), start, completed)
		}
	}
	if final != nil {
		// The run ended without this task finishing: skipped when the run passed without it,
		// otherwise stopped with the run, or never started.
		conclusion, title := final.conclusion, "Stopped"
		summary := fmt.Sprintf("The run ended before this task did: %s.", strings.ToLower(final.title))
		switch final.conclusion {
		case ci.Success:
			conclusion, title, summary = ci.Skipped, "Skipped", "The run passed without running this task."
		case ci.Failure:
			conclusion = ci.Cancelled
		case ci.Skipped:
			title = "Superseded"
		}
		if start.IsZero() && final.conclusion != ci.Success && final.conclusion != ci.Skipped {
			title = "Not run"
		}
		return taskReportOf(run, ci.StatusCompleted, conclusion, title, summary+"\n\n"+link, start, s.now())
	}
	if !start.IsZero() {
		return taskReportOf(run, ci.StatusInProgress, "", "Running", link, start, time.Time{})
	}
	return ci.Report{}
}

func taskReportOf(run ci.Run, status ci.Status, conclusion ci.Conclusion, title, summary string, start, completed time.Time) ci.Report {
	return ci.Report{
		Status: status, Conclusion: conclusion, Title: title, Summary: summary,
		Started: start, Completed: completed, Trigger: storedTrigger(run),
	}
}

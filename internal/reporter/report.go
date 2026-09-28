package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/arikkfir-org/octomatron/internal/checkrun"
	"github.com/arikkfir-org/octomatron/internal/githubapp"
	"github.com/arikkfir-org/octomatron/internal/tekton"
	"github.com/google/go-github/v92/github"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	logTailLines   = 50
	logLimitBytes  = 8 * 1024
	maxLogSections = 5

	// ResultTitle and ResultSummary are pipeline or task results that override a
	// check's title and summary.
	ResultTitle   = "check-title"
	ResultSummary = "check-summary"
)

// Task states in the progress table.
const (
	statePending   = "pending"
	stateRunning   = "running"
	stateSucceeded = "succeeded"
	stateFailed    = "failed"
	stateCancelled = "cancelled"
	stateTimedOut  = "timed_out"
	stateSkipped   = "skipped"
)

var stateLabels = map[string]string{
	statePending:   "⬜ Pending",
	stateRunning:   "⏳ Running",
	stateSucceeded: "✅ Succeeded",
	stateFailed:    "❌ Failed",
	stateCancelled: "⏹️ Cancelled",
	stateTimedOut:  "⌛ Timed out",
	stateSkipped:   "⬜ Skipped",
}

type report struct {
	conclusion string
	title      string
	summary    string
	text       string
}

type failedStep struct {
	name      string
	container string
	exitCode  int32
}

type row struct {
	name        string
	state       string
	note        string
	duration    string
	podName     string
	message     string
	failedSteps []failedStep
}

func (r row) finished() bool {
	return r.state != statePending && r.state != stateRunning
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return d.Round(time.Second).String()
}

// Title returns a check-run title for a finished (or running) outcome.
func Title(o tekton.Outcome, d time.Duration) string {
	if !o.Done() {
		return "Running"
	}
	switch o.Conclusion {
	case tekton.ConclusionSuccess:
		return "Succeeded in " + formatDuration(d)
	case tekton.ConclusionSkipped:
		return o.Title
	default:
		return o.Title + " after " + formatDuration(d)
	}
}

// header links the PipelineRun and describes its trigger.
func (r *Reporter) header(run *unstructured.Unstructured) string {
	ns, name := run.GetNamespace(), run.GetName()
	ref := "`" + ns + "/" + name + "`"
	if u := checkrun.DashboardURL(r.DashboardURL, ns, name); u != "" {
		ref = "[" + ref + "](" + u + ")"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**PipelineRun:** %s\n", ref)
	if c, ok := contextOf(run); ok {
		fmt.Fprintf(&b, "\n**Trigger:** %s\n", c.Describe())
	}
	return b.String()
}

// textWithMarker appends the run's trigger context marker to text.
func (r *Reporter) textWithMarker(run *unstructured.Unstructured, text string) string {
	if c, ok := contextOf(run); ok {
		return checkrun.WithMarker(text, c)
	}
	return checkrun.Truncate(text, checkrun.MaxOutputLength)
}

// rows lists the run's tasks in pipeline order, then any other task (finally
// tasks, tasks of a pipeline Tekton has not resolved into the status yet).
func (r *Reporter) rows(run *unstructured.Unstructured, taskRuns []unstructured.Unstructured) []row {
	byTask := map[string]*unstructured.Unstructured{}
	var order []string
	for i := range taskRuns {
		task := taskRuns[i].GetLabels()["tekton.dev/pipelineTask"]
		if task == "" {
			continue
		}
		if prev, ok := byTask[task]; !ok || taskRuns[i].GetCreationTimestamp().Time.After(prev.GetCreationTimestamp().Time) {
			if !ok {
				order = append(order, task)
			}
			byTask[task] = &taskRuns[i]
		}
	}
	st, _ := tekton.GetPipelineRunStatus(run)
	skipped := map[string]string{}
	for _, s := range st.SkippedTasks {
		skipped[s.Name] = s.Reason
	}

	var rows []row
	seen := map[string]bool{}
	add := func(task string) {
		if seen[task] {
			return
		}
		seen[task] = true
		rw := row{name: task, state: statePending}
		switch tr := byTask[task]; {
		case tr != nil:
			r.fill(&rw, tr)
		case containsKey(skipped, task):
			rw.state, rw.note = stateSkipped, skipped[task]
		}
		rows = append(rows, rw)
	}
	for _, task := range tekton.TaskNames(run) {
		add(task)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return byTask[order[i]].GetCreationTimestamp().Time.Before(byTask[order[j]].GetCreationTimestamp().Time)
	})
	for _, task := range order {
		add(task)
	}
	for _, s := range st.SkippedTasks {
		add(s.Name)
	}
	return rows
}

func containsKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

func (r *Reporter) fill(rw *row, tr *unstructured.Unstructured) {
	ts, err := tekton.GetTaskRunStatus(tr)
	if err != nil {
		return
	}
	o := tekton.OutcomeOf(ts.Conditions)
	switch {
	case !o.Done() && ts.StartTime == nil:
		rw.state = statePending
	case !o.Done():
		rw.state = stateRunning
	case o.Conclusion == tekton.ConclusionSuccess:
		rw.state = stateSucceeded
	case o.Conclusion == tekton.ConclusionCancelled:
		rw.state = stateCancelled
	case o.Conclusion == tekton.ConclusionTimedOut:
		rw.state = stateTimedOut
	default:
		rw.state = stateFailed
	}
	if o.Done() && ts.StartTime != nil {
		rw.duration = formatDuration(tekton.Duration(ts.StartTime, ts.CompletionTime, r.now()))
	}
	rw.podName = ts.PodName
	if rw.state == stateFailed || rw.state == stateTimedOut {
		rw.message = o.Message
		for _, step := range ts.Steps {
			if step.Terminated != nil && step.Terminated.ExitCode != 0 && step.Container != "" {
				rw.failedSteps = append(rw.failedSteps, failedStep{name: step.Name, container: step.Container, exitCode: step.Terminated.ExitCode})
			}
		}
	}
}

func table(rows []row) string {
	var b strings.Builder
	b.WriteString("\n| Task | State | Time |\n| --- | --- | --- |\n")
	for _, rw := range rows {
		state := stateLabels[rw.state]
		if rw.note != "" {
			state += " (" + rw.note + ")"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", cell(rw.name), cell(state), rw.duration)
	}
	return b.String()
}

// outcome is how a finished run ended, as its check will show it.
func (r *Reporter) outcome(ctx context.Context, run *unstructured.Unstructured) (report, error) {
	ns, name := run.GetNamespace(), run.GetName()
	st, err := tekton.GetPipelineRunStatus(run)
	if err != nil {
		return report{}, err
	}
	o := tekton.RunOutcome(run, st)
	taskRuns, err := r.Runs.TaskRuns(ctx, ns, name)
	if err != nil {
		return report{}, err
	}
	rows := r.rows(run, taskRuns)
	results := tekton.Results(run)
	fromTasks(run, results, taskRuns)

	rep := report{conclusion: o.Conclusion, title: Title(o, tekton.Duration(st.StartTime, st.CompletionTime, r.now()))}
	if t := strings.TrimSpace(results[ResultTitle]); t != "" {
		rep.title = t
	}
	var b strings.Builder
	if s := strings.TrimSpace(results[ResultSummary]); s != "" {
		b.WriteString(s + "\n\n")
	}
	b.WriteString(r.header(run))
	ann := run.GetAnnotations()
	by := ann[tekton.AnnotationSupersededBy]
	switch {
	case o.Conclusion == tekton.ConclusionSkipped && strings.HasPrefix(by, "head:"):
		fmt.Fprintf(&b, "\nSuperseded by a newer commit, `%s`.\n", checkrun.ShortSHA(strings.TrimPrefix(by, "head:")))
	case o.Conclusion == tekton.ConclusionSkipped && by != "":
		link := "`" + by + "`"
		if u := checkrun.DashboardURL(r.DashboardURL, ns, by); u != "" {
			link = "[" + link + "](" + u + ")"
		}
		fmt.Fprintf(&b, "\nSuperseded by a newer run, %s.\n", link)
	case o.Conclusion == tekton.ConclusionCancelled && ann[tekton.AnnotationCancelReason] != "":
		fmt.Fprintf(&b, "\nCancelled by Octomatron: %s.\n", ann[tekton.AnnotationCancelReason])
	case o.Conclusion != tekton.ConclusionSuccess && o.Message != "":
		fmt.Fprintf(&b, "\n> %s\n", oneLine(o.Message))
	}
	var failed []string
	for _, rw := range rows {
		if rw.state == stateFailed || rw.state == stateTimedOut {
			failed = append(failed, "`"+rw.name+"`")
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\nFailed: %s.\n", strings.Join(failed, ", "))
	}
	if len(rows) > 0 {
		b.WriteString(table(rows))
	}
	rep.summary = checkrun.Truncate(b.String(), checkrun.MaxSummaryLength)

	var text string
	if o.Conclusion == tekton.ConclusionFailure || o.Conclusion == tekton.ConclusionTimedOut {
		text = r.failureLogs(ctx, ns, rows)
	}
	rep.text = r.textWithMarker(run, text)
	return rep, nil
}

// failureLogs renders the log tails of failed steps.
func (r *Reporter) failureLogs(ctx context.Context, ns string, rows []row) string {
	var b strings.Builder
	sections := 0
	for _, rw := range rows {
		if rw.state != stateFailed && rw.state != stateTimedOut {
			continue
		}
		if len(rw.failedSteps) == 0 {
			if rw.message != "" && sections < maxLogSections {
				fmt.Fprintf(&b, "### %s\n\n> %s\n\n", rw.name, oneLine(rw.message))
				sections++
			}
			continue
		}
		for _, step := range rw.failedSteps {
			if sections >= maxLogSections {
				b.WriteString("_Logs of further failed steps are omitted._\n")
				return b.String()
			}
			sections++
			fmt.Fprintf(&b, "### %s › %s (exit code %d)\n\n", rw.name, step.name, step.exitCode)
			if rw.podName == "" {
				b.WriteString("_Logs unavailable: the TaskRun has no pod._\n\n")
				continue
			}
			logs, err := r.Runs.PodLogs(ctx, ns, rw.podName, step.container, logTailLines, logLimitBytes*2)
			if err != nil {
				fmt.Fprintf(&b, "_Logs unavailable: %s_\n\n", oneLine(err.Error()))
				continue
			}
			logs = checkrun.TruncateHead(logs, logLimitBytes)
			fence := codeFence(logs)
			fmt.Fprintf(&b, "%stext\n%s\n%s\n\n", fence, strings.TrimRight(logs, "\n"), fence)
		}
	}
	return b.String()
}

// taskCheckIDs returns the check run of each task recorded on a run.
func taskCheckIDs(run *unstructured.Unstructured) map[string]int64 {
	ids := map[string]int64{}
	if raw := run.GetAnnotations()[tekton.AnnotationTaskCheckIDs]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &ids)
	}
	return ids
}

// taskChecks moves each task check with its TaskRun: in progress when it
// starts, completed with its own conclusion (and check-title / check-summary
// task results) when it ends. At the run's end (final), every task check still
// open is concluded the way the run was. What was written is recorded on the run.
func (r *Reporter) taskChecks(ctx context.Context, run *unstructured.Unstructured, final *report) error {
	ids := taskCheckIDs(run)
	if len(ids) == 0 {
		return nil
	}
	ref, ok := refOf(run)
	if !ok {
		return nil
	}
	states := map[string]string{}
	_ = json.Unmarshal([]byte(run.GetAnnotations()[tekton.AnnotationTaskCheckStates]), &states)
	taskRuns, err := r.Runs.TaskRuns(ctx, run.GetNamespace(), run.GetName())
	if err != nil {
		return err
	}
	byTask := map[string]*unstructured.Unstructured{}
	for i := range taskRuns {
		byTask[taskRuns[i].GetLabels()["tekton.dev/pipelineTask"]] = &taskRuns[i]
	}
	gh := r.GitHub.Installation(ref.installationID)
	var errs []error
	written := false
	for _, task := range tekton.TaskNames(run) {
		id := ids[task]
		if id == 0 || states[task] == tekton.StatusCompleted {
			continue
		}
		upd, state := r.taskCheck(run, task, byTask[task], final)
		if state == "" || state == states[task] {
			continue
		}
		if _, err := gh.UpdateCheckRun(ctx, ref.owner, ref.repo, id, upd); err != nil {
			r.Metrics.CheckRunErrors.WithLabelValues("update").Inc()
			errs = append(errs, err)
			continue
		}
		states[task], written = state, true
	}
	if written {
		data, err := json.Marshal(states)
		if err == nil {
			err = r.annotate(ctx, run, tekton.AnnotationTaskCheckStates, string(data))
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// taskCheck is what one task's check should say now, and the state writing it
// leaves the check in; no state when there is nothing to write yet.
func (r *Reporter) taskCheck(run *unstructured.Unstructured, task string, tr *unstructured.Unstructured, final *report) (githubapp.CheckRunUpdate, string) {
	link := ""
	if u := taskURL(r.DashboardURL, run.GetNamespace(), run.GetName(), task); u != "" {
		link = fmt.Sprintf("[The task on the Dashboard](%s).", u)
	}
	var start *metav1.Time
	if tr != nil {
		ts, _ := tekton.GetTaskRunStatus(tr)
		start = ts.StartTime
		c, ok := tekton.Succeeded(ts.Conditions)
		stopped := c.Status == "False" && (strings.Contains(c.Reason, "Cancel") || strings.Contains(c.Reason, "Stopped"))
		if ok && (c.Status == "True" || (c.Status == "False" && !stopped)) {
			conclusion, title := tekton.ConclusionSuccess, "Succeeded"
			switch {
			case c.Status == "True":
			case strings.Contains(c.Reason, "Timeout"):
				conclusion, title = tekton.ConclusionTimedOut, "Timed out"
			default:
				conclusion, title = tekton.ConclusionFailure, "Failed"
			}
			res := tekton.Results(tr)
			var b strings.Builder
			if s := strings.TrimSpace(res[ResultSummary]); s != "" {
				b.WriteString(s + "\n\n")
			}
			if c.Status == "False" && c.Message != "" {
				fmt.Fprintf(&b, "%s.\n\n", strings.TrimSuffix(oneLine(c.Message), "."))
			}
			b.WriteString(link)
			if t := strings.TrimSpace(res[ResultTitle]); t != "" {
				title = t
			}
			completed := r.now()
			if ts.CompletionTime != nil {
				completed = ts.CompletionTime.Time
			}
			return r.taskCheckUpdate(run, tekton.StatusCompleted, conclusion, title, b.String(), start, &completed), tekton.StatusCompleted
		}
	}
	if final != nil {
		// The run ended without this task finishing: skipped when the run passed
		// without it, otherwise stopped with the run or never started.
		conclusion, title := final.conclusion, "Stopped"
		summary := fmt.Sprintf("The run ended before this task did: %s.", strings.ToLower(final.title))
		switch final.conclusion {
		case tekton.ConclusionSuccess:
			conclusion, title, summary = tekton.ConclusionSkipped, "Skipped", "The run passed without running this task."
		case tekton.ConclusionFailure:
			conclusion = tekton.ConclusionCancelled
		case tekton.ConclusionSkipped:
			title = "Superseded"
		}
		if start == nil && final.conclusion != tekton.ConclusionSuccess && final.conclusion != tekton.ConclusionSkipped {
			title = "Not run"
		}
		now := r.now()
		return r.taskCheckUpdate(run, tekton.StatusCompleted, conclusion, title, summary+"\n\n"+link, start, &now), tekton.StatusCompleted
	}
	if start != nil {
		return r.taskCheckUpdate(run, tekton.StatusInProgress, "", "Running", link, start, nil), tekton.StatusInProgress
	}
	return githubapp.CheckRunUpdate{}, ""
}

func (r *Reporter) taskCheckUpdate(run *unstructured.Unstructured, status, conclusion, title, summary string, start *metav1.Time, completed *time.Time) githubapp.CheckRunUpdate {
	upd := githubapp.CheckRunUpdate{
		Status: new(status),
		Output: &github.CheckRunOutput{
			Title:   new(title),
			Summary: new(checkrun.Truncate(summary, checkrun.MaxSummaryLength)),
			Text:    new(r.textWithMarker(run, "")),
		},
	}
	if conclusion != "" {
		upd.Conclusion = new(conclusion)
	}
	if start != nil {
		upd.StartedAt = &github.Timestamp{Time: start.Time}
	}
	if completed != nil {
		upd.CompletedAt = &github.Timestamp{Time: *completed}
	}
	return upd
}

func taskURL(dashboard, namespace, run, task string) string {
	u := checkrun.DashboardURL(dashboard, namespace, run)
	if u == "" {
		return ""
	}
	return u + "?pipelineTask=" + task
}

// taskResult matches a pipeline result that is exactly one task's result.
var taskResult = regexp.MustCompile(`^\$\(tasks\.([^.]+)\.results\.([^.)]+)\)$`)

// fromTasks fills in pipeline results that Tekton left out of the run (it drops
// a failed task's results) from the TaskRun they name, so a check-summary a
// failing task wrote is not lost on exactly the run that needs it.
func fromTasks(run *unstructured.Unstructured, results map[string]string, taskRuns []unstructured.Unstructured) {
	declared, _, _ := unstructured.NestedSlice(run.Object, "status", "pipelineSpec", "results")
	if len(declared) == 0 {
		declared, _, _ = unstructured.NestedSlice(run.Object, "spec", "pipelineSpec", "results")
	}
	for _, d := range declared {
		m, _ := d.(map[string]any)
		name, _ := m["name"].(string)
		value, _ := m["value"].(string)
		ref := taskResult.FindStringSubmatch(value)
		if name == "" || results[name] != "" || ref == nil {
			continue
		}
		for i := range taskRuns {
			if taskRuns[i].GetLabels()["tekton.dev/pipelineTask"] != ref[1] {
				continue
			}
			if v, ok := tekton.Results(&taskRuns[i])[ref[2]]; ok {
				results[name] = v
			}
		}
	}
}

// codeFence returns a backtick fence longer than any backtick run in s.
func codeFence(s string) string {
	longest, run := 0, 0
	for _, ch := range s {
		if ch == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

func cell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", "\\|")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

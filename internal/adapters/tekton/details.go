package tekton

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"octomaton.dev/internal/services/ci"
)

// labelPipelineTask names the pipeline task a TaskRun runs; Tekton sets it.
const labelPipelineTask = "tekton.dev/pipelineTask"

// Details reads a run's TaskRuns, without the run's secrets in their results and messages. When the
// secrets can't be read, those texts are withheld.
func (r *Runner) Details(ctx context.Context, id ci.RunID) (ci.Details, error) {
	c := r.client()
	pr, err := c.Get(ctx, id.Tenant, id.Name)
	if err != nil {
		return ci.Details{}, err
	}
	if pr == nil {
		return ci.Details{}, ci.ErrNotFound
	}
	taskRuns, err := c.TaskRuns(ctx, id.Tenant, id.Name)
	if err != nil {
		return ci.Details{}, err
	}
	results := resultsOf(pr)
	fromTasks(pr, results, taskRuns)
	red, err := c.redactorFor(ctx, pr)
	if err != nil {
		// The task table and the conclusion hold no output; only the texts that need redacting are withheld, so the
		// run's report still concludes.
		r.logger().WarnContext(ctx, "Withholding a run's results and task messages", "run", id.String(), "error", err)
		red = &redactor{withhold: true}
	}
	tasks := tasksOf(pr, taskRuns)
	for i := range tasks {
		tasks[i].Message = red.Redact(tasks[i].Message)
		redactValues(red, tasks[i].Results)
	}
	return ci.Details{Tasks: tasks, Results: redactValues(red, results)}, nil
}

// redactValues redacts every value of m in place, and returns it.
func redactValues(red *redactor, m map[string]string) map[string]string {
	for k, v := range m {
		m[k] = red.Redact(v)
	}
	return m
}

// tasksOf lists a run's tasks in pipeline order, then any other task (finally tasks, and tasks of a
// pipeline Tekton has not resolved into the status yet), then skipped ones. A task's latest TaskRun
// tells its state.
func tasksOf(pr *unstructured.Unstructured, taskRuns []unstructured.Unstructured) []ci.Task {
	byTask := map[string]*unstructured.Unstructured{}
	var others []string
	for i := range taskRuns {
		task := taskRuns[i].GetLabels()[labelPipelineTask]
		if task == "" {
			continue
		}
		if prev, ok := byTask[task]; !ok || taskRuns[i].GetCreationTimestamp().Time.After(prev.GetCreationTimestamp().Time) {
			if !ok {
				others = append(others, task)
			}
			byTask[task] = &taskRuns[i]
		}
	}
	st, _ := getPipelineRunStatus(pr)
	skipped := map[string]string{}
	for _, s := range st.SkippedTasks {
		skipped[s.Name] = s.Reason
	}
	var tasks []ci.Task
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		t := ci.Task{Name: name, State: ci.TaskPending}
		if tr := byTask[name]; tr != nil {
			fillTask(&t, tr)
		} else if reason, ok := skipped[name]; ok {
			t.State, t.Note = ci.TaskSkipped, reason
		}
		tasks = append(tasks, t)
	}
	for _, name := range taskNames(pr) {
		add(name)
	}
	sort.SliceStable(others, func(i, j int) bool {
		return byTask[others[i]].GetCreationTimestamp().Time.Before(byTask[others[j]].GetCreationTimestamp().Time)
	})
	for _, name := range others {
		add(name)
	}
	for _, s := range st.SkippedTasks {
		add(s.Name)
	}
	return tasks
}

func fillTask(t *ci.Task, tr *unstructured.Unstructured) {
	ts, err := getTaskRunStatus(tr)
	if err != nil {
		return
	}
	conclusion, message, done := conclusionOf(ts.Conditions)
	switch {
	case !done && ts.StartTime == nil:
		t.State = ci.TaskPending
	case !done:
		t.State = ci.TaskRunning
	case conclusion == ci.Success:
		t.State = ci.TaskSucceeded
	case conclusion == ci.Cancelled:
		t.State = ci.TaskCancelled
	case conclusion == ci.TimedOut:
		t.State = ci.TaskTimedOut
	default:
		t.State = ci.TaskFailed
	}
	if ts.StartTime != nil {
		t.Started = ts.StartTime.Time.UTC()
	}
	if ts.CompletionTime != nil {
		t.Finished = ts.CompletionTime.Time.UTC()
	}
	t.Results = resultsOf(tr)
	if t.State != ci.TaskFailed && t.State != ci.TaskTimedOut {
		return
	}
	t.Message = message
	for _, step := range ts.Steps {
		if step.Terminated != nil && step.Terminated.ExitCode != 0 && step.Container != "" {
			logs := ""
			if ts.PodName != "" {
				logs = ts.PodName + "/" + step.Container
			}
			t.FailedSteps = append(t.FailedSteps, ci.Step{Name: step.Name, ExitCode: step.Terminated.ExitCode, Logs: logs})
		}
	}
}

// taskResult matches a pipeline result that is exactly one task's result.
var taskResult = regexp.MustCompile(`^\$\(tasks\.([^.]+)\.results\.([^.)]+)\)$`)

// fromTasks fills in pipeline results that Tekton left out of the run (it drops a failed task's
// results) from the TaskRun they name, so a check-summary a failing task wrote is not lost on
// exactly the run that needs it.
func fromTasks(pr *unstructured.Unstructured, results map[string]string, taskRuns []unstructured.Unstructured) {
	declared, _, _ := unstructured.NestedSlice(pr.Object, "status", "pipelineSpec", "results")
	if len(declared) == 0 {
		declared, _, _ = unstructured.NestedSlice(pr.Object, "spec", "pipelineSpec", "results")
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
			if taskRuns[i].GetLabels()[labelPipelineTask] != ref[1] {
				continue
			}
			if v, ok := resultsOf(&taskRuns[i])[ref[2]]; ok {
				results[name] = v
			}
		}
	}
}

// StepLogs reads the tail of a failed step's container log, without the run's secrets. When the
// secrets can't be read, the logs are withheld.
func (r *Runner) StepLogs(ctx context.Context, id ci.RunID, step ci.Step, tailLines, limitBytes int64) (string, error) {
	pod, container, ok := strings.Cut(step.Logs, "/")
	if !ok || pod == "" {
		return "", errors.New("the TaskRun has no pod")
	}
	c := r.client()
	pr, err := c.Get(ctx, id.Tenant, id.Name)
	if err != nil {
		return "", err
	}
	if pr == nil {
		return "", ci.ErrNotFound
	}
	red, err := c.redactorFor(ctx, pr)
	if err != nil {
		return "", err
	}
	logs, err := c.PodLogs(ctx, id.Tenant, pod, container, tailLines, limitBytes)
	if err != nil {
		return "", err
	}
	return red.Redact(logs), nil
}

// SetToken stores a run's token in its token Secret, which the run owns: it is created the first
// time and replaced after.
func (r *Runner) SetToken(ctx context.Context, run ci.Run, t ci.Token) error {
	c := r.client()
	id := run.ID
	token := token{Value: t.Value, ExpiresAt: t.ExpiresAt, Permissions: t.Permissions}
	secret, err := c.TokenSecret(ctx, id.Tenant, id.Name)
	if err != nil {
		return err
	}
	if secret != nil {
		return c.UpdateTokenSecret(ctx, secret, token)
	}
	pr, err := c.Get(ctx, id.Tenant, id.Name)
	if err != nil {
		return err
	}
	if pr == nil {
		return ci.ErrNotFound
	}
	err = c.CreateTokenSecret(ctx, pr, token, map[string]string{
		annotationRepository:     run.Trigger.Repository.FullName,
		annotationInstallationID: strconv.FormatInt(run.Trigger.InstallationID, 10),
	})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// Another replica stored one meanwhile: replace it.
	if secret, err = c.TokenSecret(ctx, id.Tenant, id.Name); err != nil || secret == nil {
		return err
	}
	return c.UpdateTokenSecret(ctx, secret, token)
}

// TokenExpiry reads when a run's stored token expires. An unreadable expiry reads as the zero time:
// already expired.
func (r *Runner) TokenExpiry(ctx context.Context, id ci.RunID) (time.Time, bool, error) {
	secret, err := r.client().TokenSecret(ctx, id.Tenant, id.Name)
	if err != nil || secret == nil {
		return time.Time{}, false, err
	}
	expires, _ := time.Parse(time.RFC3339, secret.Annotations[annotationExpiresAt])
	return expires, true, nil
}

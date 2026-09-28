// Package reporter watches the PipelineRuns Switchboard created and mirrors their
// state onto GitHub: the check run goes in_progress when the run starts, shows a
// per-task progress table while it runs, and completes with the run's conclusion;
// task checks follow their TaskRuns; a comment command gets a reply. Finished runs
// release the next queued run of their concurrency group. Held runs are resumed.
// It runs on the elected leader only; what was reported is recorded on each run,
// so a new leader continues rather than repeats.
package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arikkfir-org/switchboard/internal/checkrun"
	"github.com/arikkfir-org/switchboard/internal/githubapp"
	"github.com/arikkfir-org/switchboard/internal/metrics"
	"github.com/arikkfir-org/switchboard/internal/tekton"
	"github.com/google/go-github/v92/github"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	defaultWorkers     = 4
	defaultResync      = 5 * time.Minute
	defaultHeldTooLong = 5 * time.Minute
	maxRetries         = 10
	reconcileLimit     = 2 * time.Minute
)

// LiveRuns selects the runs the reporter still has something to do with.
const LiveRuns = tekton.LabelManagedBy + "=" + tekton.ManagedByValue + ",!" + tekton.LabelDone

// Runs is the Kubernetes access the reporter needs; *tekton.Client implements it.
type Runs interface {
	Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error)
	Annotate(ctx context.Context, namespace, name string, annotations map[string]*string) error
	Label(ctx context.Context, namespace, name string, labels, annotations map[string]string) error
	TaskRuns(ctx context.Context, namespace, pipelineRun string) ([]unstructured.Unstructured, error)
	PodLogs(ctx context.Context, namespace, pod, container string, tailLines, limitBytes int64) (string, error)
}

var _ Runs = (*tekton.Client)(nil)

// Reporter mirrors PipelineRun state onto GitHub check runs.
type Reporter struct {
	// Dynamic is used by the informer.
	Dynamic      dynamic.Interface
	Runs         Runs
	GitHub       githubapp.Provider
	DashboardURL string
	Logger       *slog.Logger
	Metrics      *metrics.Metrics
	// Resume finishes starting a run held for longer than HeldTooLong (trigger.Service.Resume).
	Resume func(ctx context.Context, run *unstructured.Unstructured) error
	// ReleaseNext starts the next queued run of a finished run's group (trigger.Service.ReleaseNext).
	ReleaseNext func(ctx context.Context, run *unstructured.Unstructured) error
	// Workers is the number of concurrent reconcilers (default 4).
	Workers int
	// Resync is the informer resync period (default 5m).
	Resync time.Duration
	// HeldTooLong is how long a run may be held before it is resumed (default 5m).
	HeldTooLong time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	synced  atomic.Bool
	mu      sync.Mutex
	deleted map[string]*unstructured.Unstructured
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reporter) heldTooLong() time.Duration {
	if r.HeldTooLong > 0 {
		return r.HeldTooLong
	}
	return defaultHeldTooLong
}

// Synced reports whether the informer cache is synced (false when not running).
func (r *Reporter) Synced() bool { return r.synced.Load() }

// Run watches live PipelineRuns in all namespaces until ctx is cancelled.
func (r *Reporter) Run(ctx context.Context) error {
	workers, resync := r.Workers, r.Resync
	if workers <= 0 {
		workers = defaultWorkers
	}
	if resync <= 0 {
		resync = defaultResync
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(r.Dynamic, resync, metav1.NamespaceAll, func(o *metav1.ListOptions) {
		o.LabelSelector = LiveRuns
	})
	informer := factory.ForResource(tekton.PipelineRuns).Informer()
	queue := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "switchboard-reporter"},
	)
	defer queue.ShutDown()

	enqueue := func(obj any) {
		if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
			queue.Add(key)
		}
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, obj any) { enqueue(obj) },
		DeleteFunc: func(obj any) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			// A run labelled done leaves the watch as a deletion too; only a run
			// deleted before it was reported needs attention.
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetLabels()[tekton.LabelDone] == "" {
				r.rememberDeleted(u)
				enqueue(u)
			}
		},
	}); err != nil {
		return fmt.Errorf("registering the PipelineRun event handler: %w", err)
	}

	factory.Start(ctx.Done())
	defer factory.Shutdown()
	r.Logger.Info("Waiting for the PipelineRun informer to sync")
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return ctx.Err()
	}
	r.synced.Store(true)
	defer r.synced.Store(false)
	r.Logger.Info("Reporter started", "workers", workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for r.processNext(ctx, queue, informer.GetIndexer()) {
			}
		})
	}
	<-ctx.Done()
	queue.ShutDown()
	wg.Wait()
	r.Logger.Info("Reporter stopped")
	return nil
}

func (r *Reporter) rememberDeleted(u *unstructured.Unstructured) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleted == nil {
		r.deleted = map[string]*unstructured.Unstructured{}
	}
	r.deleted[u.GetNamespace()+"/"+u.GetName()] = u
}

func (r *Reporter) takeDeleted(key string) *unstructured.Unstructured {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.deleted[key]
	delete(r.deleted, key)
	return u
}

func (r *Reporter) processNext(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string], indexer cache.Indexer) bool {
	key, shutdown := queue.Get()
	if shutdown {
		return false
	}
	defer queue.Done(key)

	start := time.Now()
	after, err := r.sync(ctx, key, indexer)
	result := "ok"
	switch {
	case err == nil:
		queue.Forget(key)
		if after > 0 {
			queue.AddAfter(key, after)
		}
	case queue.NumRequeues(key) < maxRetries:
		result = "retry"
		r.Logger.Warn("Reporting a PipelineRun failed; will retry", "key", key, "error", err)
		queue.AddRateLimited(key)
	default:
		result = "error"
		r.Logger.Error("Reporting a PipelineRun failed; giving up until it changes", "key", key, "error", err)
		queue.Forget(key)
	}
	r.Metrics.ReconcileDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
	return true
}

func (r *Reporter) sync(ctx context.Context, key string, indexer cache.Indexer) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, reconcileLimit)
	defer cancel()
	obj, exists, err := indexer.GetByKey(key)
	if err != nil {
		return 0, err
	}
	if !exists {
		if u := r.takeDeleted(key); u != nil {
			return 0, r.ReportDeleted(ctx, u)
		}
		return 0, nil
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return 0, nil
	}
	return r.Reconcile(ctx, u.DeepCopy())
}

// Reconcile brings GitHub up to date with one run. It returns a delay after
// which the run should be looked at again (for held runs), or zero.
func (r *Reporter) Reconcile(ctx context.Context, run *unstructured.Unstructured) (time.Duration, error) {
	ann := run.GetAnnotations()
	reported := ann[tekton.AnnotationReported]
	checkID, _ := strconv.ParseInt(ann[tekton.AnnotationCheckRunID], 10, 64)
	switch {
	case run.GetLabels()[tekton.LabelDone] != "":
		return 0, nil
	case tekton.IsDone(run):
		return 0, r.finish(ctx, run.GetNamespace(), run.GetName())
	case tekton.IsPending(run) && run.GetDeletionTimestamp() == nil:
		// A start takes seconds. A run held longer lost the rest of its start to a
		// restart, or waits its turn in a queue: resuming does what is left and
		// releases it when it is next.
		held := r.now().Sub(run.GetCreationTimestamp().Time)
		if held < r.heldTooLong() {
			return r.heldTooLong() - held, nil
		}
		if r.Resume == nil {
			return 0, nil
		}
		return r.heldTooLong(), r.Resume(ctx, run)
	case tekton.Started(run) && reported == tekton.ReportedQueued && checkID != 0:
		if err := r.markInProgress(ctx, run, checkID); err != nil {
			return 0, err
		}
		return 0, r.taskChecks(ctx, run, nil)
	case tekton.Started(run) && reported == tekton.ReportedInProgress && checkID != 0:
		if err := r.progress(ctx, run, checkID); err != nil {
			return 0, err
		}
		return 0, r.taskChecks(ctx, run, nil)
	}
	return 0, nil
}

type runRef struct {
	checkID        int64
	installationID int64
	owner, repo    string
}

func refOf(run *unstructured.Unstructured) (runRef, bool) {
	a := run.GetAnnotations()
	id, _ := strconv.ParseInt(a[tekton.AnnotationCheckRunID], 10, 64)
	inst, _ := strconv.ParseInt(a[tekton.AnnotationInstallationID], 10, 64)
	owner, repo, ok := strings.Cut(a[tekton.AnnotationRepository], "/")
	if !ok || owner == "" || repo == "" || inst == 0 {
		return runRef{}, false
	}
	return runRef{checkID: id, installationID: inst, owner: owner, repo: repo}, true
}

// contextOf returns the trigger context stored on a run.
func contextOf(run *unstructured.Unstructured) (checkrun.Context, bool) {
	raw := run.GetAnnotations()[tekton.AnnotationContext]
	if raw == "" {
		return checkrun.Context{}, false
	}
	var c checkrun.Context
	if err := json.Unmarshal([]byte(raw), &c); err != nil || c.Version != checkrun.ContextVersion {
		return checkrun.Context{}, false
	}
	return c, true
}

func (r *Reporter) annotate(ctx context.Context, run *unstructured.Unstructured, key, value string) error {
	return r.Runs.Annotate(ctx, run.GetNamespace(), run.GetName(), map[string]*string{key: &value})
}

// markInProgress reports that a run started.
func (r *Reporter) markInProgress(ctx context.Context, run *unstructured.Unstructured, checkID int64) error {
	ref, ok := refOf(run)
	if !ok {
		return nil
	}
	st, _ := tekton.GetPipelineRunStatus(run)
	upd := githubapp.CheckRunUpdate{
		Status: new(tekton.StatusInProgress),
		Output: &github.CheckRunOutput{
			Title:   new("Running"),
			Summary: new(r.header(run)),
			Text:    new(r.textWithMarker(run, "")),
		},
	}
	if st.StartTime != nil {
		upd.StartedAt = &github.Timestamp{Time: st.StartTime.Time}
	}
	if _, err := r.GitHub.Installation(ref.installationID).UpdateCheckRun(ctx, ref.owner, ref.repo, checkID, upd); err != nil {
		r.Metrics.CheckRunErrors.WithLabelValues("update").Inc()
		return err
	}
	return r.annotate(ctx, run, tekton.AnnotationReported, tekton.ReportedInProgress)
}

// progress keeps a running check saying what it does: for runs of two or more
// tasks, a title naming the done count, the running task and the elapsed time,
// and a per-task table. It writes only when the table changes.
func (r *Reporter) progress(ctx context.Context, run *unstructured.Unstructured, checkID int64) error {
	ref, ok := refOf(run)
	if !ok {
		return nil
	}
	taskRuns, err := r.Runs.TaskRuns(ctx, run.GetNamespace(), run.GetName())
	if err != nil {
		return err
	}
	rows := r.rows(run, taskRuns)
	if len(rows) < 2 {
		return nil
	}
	done, running := 0, ""
	for _, row := range rows {
		switch {
		case row.finished():
			done++
		case row.state == stateRunning && running == "":
			running = row.name
		}
	}
	key := fmt.Sprintf("%d/%d %s", done, len(rows), running)
	for _, row := range rows {
		key += " " + row.state
	}
	if run.GetAnnotations()[tekton.AnnotationProgress] == key {
		return nil
	}
	st, _ := tekton.GetPipelineRunStatus(run)
	title := fmt.Sprintf("%d of %d", done, len(rows))
	if running != "" {
		title += " · " + running
	}
	title += " · " + formatDuration(tekton.Duration(st.StartTime, nil, r.now()))
	summary := r.header(run) + "\n" + table(rows)
	upd := githubapp.CheckRunUpdate{
		Status: new(tekton.StatusInProgress),
		Output: &github.CheckRunOutput{
			Title:   new(title),
			Summary: new(checkrun.Truncate(summary, checkrun.MaxSummaryLength)),
			Text:    new(r.textWithMarker(run, "")),
		},
	}
	if _, err := r.GitHub.Installation(ref.installationID).UpdateCheckRun(ctx, ref.owner, ref.repo, checkID, upd); err != nil {
		r.Metrics.CheckRunErrors.WithLabelValues("update").Inc()
		return err
	}
	return r.annotate(ctx, run, tekton.AnnotationProgress, key)
}

// finish reports a finished run, in two recorded steps: its check (and task
// checks) are concluded and the next queued run released ("concluded"), then a
// comment command is answered and the run let go ("completed", done label). The
// run is read again first, because the watched copy may predate what this
// reporter already wrote.
func (r *Reporter) finish(ctx context.Context, ns, name string) error {
	run, err := r.Runs.Get(ctx, ns, name)
	if err != nil || run == nil {
		return err
	}
	ann := run.GetAnnotations()
	reported := ann[tekton.AnnotationReported]
	if reported == tekton.ReportedCompleted {
		return r.letGo(ctx, run)
	}
	ref, ok := refOf(run)
	if !ok || ref.checkID == 0 {
		// Never opened its check (it was never fully started): nothing to report.
		return r.letGo(ctx, run)
	}
	rep, err := r.outcome(ctx, run)
	if err != nil {
		return err
	}
	if reported != tekton.ReportedConcluded {
		if err := r.taskChecks(ctx, run, &rep); err != nil {
			return err
		}
		st, _ := tekton.GetPipelineRunStatus(run)
		completed := r.now()
		if st.CompletionTime != nil {
			completed = st.CompletionTime.Time
		}
		upd := githubapp.CheckRunUpdate{
			Status:      new(tekton.StatusCompleted),
			Conclusion:  new(rep.conclusion),
			CompletedAt: &github.Timestamp{Time: completed},
			Output: &github.CheckRunOutput{
				Title:   new(rep.title),
				Summary: new(rep.summary),
				Text:    new(rep.text),
			},
		}
		if st.StartTime != nil {
			upd.StartedAt = &github.Timestamp{Time: st.StartTime.Time}
		}
		if _, err := r.GitHub.Installation(ref.installationID).UpdateCheckRun(ctx, ref.owner, ref.repo, ref.checkID, upd); err != nil {
			r.Metrics.CheckRunErrors.WithLabelValues("update").Inc()
			return err
		}
		if r.ReleaseNext != nil {
			if err := r.ReleaseNext(ctx, run); err != nil {
				return err
			}
		}
		if err := r.annotate(ctx, run, tekton.AnnotationReported, tekton.ReportedConcluded); err != nil {
			return err
		}
		c, _ := contextOf(run)
		r.Logger.Info("Run finished", "namespace", ns, "name", name, "repository", ann[tekton.AnnotationRepository],
			"pipeline", run.GetLabels()[tekton.LabelPipeline], "event", run.GetLabels()[tekton.LabelEvent], "sha", ann[tekton.AnnotationSHA],
			"branch", c.Branch, "conclusion", rep.conclusion, "url", checkrun.DashboardURL(r.DashboardURL, ns, name))
	}
	if err := r.reply(ctx, run, ref, rep); err != nil {
		return err
	}
	return r.letGo(ctx, run)
}

// letGo marks a run done: it leaves the reporter's watch for good.
func (r *Reporter) letGo(ctx context.Context, run *unstructured.Unstructured) error {
	return r.Runs.Label(ctx, run.GetNamespace(), run.GetName(), map[string]string{tekton.LabelDone: "true"},
		map[string]string{tekton.AnnotationReported: tekton.ReportedCompleted})
}

// marks show a conclusion in a comment reply.
var marks = map[string]string{
	tekton.ConclusionSuccess:   "✅",
	tekton.ConclusionFailure:   "❌",
	tekton.ConclusionCancelled: "⏹️",
	tekton.ConclusionSkipped:   "⏹️",
	tekton.ConclusionTimedOut:  "⌛",
}

// reply answers the comment command that started a run with how it ended.
func (r *Reporter) reply(ctx context.Context, run *unstructured.Unstructured, ref runRef, rep report) error {
	c, ok := contextOf(run)
	if !ok || c.Comment == nil || c.PullRequest == nil {
		return nil
	}
	command := strings.TrimSpace(c.Comment.Command + " " + c.Comment.Arguments)
	mark := marks[rep.conclusion]
	if mark == "" {
		mark = rep.conclusion
	}
	body := fmt.Sprintf("@%s %s `%s`: %s\n\n%s", c.Comment.Author, mark, strings.ReplaceAll(command, "`", "'"), rep.title, rep.summary)
	if err := r.GitHub.Installation(ref.installationID).Comment(ctx, ref.owner, ref.repo, c.PullRequest.Number, checkrun.Truncate(body, checkrun.MaxSummaryLength)); err != nil {
		return fmt.Errorf("replying to the comment: %w", err)
	}
	return nil
}

// ReportDeleted reports a run deleted before it was reported: its check is
// concluded as cancelled.
func (r *Reporter) ReportDeleted(ctx context.Context, run *unstructured.Unstructured) error {
	if run.GetAnnotations()[tekton.AnnotationReported] == tekton.ReportedCompleted {
		return nil
	}
	ref, ok := refOf(run)
	if !ok || ref.checkID == 0 {
		return nil
	}
	ns, name := run.GetNamespace(), run.GetName()
	upd := githubapp.CheckRunUpdate{
		Status:      new(tekton.StatusCompleted),
		Conclusion:  new(tekton.ConclusionCancelled),
		CompletedAt: &github.Timestamp{Time: r.now()},
		Output: &github.CheckRunOutput{
			Title:   new("Cancelled"),
			Summary: new(r.header(run) + "\nThe PipelineRun was deleted before it finished."),
			Text:    new(r.textWithMarker(run, "")),
		},
	}
	if _, err := r.GitHub.Installation(ref.installationID).UpdateCheckRun(ctx, ref.owner, ref.repo, ref.checkID, upd); err != nil {
		r.Metrics.CheckRunErrors.WithLabelValues("update").Inc()
		return err
	}
	r.Logger.Info("Reported deleted PipelineRun as cancelled", "namespace", ns, "name", name)
	return nil
}

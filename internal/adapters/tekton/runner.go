package tekton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/system/metrics"
)

// supersededByHead prefixes the superseded-by annotation of a run stood down for a newer commit.
const supersededByHead = "head:"

// Runner runs pipelines as Tekton PipelineRuns: it implements ci.Runner. What Octomaton records
// about a run lives in the PipelineRun's labels and annotations, which only this package reads and
// writes.
type Runner struct {
	Dynamic    dynamic.Interface
	Kube       kubernetes.Interface
	Namespaces *Namespaces
	// DashboardURL is the Tekton Dashboard's base URL; without it, runs link nowhere.
	DashboardURL string
	Logger       *slog.Logger
	Metrics      *metrics.Metrics
	// Workers is the number of runs Watch hands over at once (default 4).
	Workers int
	// Resync is how often Watch hands over every live run again (default 5m).
	Resync time.Duration

	synced  atomic.Bool
	mu      sync.Mutex
	deleted map[string]*unstructured.Unstructured
}

var _ ci.Runner = (*Runner)(nil)

func (r *Runner) client() *kubeClient { return &kubeClient{Dynamic: r.Dynamic, Kube: r.Kube} }

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Runner) metrics() *metrics.Metrics {
	if r.Metrics != nil {
		return r.Metrics
	}
	return metrics.Discard()
}

// refusal is a run that cannot start, reported as the core reports its own.
func refusal(format string, args ...any) *ci.Refusal {
	return &ci.Refusal{Title: "Could not start the pipeline", Reason: fmt.Sprintf(format, args...)}
}

// Check makes sure the repository's namespace exists and the definition is a PipelineRun this
// repository may run.
func (r *Runner) Check(ctx context.Context, spec ci.RunSpec) error {
	repo := spec.Trigger.Repository
	ns, err := r.Namespaces.Resolve(repo)
	if err != nil {
		return refusal("Could not determine the namespace for %s: %v", repo.FullName, err)
	}
	exists, err := r.client().NamespaceExists(ctx, ns)
	if err != nil {
		refused := refusal("Could not verify that namespace `%s` exists: %v", ns, err)
		refused.Cause = err
		return refused
	}
	if !exists {
		return refusal("repository not onboarded: namespace %s not found", ns)
	}
	_, err = r.definition(spec, ns)
	return err
}

// definition parses spec's PipelineRun file and checks it fits the namespace and the pipeline's
// settings.
func (r *Runner) definition(spec ci.RunSpec, ns string) (*unstructured.Unstructured, error) {
	pr, err := parsePipelineRun(spec.Definition)
	if err != nil {
		return nil, refusal("`%s` is not a valid PipelineRun file: %v", spec.Path, err)
	}
	if fileNS := pr.GetNamespace(); fileNS != "" && fileNS != ns {
		return nil, refusal("`%s` sets namespace `%s`, but this repository's runs must be created in namespace `%s`. Remove `metadata.namespace` from the file.", spec.Path, fileNS, ns)
	}
	if err := checkRemoteRefs(pr.Object); err != nil {
		return nil, refusal("`%s`: %v.", spec.Path, err)
	}
	if spec.TaskReports && len(taskNames(pr)) == 0 {
		return nil, refusal("Pipeline `%s` sets `taskChecks`, which needs the PipelineRun's own `spec.pipelineSpec` to list its tasks.", spec.Trigger.Pipeline)
	}
	return pr, nil
}

// Create renders spec's PipelineRun for attempt and creates it held.
func (r *Runner) Create(ctx context.Context, spec ci.RunSpec, attempt int) (ci.Run, error) {
	t := spec.Trigger
	ns, err := r.Namespaces.Resolve(t.Repository)
	if err != nil {
		return ci.Run{}, refusal("Could not determine the namespace for %s: %v", t.Repository.FullName, err)
	}
	src, err := r.definition(spec, ns)
	if err != nil {
		return ci.Run{}, err
	}
	name := runName(t.Repository.Name, t.Pipeline, t.Revision, attempt)
	in := renderInput{Namespace: ns, Name: name, Params: spec.Params, Timeout: spec.Timeout, Labels: runLabels(spec), Annotations: runAnnotations(spec, attempt), Held: true}
	token := ""
	if spec.Token != nil {
		in.TokenWorkspace, token = spec.Token.Workspace, tokenSecretName(name)
	}
	pr, err := render(src, in)
	if err != nil {
		return ci.Run{}, refusal("%v", err)
	}
	if err := checkSecrets(pr, token, spec.Secrets); err != nil {
		return ci.Run{}, &ci.Refusal{Title: "Refused", Reason: err.Error()}
	}
	if err := checkServiceAccounts(ctx, r.client(), ns, spec.Path, pr, t.Branch); err != nil {
		return ci.Run{}, err
	}
	created, err := r.client().Create(ctx, pr)
	if errors.Is(err, errAlreadyExists) {
		existing, gerr := r.Get(ctx, ci.RunID{Tenant: ns, Name: name})
		if gerr != nil {
			existing = ci.Run{}
		}
		return existing, ci.ErrExists
	}
	if err != nil {
		return ci.Run{}, &ci.Refusal{Title: "Could not create the PipelineRun", Reason: fmt.Sprintf("Kubernetes refused PipelineRun `%s/%s`:\n\n```\n%v\n```", ns, name, err), Cause: err}
	}
	return runOf(created), nil
}

// runLabels are what queries select runs by.
func runLabels(spec ci.RunSpec) map[string]string {
	t := spec.Trigger
	l := map[string]string{
		labelManagedBy:    managedByValue,
		labelPipeline:     t.Pipeline,
		labelEvent:        t.Event,
		labelRepositoryID: strconv.FormatInt(t.Repository.ID, 10),
		labelSHA:          t.Revision,
	}
	if key := spec.Concurrency.Key; key != "" {
		l[labelConcurrencyGroup] = groupLabel(t.Repository.FullName, key)
	}
	if t.Comment != nil {
		l[labelComment] = strconv.FormatInt(t.Comment.ID, 10)
	}
	if t.Schedule != nil {
		if slot, err := time.Parse(time.RFC3339, t.Schedule.Slot); err == nil {
			l[labelSlot] = slotLabel(slot)
		}
	}
	return l
}

func slotLabel(slot time.Time) string { return strconv.FormatInt(slot.Unix(), 10) }

// runAnnotations record the rest of what a run is.
func runAnnotations(spec ci.RunSpec, attempt int) map[string]string {
	t := spec.Trigger
	if t.Version == 0 {
		t.Version = ci.TriggerVersion
	}
	a := map[string]string{
		annotationRepository:     t.Repository.FullName,
		annotationSHA:            t.Revision,
		annotationInstallationID: strconv.FormatInt(t.InstallationID, 10),
		annotationDeliveryID:     t.DeliveryID,
		annotationHead:           t.Head(),
		annotationAttempt:        strconv.Itoa(attempt),
	}
	if data, err := json.Marshal(t); err == nil {
		a[annotationContext] = string(data)
	}
	if c := spec.Concurrency; c.Group != "" {
		a[annotationConcurrencyGroup] = c.Group
		a[annotationConcurrencyPolicy] = string(c.Policy)
	}
	if spec.Token != nil {
		if data, err := json.Marshal(spec.Token); err == nil {
			a[annotationToken] = string(data)
		}
	}
	if spec.TaskReports {
		a[annotationTaskChecks] = "true"
	}
	return a
}

// Get returns a run, or ci.ErrNotFound.
func (r *Runner) Get(ctx context.Context, id ci.RunID) (ci.Run, error) {
	pr, err := r.client().Get(ctx, id.Tenant, id.Name)
	if err != nil {
		return ci.Run{}, err
	}
	if pr == nil {
		return ci.Run{}, ci.ErrNotFound
	}
	return runOf(pr), nil
}

// List returns the runs q selects, in the repository's namespace, or in all namespaces.
func (r *Runner) List(ctx context.Context, q ci.RunQuery) ([]ci.Run, error) {
	ns := ""
	set := labels.Set{labelManagedBy: managedByValue}
	if q.Repository != nil {
		var err error
		if ns, err = r.Namespaces.Resolve(*q.Repository); err != nil {
			return nil, err
		}
		set[labelRepositoryID] = strconv.FormatInt(q.Repository.ID, 10)
	}
	for label, value := range map[string]string{labelPipeline: q.Pipeline, labelSHA: q.Revision, labelEvent: q.Event, labelConcurrencyGroup: q.Group} {
		if value != "" {
			set[label] = value
		}
	}
	if !q.Slot.IsZero() {
		set[labelSlot] = slotLabel(q.Slot)
	}
	selector := labels.SelectorFromSet(set).String()
	if q.Live {
		selector += ",!" + labelDone
	}
	items, err := r.client().List(ctx, ns, selector)
	if err != nil {
		return nil, err
	}
	runs := make([]ci.Run, 0, len(items))
	for i := range items {
		runs = append(runs, runOf(&items[i]))
	}
	return runs, nil
}

// Release clears spec.status, which lets Tekton start the run.
func (r *Runner) Release(ctx context.Context, id ci.RunID) error {
	return r.client().SetStatus(ctx, id.Tenant, id.Name, "")
}

// Cancel stops a run (its finally tasks still run) and records why.
func (r *Runner) Cancel(ctx context.Context, id ci.RunID, why ci.Cancellation) error {
	a := map[string]string{}
	if why.Reason != "" {
		a[annotationCancelReason] = why.Reason
	}
	switch {
	case why.SupersededBy != "":
		a[annotationSupersededBy] = why.SupersededBy
	case why.NewerCommit != "":
		a[annotationSupersededBy] = supersededByHead + why.NewerCommit
	}
	return r.client().Cancel(ctx, id.Tenant, id.Name, a)
}

// Record writes what was reported into the run's annotations; letting a run go labels it done,
// which takes it out of Watch.
func (r *Runner) Record(ctx context.Context, id ci.RunID, rec ci.Record) error {
	a := map[string]string{}
	if rec.ReportID != nil {
		a[annotationCheckRunID] = strconv.FormatInt(int64(*rec.ReportID), 10)
	}
	if rec.Reported != nil {
		a[annotationReported] = string(*rec.Reported)
	}
	if rec.Progress != nil {
		a[annotationProgress] = *rec.Progress
	}
	if rec.WaitingFor != nil {
		a[annotationWaitingFor] = *rec.WaitingFor
	}
	// Maps of strings and numbers always marshal.
	if rec.TaskReportIDs != nil {
		data, _ := json.Marshal(rec.TaskReportIDs)
		a[annotationTaskCheckIDs] = string(data)
	}
	if rec.TaskReportStates != nil {
		data, _ := json.Marshal(rec.TaskReportStates)
		a[annotationTaskCheckStates] = string(data)
	}
	meta := map[string]any{"annotations": a}
	if rec.Done {
		meta["labels"] = map[string]string{labelDone: "true"}
	}
	return r.client().patch(ctx, id.Tenant, id.Name, map[string]any{"metadata": meta})
}

// Link tells where people see a run: the Tekton Dashboard, when there is one.
func (r *Runner) Link(id ci.RunID) ci.RunLink {
	return ci.RunLink{Kind: kindPipelineRun, Name: id.String(), URL: dashboardURL(r.DashboardURL, id)}
}

// TaskURL is where people see one task of a run on the Tekton Dashboard.
func (r *Runner) TaskURL(id ci.RunID, task string) string {
	u := dashboardURL(r.DashboardURL, id)
	if u == "" {
		return ""
	}
	return u + "?pipelineTask=" + url.QueryEscape(task)
}

func dashboardURL(base string, id ci.RunID) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/#/namespaces/" + url.PathEscape(id.Tenant) + "/pipelineruns/" + url.PathEscape(id.Name)
}

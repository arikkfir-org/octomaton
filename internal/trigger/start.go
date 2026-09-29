package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/go-github/v92/github"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
	"octomaton.dev/internal/system/metrics"
	"octomaton.dev/internal/tekton"
)

// maxNameTries bounds retries when another delivery takes a run name first.
const maxNameTries = 10

// Refusal is a pipeline Octomaton would not start. The reason is reported on
// a failed check run (for comment commands, in the reply instead).
type Refusal struct {
	Pipeline string
	Reason   string
}

func (r *Refusal) Error() string { return r.Pipeline + ": " + r.Reason }

type startOptions struct {
	// Rerun creates a new attempt even when an equivalent run exists.
	Rerun bool
	// Dedupe are labels identifying the same run (a comment's, a schedule slot's);
	// without them, runs of the pipeline at the same commit and head are the same.
	Dedupe map[string]string
}

// tokenConfig is what a run records about its GitHub token.
type tokenConfig struct {
	Workspace   string            `json:"workspace"`
	Permissions map[string]string `json:"permissions"`
}

// prepared is everything needed to create a PipelineRun, computed before any
// object exists so that setup problems produce a single failed check run.
type prepared struct {
	namespace   string
	pipelineRun *unstructured.Unstructured
	params      map[string]string
	concurrency ci.Concurrency
}

// prepare resolves the namespace, loads the PipelineRun file and renders params
// and the concurrency group. Problems are returned as Markdown for the check run.
func (s *Service) prepare(ctx context.Context, gh githubapp.Client, c checkrun.Context, p *pipelines.Pipeline) (*prepared, string) {
	ns, err := s.Namespaces.Resolve(pipelines.RepositoryOf(c.Repository))
	if err != nil {
		return nil, fmt.Sprintf("Could not determine the namespace for %s: %v", c.Repository.FullName, err)
	}
	exists, err := s.Runs.NamespaceExists(ctx, ns)
	if err != nil {
		return nil, fmt.Sprintf("Could not verify that namespace `%s` exists: %v", ns, err)
	}
	if !exists {
		return nil, fmt.Sprintf("repository not onboarded: namespace %s not found", ns)
	}
	data, err := gh.GetFile(ctx, c.Repository.Owner, c.Repository.Name, p.PipelineRun, c.ConfigAt())
	if errors.Is(err, githubapp.ErrNotFound) {
		return nil, fmt.Sprintf("The pipelineRun file `%s` does not exist at `%s`.", p.PipelineRun, checkrun.ShortSHA(c.ConfigAt()))
	}
	if err != nil {
		return nil, fmt.Sprintf("Could not read the pipelineRun file `%s`: %v", p.PipelineRun, err)
	}
	pr, err := tekton.ParsePipelineRun(data)
	if err != nil {
		return nil, fmt.Sprintf("`%s` is not a valid PipelineRun file: %v", p.PipelineRun, err)
	}
	if fileNS := pr.GetNamespace(); fileNS != "" && fileNS != ns {
		return nil, fmt.Sprintf("`%s` sets namespace `%s`, but this repository's runs must be created in namespace `%s`. Remove `metadata.namespace` from the file.", p.PipelineRun, fileNS, ns)
	}
	if p.TaskChecks && len(tekton.TaskNames(pr)) == 0 {
		return nil, fmt.Sprintf("Pipeline `%s` sets `taskChecks`, which needs the PipelineRun's own `spec.pipelineSpec` to list its tasks.", p.Name)
	}
	tc := pipelines.ContextOf(c)
	params, err := p.RenderParams(tc)
	if err != nil {
		return nil, fmt.Sprintf("Could not render the params of pipeline `%s`: %v", p.Name, err)
	}
	conc, err := p.ConcurrencyFor(tc)
	if err != nil {
		return nil, fmt.Sprintf("Could not render the concurrency group of pipeline `%s`: %v", p.Name, err)
	}
	return &prepared{namespace: ns, pipelineRun: pr, params: params, concurrency: conc}, ""
}

// start creates the run of pipeline p for c, or finds the existing one: a
// redelivery (or another event for the same commit and head) finds the run
// already there; a re-run creates the next attempt. The run is created held,
// then its check run, task checks and token Secret are created, and it is
// released per its concurrency policy. Any failure after creation cancels the
// run and fails its check.
func (s *Service) start(ctx context.Context, gh githubapp.Client, c checkrun.Context, p *pipelines.Pipeline, opts startOptions) (string, bool, error) {
	log := s.logFor(c)
	refuse := func(title, reason string) (string, bool, error) {
		log.Warn("Pipeline refused", "title", title, "reason", reason)
		s.Metrics.RunCreated(ctx, metrics.RunFailed)
		if c.Comment == nil {
			s.createCompleted(ctx, gh, c, p.Name, "failure", title, reason, nil, "")
		}
		return "", false, &Refusal{Pipeline: p.Name, Reason: reason}
	}

	prep, problem := s.prepare(ctx, gh, c, p)
	if problem != "" {
		return refuse("Could not start the pipeline", problem)
	}
	runs, err := s.Runs.List(ctx, prep.namespace, labels.SelectorFromSet(map[string]string{
		tekton.LabelManagedBy:    tekton.ManagedByValue,
		tekton.LabelRepositoryID: strconv.FormatInt(c.Repository.ID, 10),
		tekton.LabelPipeline:     p.Name,
		tekton.LabelSHA:          c.Revision,
	}).String())
	if err != nil {
		s.Metrics.RunCreated(ctx, metrics.RunError)
		return "", false, err
	}
	same := sameRun(c, opts.Dedupe)
	if !opts.Rerun {
		var there *unstructured.Unstructured
		for i := range runs {
			if same(&runs[i]) && (there == nil || outranks(&runs[i], there)) {
				there = &runs[i]
			}
		}
		if there != nil && !tekton.CancelRequested(there) {
			log.Info("The run already exists", "name", there.GetName())
			s.Metrics.RunCreated(ctx, metrics.RunExisting)
			return there.GetName(), true, nil
		}
	}

	attempt := nextAttempt(runs)
	var created *unstructured.Unstructured
	for tries := 0; ; tries++ {
		name := tekton.RunName(c.Repository.Name, p.Name, c.Revision, attempt)
		pr, err := tekton.Render(prep.pipelineRun, tekton.RenderInput{
			Namespace:      prep.namespace,
			Name:           name,
			Params:         prep.params,
			Timeout:        p.TimeoutDuration(),
			TokenWorkspace: p.TokenWorkspace(),
			Labels:         runLabels(c, p, prep, opts),
			Annotations:    runAnnotations(c, p, prep, attempt),
			Held:           true,
		})
		if err != nil {
			return refuse("Could not start the pipeline", err.Error())
		}
		allowed := ""
		if p.GitHubToken != nil {
			allowed = tekton.TokenSecretName(name)
		}
		if err := tekton.CheckSecrets(pr, allowed); err != nil {
			return refuse("Refused", err.Error())
		}
		created, err = s.Runs.Create(ctx, pr)
		if errors.Is(err, tekton.ErrAlreadyExists) && tries < maxNameTries {
			// Another delivery took this attempt meanwhile: it is this run when it
			// is the same one, otherwise this is the next attempt.
			if other, gerr := s.Runs.Get(ctx, prep.namespace, name); gerr == nil && other != nil && !opts.Rerun && same(other) && !tekton.CancelRequested(other) {
				s.Metrics.RunCreated(ctx, metrics.RunExisting)
				return name, true, nil
			}
			attempt++
			continue
		}
		if err != nil {
			return refuse("Could not create the PipelineRun", fmt.Sprintf("Kubernetes refused PipelineRun `%s/%s`:\n\n```\n%v\n```", prep.namespace, name, err))
		}
		break
	}

	checkID, err := s.openCheck(ctx, gh, c, created, 0)
	if err == nil && p.TaskChecks {
		err = s.openTaskChecks(ctx, gh, c, created, false)
	}
	if err == nil && p.GitHubToken != nil {
		err = s.mintToken(ctx, c, created, p.TokenPermissions())
	}
	if err == nil {
		err = s.release(ctx, created)
	}
	if err != nil {
		s.Metrics.RunCreated(ctx, metrics.RunError)
		s.abort(ctx, gh, c, created, checkID, err)
		return "", false, err
	}
	s.Metrics.RunCreated(ctx, metrics.RunCreated)
	log.Info("Started pipeline", "namespace", created.GetNamespace(), "name", created.GetName(), "checkRunID", checkID,
		"attempt", attempt, "concurrencyGroup", prep.concurrency.Group, "policy", prep.concurrency.Policy)
	return created.GetName(), false, nil
}

// sameRun reports whether an existing run is the one a start would create.
func sameRun(c checkrun.Context, dedupe map[string]string) func(*unstructured.Unstructured) bool {
	return func(run *unstructured.Unstructured) bool {
		l := run.GetLabels()
		if len(dedupe) > 0 {
			for k, v := range dedupe {
				if l[k] != v {
					return false
				}
			}
			return true
		}
		if l[tekton.LabelComment] != "" || l[tekton.LabelSlot] != "" {
			return false
		}
		return run.GetAnnotations()[tekton.AnnotationHead] == c.Head()
	}
}

func attemptOf(run *unstructured.Unstructured) int {
	n, _ := strconv.Atoi(run.GetAnnotations()[tekton.AnnotationAttempt])
	return n
}

func checkRunIDOf(run *unstructured.Unstructured) int64 {
	id, _ := strconv.ParseInt(run.GetAnnotations()[tekton.AnnotationCheckRunID], 10, 64)
	return id
}

// nextAttempt follows the highest attempt there (not the count: older attempts may be pruned).
func nextAttempt(runs []unstructured.Unstructured) int {
	attempt := 1
	for i := range runs {
		if n := attemptOf(&runs[i]); n >= attempt {
			attempt = n + 1
		}
	}
	return attempt
}

// outranks reports whether GitHub shows a's check over b's (runs of one commit):
// the newer check, then the later attempt.
func outranks(a, b *unstructured.Unstructured) bool {
	if ca, cb := checkRunIDOf(a), checkRunIDOf(b); ca != cb {
		return ca > cb
	}
	return attemptOf(a) > attemptOf(b)
}

func runLabels(c checkrun.Context, p *pipelines.Pipeline, prep *prepared, opts startOptions) map[string]string {
	l := map[string]string{
		tekton.LabelManagedBy:    tekton.ManagedByValue,
		tekton.LabelPipeline:     p.Name,
		tekton.LabelEvent:        c.Event,
		tekton.LabelRepositoryID: strconv.FormatInt(c.Repository.ID, 10),
		tekton.LabelSHA:          c.Revision,
	}
	if prep.concurrency.Key != "" {
		l[tekton.LabelConcurrencyGroup] = tekton.GroupLabel(c.Repository.FullName, prep.concurrency.Key)
	}
	for k, v := range opts.Dedupe {
		l[k] = v
	}
	return l
}

func runAnnotations(c checkrun.Context, p *pipelines.Pipeline, prep *prepared, attempt int) map[string]string {
	a := map[string]string{
		tekton.AnnotationRepository:     c.Repository.FullName,
		tekton.AnnotationSHA:            c.Revision,
		tekton.AnnotationInstallationID: strconv.FormatInt(c.InstallationID, 10),
		tekton.AnnotationDeliveryID:     c.DeliveryID,
		tekton.AnnotationHead:           c.Head(),
		tekton.AnnotationAttempt:        strconv.Itoa(attempt),
	}
	if data, err := json.Marshal(c); err == nil {
		a[tekton.AnnotationContext] = string(data)
	}
	if prep.concurrency.Group != "" {
		a[tekton.AnnotationConcurrencyGroup] = prep.concurrency.Group
		a[tekton.AnnotationConcurrencyPolicy] = string(prep.concurrency.Policy)
	}
	if p.GitHubToken != nil {
		if data, err := json.Marshal(tokenConfig{Workspace: p.GitHubToken.Workspace, Permissions: p.TokenPermissions()}); err == nil {
			a[tekton.AnnotationToken] = string(data)
		}
	}
	if p.TaskChecks {
		a[tekton.AnnotationTaskChecks] = "true"
	}
	return a
}

// ContextOf returns the trigger context stored on a run.
func ContextOf(run *unstructured.Unstructured) (checkrun.Context, bool) {
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

func queuedSummary(c checkrun.Context, dashboard, namespace, name string) string {
	ref := "`" + namespace + "/" + name + "`"
	if u := checkrun.DashboardURL(dashboard, namespace, name); u != "" {
		ref = "[" + ref + "](" + u + ")"
	}
	return fmt.Sprintf("**PipelineRun:** %s\n\n**Trigger:** %s", ref, c.Describe())
}

// openCheck creates the run's check run (or takes over an existing one) and
// records it on the run.
func (s *Service) openCheck(ctx context.Context, gh githubapp.Client, c checkrun.Context, run *unstructured.Unstructured, id int64) (int64, error) {
	ns, name := run.GetNamespace(), run.GetName()
	external := ns + "/" + name
	output := &github.CheckRunOutput{
		Title:   new("Queued"),
		Summary: new(queuedSummary(c, s.DashboardURL, ns, name)),
		Text:    new(checkrun.WithMarker("", c)),
	}
	details := checkrun.DashboardURL(s.DashboardURL, ns, name)
	if id == 0 {
		opts := github.CreateCheckRunOptions{
			Name:       c.Pipeline,
			HeadSHA:    c.Revision,
			Status:     new(tekton.StatusQueued),
			ExternalID: new(external),
			Output:     output,
		}
		if details != "" {
			opts.DetailsURL = new(details)
		}
		cr, err := gh.CreateCheckRun(ctx, c.Repository.Owner, c.Repository.Name, opts)
		if err != nil {
			s.Metrics.CheckRunError(ctx, "create")
			return 0, err
		}
		id = cr.GetID()
	} else {
		upd := githubapp.CheckRunUpdate{ExternalID: new(external), Status: new(tekton.StatusQueued), Output: output}
		if details != "" {
			upd.DetailsURL = new(details)
		}
		if _, err := gh.UpdateCheckRun(ctx, c.Repository.Owner, c.Repository.Name, id, upd); err != nil {
			s.Metrics.CheckRunError(ctx, "update")
			return id, err
		}
	}
	idValue, queued := strconv.FormatInt(id, 10), tekton.ReportedQueued
	return id, s.Runs.Annotate(ctx, ns, name, map[string]*string{tekton.AnnotationCheckRunID: &idValue, tekton.AnnotationReported: &queued})
}

// TaskCheckIDs returns the check run of each task recorded on a run.
func TaskCheckIDs(run *unstructured.Unstructured) map[string]int64 {
	ids := map[string]int64{}
	if raw := run.GetAnnotations()[tekton.AnnotationTaskCheckIDs]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &ids)
	}
	return ids
}

// openTaskChecks opens a queued check for each task of the run's pipeline, named
// "<pipeline> / <task>", and records them on the run. With find, checks GitHub
// already has for the run are taken over instead of opened again.
func (s *Service) openTaskChecks(ctx context.Context, gh githubapp.Client, c checkrun.Context, run *unstructured.Unstructured, find bool) error {
	ns, name := run.GetNamespace(), run.GetName()
	external := ns + "/" + name
	ids := TaskCheckIDs(run)
	opened := false
	var err error
	for _, task := range tekton.TaskNames(run) {
		if ids[task] != 0 {
			continue
		}
		checkName := tekton.TaskCheckName(c.Pipeline, task)
		var id int64
		if find {
			if id, err = gh.FindCheckRun(ctx, c.Repository.Owner, c.Repository.Name, c.Revision, checkName, external); err != nil {
				break
			}
		}
		if id == 0 {
			opts := github.CreateCheckRunOptions{
				Name:       checkName,
				HeadSHA:    c.Revision,
				Status:     new(tekton.StatusQueued),
				ExternalID: new(external),
				Output: &github.CheckRunOutput{
					Title:   new("Queued"),
					Summary: new(fmt.Sprintf("Task `%s` of %s.", task, queuedSummary(c, s.DashboardURL, ns, name))),
					Text:    new(checkrun.WithMarker("", c)),
				},
			}
			if u := TaskURL(s.DashboardURL, ns, name, task); u != "" {
				opts.DetailsURL = new(u)
			}
			cr, cerr := gh.CreateCheckRun(ctx, c.Repository.Owner, c.Repository.Name, opts)
			if cerr != nil {
				s.Metrics.CheckRunError(ctx, "create")
				err = cerr
				break
			}
			id = cr.GetID()
		}
		ids[task], opened = id, true
	}
	if !opened {
		return err
	}
	data, merr := json.Marshal(ids)
	if merr == nil {
		value := string(data)
		merr = s.Runs.Annotate(ctx, ns, name, map[string]*string{tekton.AnnotationTaskCheckIDs: &value})
	}
	return errors.Join(err, merr)
}

// mintToken mints a repository-scoped installation token into the run's Secret.
func (s *Service) mintToken(ctx context.Context, c checkrun.Context, run *unstructured.Unstructured, permissions map[string]string) error {
	tok, err := s.GitHub.RepositoryToken(ctx, c.InstallationID, c.Repository.ID, permissions)
	if err != nil {
		return fmt.Errorf("minting the GitHub token: %w", err)
	}
	err = s.Runs.CreateTokenSecret(ctx, run, tekton.Token{Value: tok.Value, ExpiresAt: tok.ExpiresAt, Permissions: permissions}, map[string]string{
		tekton.AnnotationRepository:     c.Repository.FullName,
		tekton.AnnotationInstallationID: strconv.FormatInt(c.InstallationID, 10),
	})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// abort cancels a run that could not be started and fails its check.
func (s *Service) abort(ctx context.Context, gh githubapp.Client, c checkrun.Context, run *unstructured.Unstructured, checkID int64, cause error) {
	ns, name := run.GetNamespace(), run.GetName()
	log := s.logFor(c).With("namespace", ns, "name", name)
	log.Error("Could not start the run; cancelling it", "error", cause)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.Runs.Cancel(ctx, ns, name, map[string]string{tekton.AnnotationCancelReason: "could not be started"}); err != nil {
		log.Error("Could not cancel the run", "error", err)
	}
	if checkID != 0 {
		s.failCheckRun(ctx, gh, c, checkID, "The run could not be started",
			fmt.Sprintf("Octomaton could not start PipelineRun `%s/%s`:\n\n```\n%v\n```\n\nRe-run this check to try again.", ns, name, cause))
	}
	// The failure is reported; the reporter must not report the cancellation over it.
	if err := s.Runs.Label(ctx, ns, name, map[string]string{tekton.LabelDone: "true"}, map[string]string{tekton.AnnotationReported: tekton.ReportedCompleted}); err != nil {
		log.Error("Could not mark the run as reported", "error", err)
	}
}

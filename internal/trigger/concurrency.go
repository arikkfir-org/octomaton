package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/arikkfir-org/switchboard/internal/checkrun"
	"github.com/arikkfir-org/switchboard/internal/githubapp"
	"github.com/arikkfir-org/switchboard/internal/repoconfig"
	"github.com/arikkfir-org/switchboard/internal/tekton"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// SupersededByHead prefixes the superseded-by annotation of a run stood down
// because its commit is no longer the head of its branch or pull request.
const SupersededByHead = "head:"

func groupSelector(key string) string {
	return tekton.LabelConcurrencyGroup + "=" + key + ",!" + tekton.LabelDone
}

// release lets a held run start, subject to its concurrency policy: supersede
// stands down all but the newest run of the group, queue waits for the others,
// and latest waits too but keeps only the newest waiting run.
func (s *Service) release(ctx context.Context, run *unstructured.Unstructured) error {
	ns, name := run.GetNamespace(), run.GetName()
	key := run.GetLabels()[tekton.LabelConcurrencyGroup]
	if key == "" {
		return s.Runs.SetStatus(ctx, ns, name, "")
	}
	switch run.GetAnnotations()[tekton.AnnotationConcurrencyPolicy] {
	case repoconfig.PolicySupersede:
		return s.supersede(ctx, ns, key, name)
	case repoconfig.PolicyLatest:
		waits, err := s.keepLatest(ctx, ns, key, name)
		if err != nil || !waits {
			return err
		}
	}
	head, err := s.queueHead(ctx, ns, key)
	if err != nil {
		return err
	}
	if head != name {
		return nil // stays held; released when the runs ahead of it finish
	}
	return s.Runs.SetStatus(ctx, ns, name, "")
}

// supersede releases a run whose group keeps only its newest run going, and
// stands the others down. Newest means the head of its branch or pull request,
// not the order deliveries arrived in: a run whose commit is no longer the head
// stands itself down, so a late delivery for an old commit never stops the
// head's run (whose "skipped" check would read as passed). Of one commit's
// runs, the one that runs is the one whose check GitHub shows: the newest check,
// then the later attempt. A rival still opening its check cannot be ranked yet,
// so the run waits, held, and the rival decides for both when it releases.
func (s *Service) supersede(ctx context.Context, ns, key, name string) error {
	run, err := s.Runs.Get(ctx, ns, name)
	if err != nil || run == nil || tekton.CancelRequested(run) || tekton.IsDone(run) {
		return err
	}
	all, err := s.Runs.List(ctx, ns, groupSelector(key))
	if err != nil {
		return fmt.Errorf("listing the runs of the concurrency group: %w", err)
	}
	var rivals []*unstructured.Unstructured
	for i := range all {
		if r := &all[i]; r.GetName() != name && !tekton.IsDone(r) && !tekton.CancelRequested(r) {
			rivals = append(rivals, r)
		}
	}
	head, known, err := s.currentHead(ctx, run)
	if err != nil {
		return err
	}
	sha := run.GetLabels()[tekton.LabelSHA]
	if known && sha != head {
		return s.supersededBy(ctx, ns, name, SupersededByHead+head)
	}
	first := run
	for _, r := range rivals {
		switch {
		case r.GetLabels()[tekton.LabelSHA] != sha:
		case checkPending(r):
			waitFor := r.GetName()
			return s.Runs.Annotate(ctx, ns, name, map[string]*string{tekton.AnnotationWaitingFor: &waitFor})
		case outranks(r, first):
			first = r
		}
	}
	if first != run {
		if err := s.supersededBy(ctx, ns, name, first.GetName()); err != nil {
			return err
		}
		if tekton.IsPending(first) && first.GetAnnotations()[tekton.AnnotationWaitingFor] != "" {
			return s.supersede(ctx, ns, key, first.GetName())
		}
		return nil
	}
	for _, r := range rivals {
		if err := s.supersededBy(ctx, ns, r.GetName(), name); err != nil {
			return fmt.Errorf("superseding %s: %w", r.GetName(), err)
		}
	}
	return s.Runs.SetStatus(ctx, ns, name, "")
}

// keepLatest leaves one run waiting in a latest group, the newest, and reports
// whether that is this run. Newer is the head's run first, then the later
// created run, then the greater name (a total order, so two runs releasing at
// once agree). The running run is not touched, except that a run for another
// commit stands down when the head's run is already running.
func (s *Service) keepLatest(ctx context.Context, ns, key, name string) (bool, error) {
	run, err := s.Runs.Get(ctx, ns, name)
	if err != nil || run == nil || tekton.CancelRequested(run) || tekton.IsDone(run) {
		return false, err
	}
	all, err := s.Runs.List(ctx, ns, groupSelector(key))
	if err != nil {
		return false, fmt.Errorf("listing the runs of the concurrency group: %w", err)
	}
	var held, running []*unstructured.Unstructured
	for i := range all {
		r := &all[i]
		switch {
		case r.GetName() == name || tekton.IsDone(r) || tekton.CancelRequested(r):
		case tekton.IsPending(r):
			held = append(held, r)
		default:
			running = append(running, r)
		}
	}
	head, known, err := s.currentHead(ctx, run)
	if err != nil {
		return false, err
	}
	if known && run.GetLabels()[tekton.LabelSHA] != head {
		for _, r := range running {
			if r.GetLabels()[tekton.LabelSHA] == head {
				return false, s.supersededBy(ctx, ns, name, r.GetName())
			}
		}
	}
	newer := func(a, b *unstructured.Unstructured) bool {
		if ah, bh := a.GetLabels()[tekton.LabelSHA] == head, b.GetLabels()[tekton.LabelSHA] == head; known && ah != bh {
			return ah
		}
		if at, bt := a.GetCreationTimestamp().Time, b.GetCreationTimestamp().Time; !at.Equal(bt) {
			return at.After(bt)
		}
		return a.GetName() > b.GetName()
	}
	for _, r := range held {
		if newer(r, run) {
			return false, s.supersededBy(ctx, ns, name, r.GetName())
		}
	}
	for _, r := range held {
		// A run still opening its check stands itself down when it releases.
		if checkPending(r) {
			continue
		}
		if err := s.supersededBy(ctx, ns, r.GetName(), name); err != nil {
			return false, fmt.Errorf("superseding %s: %w", r.GetName(), err)
		}
	}
	return true, nil
}

// queueHead returns the held run of a group that may start: none while a run of
// the group is running, else the oldest held one (creation time, then name).
// Held runs still opening their check are passed over: their own start (or
// Resume) releases them once the check exists.
func (s *Service) queueHead(ctx context.Context, ns, key string) (string, error) {
	runs, err := s.Runs.List(ctx, ns, groupSelector(key))
	if err != nil {
		return "", fmt.Errorf("listing the runs of the concurrency group: %w", err)
	}
	var head *unstructured.Unstructured
	for i := range runs {
		r := &runs[i]
		switch {
		case tekton.IsDone(r):
		case !tekton.IsPending(r):
			return "", nil
		case checkPending(r):
		case head == nil || createdBefore(r, head):
			head = r
		}
	}
	if head == nil {
		return "", nil
	}
	return head.GetName(), nil
}

func createdBefore(a, b *unstructured.Unstructured) bool {
	at, bt := a.GetCreationTimestamp().Time, b.GetCreationTimestamp().Time
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.GetName() < b.GetName()
}

// checkPending reports whether a run is still opening its check.
func checkPending(run *unstructured.Unstructured) bool {
	return run.GetAnnotations()[tekton.AnnotationCheckRunID] == ""
}

func (s *Service) supersededBy(ctx context.Context, ns, name, by string) error {
	s.Logger.Info("Superseding run", "namespace", ns, "name", name, "supersededBy", by)
	return s.Runs.Cancel(ctx, ns, name, map[string]string{tekton.AnnotationSupersededBy: by})
}

// currentHead returns the commit at the head of the run's pull request or
// branch; known is false when there is no such head (tags, merge groups, a
// branch or pull request that is gone).
func (s *Service) currentHead(ctx context.Context, run *unstructured.Unstructured) (string, bool, error) {
	c, ok := ContextOf(run)
	if !ok {
		return "", false, nil
	}
	gh := s.GitHub.Installation(c.InstallationID)
	owner, repo := c.Repository.Owner, c.Repository.Name
	switch {
	case c.PullRequest != nil:
		pr, err := gh.PullRequest(ctx, owner, repo, c.PullRequest.Number)
		if errors.Is(err, githubapp.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return pr.HeadSHA, true, nil
	case c.MergeGroup != nil || c.Branch == "":
		return "", false, nil
	default:
		sha, err := gh.BranchHead(ctx, owner, repo, c.Branch)
		if errors.Is(err, githubapp.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return sha, true, nil
	}
}

// ReleaseNext starts the next held run of a finished run's queue or latest group.
func (s *Service) ReleaseNext(ctx context.Context, run *unstructured.Unstructured) error {
	key := run.GetLabels()[tekton.LabelConcurrencyGroup]
	if key == "" || run.GetAnnotations()[tekton.AnnotationConcurrencyPolicy] == repoconfig.PolicySupersede {
		return nil
	}
	next, err := s.queueHead(ctx, run.GetNamespace(), key)
	if err != nil || next == "" {
		return err
	}
	s.Logger.Info("Releasing the next run of the concurrency group", "namespace", run.GetNamespace(), "finished", run.GetName(), "next", next)
	return s.Runs.SetStatus(ctx, run.GetNamespace(), next, "")
}

// Resume finishes starting a run that has been held for a while: its start may
// have been cut short (a pod restart) or it waits its turn in a queue. It opens
// the check (taking over one GitHub already has), the task checks and the token
// Secret when missing, then applies the concurrency policy again.
func (s *Service) Resume(ctx context.Context, run *unstructured.Unstructured) error {
	c, ok := ContextOf(run)
	if !ok {
		return fmt.Errorf("PipelineRun %s/%s has no trigger context", run.GetNamespace(), run.GetName())
	}
	ns, name := run.GetNamespace(), run.GetName()
	gh := s.GitHub.Installation(c.InstallationID)
	ann := run.GetAnnotations()
	if ann[tekton.AnnotationReported] == "" {
		id := checkRunIDOf(run)
		if id == 0 {
			found, err := gh.FindCheckRun(ctx, c.Repository.Owner, c.Repository.Name, c.Revision, c.Pipeline, ns+"/"+name)
			if err != nil {
				return fmt.Errorf("finding the check run: %w", err)
			}
			id = found
		}
		if _, err := s.openCheck(ctx, gh, c, run, id); err != nil {
			return fmt.Errorf("opening the check run: %w", err)
		}
	}
	if ann[tekton.AnnotationTaskChecks] == "true" {
		if err := s.openTaskChecks(ctx, gh, c, run, true); err != nil {
			return fmt.Errorf("opening the task checks: %w", err)
		}
	}
	if raw := ann[tekton.AnnotationToken]; raw != "" {
		secret, err := s.Runs.TokenSecret(ctx, ns, name)
		if err != nil {
			return err
		}
		if secret == nil {
			var cfg tokenConfig
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				return fmt.Errorf("reading the token settings: %w", err)
			}
			if err := s.mintToken(ctx, c, run, cfg.Permissions); err != nil {
				return err
			}
		}
	}
	return s.release(ctx, run)
}

// CancelMergeGroup cancels the unfinished runs of a destroyed merge group.
func (s *Service) CancelMergeGroup(ctx context.Context, c checkrun.Context, reason string) {
	log := s.logFor(c)
	ns, err := s.Namespaces.Resolve(c.TemplateRepository())
	if err != nil {
		log.Error("Could not resolve namespace", "error", err)
		return
	}
	selector := labels.SelectorFromSet(map[string]string{
		tekton.LabelManagedBy:    tekton.ManagedByValue,
		tekton.LabelEvent:        checkrun.EventMergeGroup,
		tekton.LabelRepositoryID: strconv.FormatInt(c.Repository.ID, 10),
		tekton.LabelSHA:          c.Revision,
	}).String()
	items, err := s.Runs.List(ctx, ns, selector)
	if err != nil {
		log.Error("Could not list merge group runs", "error", err)
		return
	}
	why := "merge group destroyed"
	if reason != "" {
		why += " (" + reason + ")"
	}
	for i := range items {
		item := &items[i]
		if tekton.IsDone(item) || tekton.CancelRequested(item) {
			continue
		}
		if err := s.Runs.Cancel(ctx, ns, item.GetName(), map[string]string{tekton.AnnotationCancelReason: why}); err != nil {
			log.Error("Could not cancel merge group run", "name", item.GetName(), "error", err)
			continue
		}
		log.Info("Cancelled merge group run", "name", item.GetName(), "reason", reason)
	}
}

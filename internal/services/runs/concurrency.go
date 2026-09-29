package runs

import (
	"context"
	"errors"
	"fmt"

	"octomaton.dev/internal/services/ci"
)

// release lets a held run go, per its concurrency policy: supersede stands down all but the newest
// run of the group, queue waits for the others, and latest waits too but keeps only the newest
// waiting run.
func (s *Service) release(ctx context.Context, run ci.Run) error {
	if run.Group == "" {
		return s.Runner.Release(ctx, run.ID)
	}
	switch run.Policy {
	case ci.Supersede:
		return s.supersede(ctx, run.ID)
	case ci.Latest:
		waits, err := s.keepLatest(ctx, run.ID)
		if err != nil || !waits {
			return err
		}
	}
	next, err := s.queueHead(ctx, run)
	if err != nil || next != run.ID {
		return err // stays held; let go when the runs ahead of it finish
	}
	return s.Runner.Release(ctx, run.ID)
}

// groupRuns lists the live runs of a run's concurrency group.
func (s *Service) groupRuns(ctx context.Context, run ci.Run) ([]ci.Run, error) {
	runs, err := s.Runner.List(ctx, ci.RunQuery{Repository: &run.Trigger.Repository, Group: run.Group, Live: true})
	if err != nil {
		return nil, fmt.Errorf("listing the runs of the concurrency group: %w", err)
	}
	return runs, nil
}

// get reads a run; found is false when it is gone.
func (s *Service) get(ctx context.Context, id ci.RunID) (ci.Run, bool, error) {
	run, err := s.Runner.Get(ctx, id)
	if errors.Is(err, ci.ErrNotFound) {
		return ci.Run{}, false, nil
	}
	return run, err == nil, err
}

// supersede lets a run go whose group keeps only its newest run going, and stands the others down.
// Newest means the head of its branch or pull request, not the order deliveries arrived in: a run
// whose commit is no longer the head stands itself down, so a late delivery for an old commit never
// stops the head's run (whose "skipped" report would read as passed). Of one commit's runs, the one
// that runs is the one whose report the code host shows: the newest report, then the later attempt.
// A rival still opening its report cannot be ranked yet, so the run waits, held, and the rival
// decides for both when it goes.
func (s *Service) supersede(ctx context.Context, id ci.RunID) error {
	run, found, err := s.get(ctx, id)
	if err != nil || !found || run.CancelRequested || run.Phase == ci.Finished {
		return err
	}
	all, err := s.groupRuns(ctx, run)
	if err != nil {
		return err
	}
	var rivals []ci.Run
	for _, r := range all {
		if r.ID != run.ID && r.Phase != ci.Finished && !r.CancelRequested {
			rivals = append(rivals, r)
		}
	}
	head, known, err := s.currentHead(ctx, run.Trigger)
	if err != nil {
		return err
	}
	sha := run.Trigger.Revision
	if known && sha != head {
		return s.supersededBy(ctx, run.ID, ci.Cancellation{NewerCommit: head})
	}
	first := run
	for _, r := range rivals {
		switch {
		case r.Trigger.Revision != sha:
		case r.ReportID == 0:
			return s.Runner.Record(ctx, run.ID, ci.Record{WaitingFor: &r.ID.Name})
		case outranks(r, first):
			first = r
		}
	}
	if first.ID != run.ID {
		if err := s.supersededBy(ctx, run.ID, ci.Cancellation{SupersededBy: first.ID.Name}); err != nil {
			return err
		}
		if first.Phase == ci.Held && first.WaitingFor != "" {
			return s.supersede(ctx, first.ID)
		}
		return nil
	}
	for _, r := range rivals {
		if err := s.supersededBy(ctx, r.ID, ci.Cancellation{SupersededBy: run.ID.Name}); err != nil {
			return fmt.Errorf("superseding %s: %w", r.ID, err)
		}
	}
	return s.Runner.Release(ctx, run.ID)
}

// keepLatest leaves one run waiting in a latest group, the newest, and reports whether that is this
// run. Newer is the head's run first, then the later created run, then the greater name (a total
// order, so two runs going at once agree). The running run is not touched, except that a run for
// another commit stands down when the head's run is already running.
func (s *Service) keepLatest(ctx context.Context, id ci.RunID) (bool, error) {
	run, found, err := s.get(ctx, id)
	if err != nil || !found || run.CancelRequested || run.Phase == ci.Finished {
		return false, err
	}
	all, err := s.groupRuns(ctx, run)
	if err != nil {
		return false, err
	}
	var held, going []ci.Run
	for _, r := range all {
		switch {
		case r.ID == run.ID || r.Phase == ci.Finished || r.CancelRequested:
		case r.Phase == ci.Held:
			held = append(held, r)
		default:
			going = append(going, r)
		}
	}
	head, known, err := s.currentHead(ctx, run.Trigger)
	if err != nil {
		return false, err
	}
	if known && run.Trigger.Revision != head {
		for _, r := range going {
			if r.Trigger.Revision == head {
				return false, s.supersededBy(ctx, run.ID, ci.Cancellation{SupersededBy: r.ID.Name})
			}
		}
	}
	newer := func(a, b ci.Run) bool {
		if ah, bh := a.Trigger.Revision == head, b.Trigger.Revision == head; known && ah != bh {
			return ah
		}
		if !a.Created.Equal(b.Created) {
			return a.Created.After(b.Created)
		}
		return a.ID.Name > b.ID.Name
	}
	for _, r := range held {
		if newer(r, run) {
			return false, s.supersededBy(ctx, run.ID, ci.Cancellation{SupersededBy: r.ID.Name})
		}
	}
	for _, r := range held {
		// A run still opening its report stands itself down when it goes.
		if r.ReportID == 0 {
			continue
		}
		if err := s.supersededBy(ctx, r.ID, ci.Cancellation{SupersededBy: run.ID.Name}); err != nil {
			return false, fmt.Errorf("superseding %s: %w", r.ID, err)
		}
	}
	return true, nil
}

// queueHead returns the held run of a run's group that may go: none while a run of the group is
// going, else the oldest held one (creation time, then name). Held runs still opening their report
// are passed over: their own start (or Resume) lets them go once the report exists.
func (s *Service) queueHead(ctx context.Context, run ci.Run) (ci.RunID, error) {
	runs, err := s.groupRuns(ctx, run)
	if err != nil {
		return ci.RunID{}, err
	}
	var head *ci.Run
	for i := range runs {
		r := &runs[i]
		switch {
		case r.Phase == ci.Finished:
		case r.Phase != ci.Held:
			return ci.RunID{}, nil
		case r.ReportID == 0:
		case head == nil || createdBefore(*r, *head):
			head = r
		}
	}
	if head == nil {
		return ci.RunID{}, nil
	}
	return head.ID, nil
}

func createdBefore(a, b ci.Run) bool {
	if !a.Created.Equal(b.Created) {
		return a.Created.Before(b.Created)
	}
	return a.ID.Name < b.ID.Name
}

func (s *Service) supersededBy(ctx context.Context, id ci.RunID, why ci.Cancellation) error {
	s.Logger.InfoContext(ctx, "Superseding run", "run", id.String(), "supersededBy", why.SupersededBy, "newerCommit", why.NewerCommit)
	return s.Runner.Cancel(ctx, id, why)
}

// currentHead returns the commit at the head of the trigger's pull request or branch; known is
// false when there is no such head (tags, merge groups, a branch or pull request that is gone).
func (s *Service) currentHead(ctx context.Context, t ci.Trigger) (string, bool, error) {
	gh := s.Host.Installation(t.InstallationID)
	var (
		sha string
		err error
	)
	switch {
	case t.PullRequest != nil:
		var pr ci.PullRequestState
		pr, err = gh.PullRequest(ctx, t.Repository, t.PullRequest.Number)
		sha = pr.HeadSHA
	case t.MergeGroup != nil || t.Branch == "":
		return "", false, nil
	default:
		sha, err = gh.BranchHead(ctx, t.Repository, t.Branch)
	}
	if errors.Is(err, ci.ErrNotFound) {
		return "", false, nil
	}
	return sha, err == nil, err
}

// ReleaseNext lets the next held run of a finished run's queue or latest group go.
func (s *Service) ReleaseNext(ctx context.Context, run ci.Run) error {
	if run.Group == "" || run.Policy == ci.Supersede {
		return nil
	}
	next, err := s.queueHead(ctx, run)
	if err != nil || next.Name == "" {
		return err
	}
	s.Logger.InfoContext(ctx, "Releasing the next run of the concurrency group", "finished", run.ID.String(), "next", next.String())
	return s.Runner.Release(ctx, next)
}

// Resume finishes starting a run that has been held for a while: its start may have been cut short
// (a restart), or it waits its turn in a queue. It opens the report (taking over one the code host
// already has), the task reports and the token when missing, then applies the concurrency policy
// again.
func (s *Service) Resume(ctx context.Context, run ci.Run) error {
	t := run.Trigger
	if t.Version != ci.TriggerVersion {
		return fmt.Errorf("run %s does not know its trigger", run.ID)
	}
	gh := s.Host.Installation(t.InstallationID)
	if run.Reported == "" {
		id := run.ReportID
		if id == 0 {
			found, err := gh.FindReport(ctx, t.Repository, t.Revision, t.Pipeline, run.ID.String())
			if err != nil {
				return fmt.Errorf("finding the report: %w", err)
			}
			id = found
		}
		if _, err := s.openReport(ctx, gh, run, id); err != nil {
			return fmt.Errorf("opening the report: %w", err)
		}
	}
	if run.TaskReports {
		if err := s.openTaskReports(ctx, gh, run, true); err != nil {
			return fmt.Errorf("opening the task reports: %w", err)
		}
	}
	if run.Token != nil {
		_, ok, err := s.Runner.TokenExpiry(ctx, run.ID)
		if err != nil {
			return err
		}
		if !ok {
			if err := s.mintToken(ctx, run); err != nil {
				return err
			}
		}
	}
	return s.release(ctx, run)
}

// CancelMergeGroup cancels the unfinished runs of a merge group the queue dropped.
func (s *Service) CancelMergeGroup(ctx context.Context, t ci.Trigger, reason string) {
	log := s.logFor(t)
	runs, err := s.Runner.List(ctx, ci.RunQuery{Repository: &t.Repository, Event: ci.EventMergeGroup, Revision: t.Revision})
	if err != nil {
		log.ErrorContext(ctx, "Could not list the merge group's runs", "error", err)
		return
	}
	why := "merge group destroyed"
	if reason != "" {
		why += " (" + reason + ")"
	}
	for _, r := range runs {
		if r.Phase == ci.Finished || r.CancelRequested {
			continue
		}
		if err := s.Runner.Cancel(ctx, r.ID, ci.Cancellation{Reason: why}); err != nil {
			log.ErrorContext(ctx, "Could not cancel the merge group's run", "run", r.ID.String(), "error", err)
			continue
		}
		log.InfoContext(ctx, "Cancelled the merge group's run", "run", r.ID.String(), "reason", reason)
	}
}

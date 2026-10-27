package reports

import (
	"context"
	"strings"
	"time"

	"octomaton.dev/internal/services/ci"
)

const (
	// liveInterval is how often a running task's output is read for its live report, which is
	// written at most as often.
	liveInterval = 15 * time.Second
	// liveSlack reads output from a little before the last look: the clocks of the step's node and of
	// Octomaton's may disagree.
	liveSlack = 5 * time.Second
	// liveWriteLimit bounds a live report's write: it is best effort, and must not hold the reports
	// of other runs, which wait for the same workers, behind a slow or rate-limited code host.
	liveWriteLimit = 10 * time.Second
	// liveForgotten is how long a task's live state outlives its last try: by then, its run is
	// over or another replica reports it.
	liveForgotten = 10 * time.Minute
)

// liveNames are the output lines a live report shows: the results that end it too.
var liveNames = []string{ResultTitle, ResultSummary}

// liveKey names one task of a run.
type liveKey struct {
	run  ci.RunID
	task string
}

// live is what one running task printed and what of it its report shows.
type live struct {
	// tried is when its output was last read or tried to be; looked when it was last read, and read
	// names the steps it had then.
	tried, looked time.Time
	read          map[string]bool
	// title and summary are the newest the task printed; written* what its report shows.
	title, summary               string
	writtenTitle, writtenSummary string
}

// liveTaskReports has the report of each running task of a run with live task reports show the
// title and summary the task printed last. It returns when to look again (zero: no task runs).
// What it fails to read or write is logged and tried again at the next look: a live report is
// best effort, and never holds the run's other reports.
func (s *Service) liveTaskReports(ctx context.Context, run ci.Run, details ci.Details) time.Duration {
	if !run.LiveTaskReports || len(run.TaskReportIDs) == 0 {
		return 0
	}
	now := s.now()
	var again time.Duration
	running := map[string]bool{}
	for _, t := range details.Tasks {
		id := run.TaskReportIDs[t.Name]
		if id == 0 || t.State != ci.TaskRunning {
			continue
		}
		running[t.Name] = true
		if wait := s.liveTaskReport(ctx, run, id, t, now); again == 0 || wait < again {
			again = wait
		}
	}
	s.forgetLive(func(k liveKey, l live) bool {
		return k.run == run.ID && !running[k.task] || now.Sub(l.tried) > liveForgotten
	})
	return again
}

// liveTaskReport reads what one running task printed since the last look, and writes its report
// when that changed what it shows. It returns when to look again.
func (s *Service) liveTaskReport(ctx context.Context, run ci.Run, id ci.ReportID, t ci.Task, now time.Time) time.Duration {
	key := liveKey{run: run.ID, task: t.Name}
	l := s.liveState(key)
	if waited := now.Sub(l.tried); waited < liveInterval {
		return liveInterval - waited
	}
	reads := liveReads(l, t.Steps)
	if len(reads) == 0 && l.looked.IsZero() {
		return liveInterval // no step started yet
	}
	l.tried = now
	defer func() { s.setLive(key, l) }()
	log := s.Logger.With("run", run.ID.String(), "task", t.Name)
	if len(reads) > 0 {
		out, err := s.Runner.TaskOutput(ctx, run.ID, reads, liveNames)
		if err != nil {
			log.WarnContext(ctx, "Reading a task's output for its live check failed", "error", err)
			return liveInterval
		}
		if v := strings.TrimSpace(out[ResultTitle]); v != "" {
			l.title = v
		}
		if v := strings.TrimSpace(out[ResultSummary]); v != "" {
			l.summary = v
		}
	}
	l.looked, l.read = now, map[string]bool{}
	for _, step := range t.Steps {
		l.read[step.Name] = true
	}
	if l.title == l.writtenTitle && l.summary == l.writtenSummary {
		return liveInterval
	}
	title, summary := l.title, l.summary
	if title == "" {
		title = "Running"
	}
	if u := s.Runner.TaskURL(run.ID, t.Name); u != "" {
		summary = strings.TrimSpace(summary + "\n\n[The task on the Dashboard](" + u + ").")
	}
	// No status: a write that lands after the task's report concluded leaves its conclusion alone.
	r := ci.Report{Title: title, Summary: summary, Trigger: storedTrigger(run)}
	wctx, cancel := context.WithTimeout(ctx, liveWriteLimit)
	defer cancel()
	if err := s.installation(run).UpdateReport(wctx, run.Trigger.Repository, id, r); err != nil {
		log.WarnContext(ctx, "Writing a task's live check failed", "error", err)
		return liveInterval
	}
	l.writtenTitle, l.writtenSummary = l.title, l.summary
	return liveInterval
}

// liveReads is what to read of a task's started steps: a step read before, since a little before
// the last look, unless it ended before that; a step not read before (it started since, or this
// replica never looked), everything it printed.
func liveReads(l live, steps []ci.Step) []ci.OutputRead {
	since := l.looked.Add(-liveSlack)
	var reads []ci.OutputRead
	for _, step := range steps {
		switch {
		case !l.read[step.Name]:
			reads = append(reads, ci.OutputRead{Step: step})
		case step.Finished.IsZero() || !step.Finished.Before(since):
			reads = append(reads, ci.OutputRead{Step: step, Since: since})
		}
	}
	return reads
}

// liveState returns a task's live state; the zero state when there is none.
func (s *Service) liveState(k liveKey) live {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.live[k]
}

// setLive records a task's live state.
func (s *Service) setLive(k liveKey, l live) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live == nil {
		s.live = map[liveKey]live{}
	}
	s.live[k] = l
}

// forgetLive drops the live states drop selects.
func (s *Service) forgetLive(drop func(liveKey, live) bool) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	for k, l := range s.live {
		if drop(k, l) {
			delete(s.live, k)
		}
	}
}

// forgetRun drops the live states of a run's tasks.
func (s *Service) forgetRun(id ci.RunID) {
	s.forgetLive(func(k liveKey, _ live) bool { return k.run == id })
}

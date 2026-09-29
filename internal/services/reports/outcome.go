package reports

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"octomaton.dev/internal/services/ci"
)

const (
	logTailLines   = 50
	logLimitBytes  = 8 * 1024
	maxLogSections = 5

	// ResultTitle and ResultSummary are pipeline or task results that override a report's title and
	// summary.
	ResultTitle   = "check-title"
	ResultSummary = "check-summary"
)

var stateLabels = map[ci.TaskState]string{
	ci.TaskPending:   "⬜ Pending",
	ci.TaskRunning:   "⏳ Running",
	ci.TaskSucceeded: "✅ Succeeded",
	ci.TaskFailed:    "❌ Failed",
	ci.TaskCancelled: "⏹️ Cancelled",
	ci.TaskTimedOut:  "⌛ Timed out",
	ci.TaskSkipped:   "⬜ Skipped",
}

// titles name a finished run's conclusion.
var titles = map[ci.Conclusion]string{
	ci.Success:   "Succeeded",
	ci.Failure:   "Failed",
	ci.Cancelled: "Cancelled",
	ci.TimedOut:  "Timed out",
	ci.Skipped:   "Superseded",
}

// outcome is how a finished run ended, as its report shows it.
type outcome struct {
	conclusion ci.Conclusion
	title      string
	summary    string
	text       string
}

// Title returns the title of a finished run's report, from its conclusion and how long it ran.
func Title(c ci.Conclusion, d time.Duration) string {
	switch c {
	case ci.Success:
		return "Succeeded in " + formatDuration(d)
	case ci.Skipped:
		return titles[c]
	default:
		return titles[c] + " after " + formatDuration(d)
	}
}

// conclusionOf is how a finished run concludes: a run Octomaton superseded is skipped, unless it
// succeeded first.
func conclusionOf(run ci.Run) ci.Conclusion {
	if c := run.Outcome.Conclusion; c != ci.Success && run.Cancellation.Superseded() {
		return ci.Skipped
	}
	return run.Outcome.Conclusion
}

func (s *Service) outcome(ctx context.Context, run ci.Run, details ci.Details) outcome {
	c := conclusionOf(run)
	o := outcome{conclusion: c, title: Title(c, elapsed(run.Started, run.Finished, s.now()))}
	if t := strings.TrimSpace(details.Results[ResultTitle]); t != "" {
		o.title = t
	}
	var b strings.Builder
	if summary := strings.TrimSpace(details.Results[ResultSummary]); summary != "" {
		b.WriteString(summary + "\n\n")
	}
	b.WriteString(s.header(run))
	why := run.Cancellation
	switch {
	case c == ci.Skipped && why.NewerCommit != "":
		fmt.Fprintf(&b, "\nSuperseded by a newer commit, `%s`.\n", ci.ShortSHA(why.NewerCommit))
	case c == ci.Skipped && why.SupersededBy != "":
		link := "`" + why.SupersededBy + "`"
		if u := s.Runner.Link(ci.RunID{Tenant: run.ID.Tenant, Name: why.SupersededBy}).URL; u != "" {
			link = "[" + link + "](" + u + ")"
		}
		fmt.Fprintf(&b, "\nSuperseded by a newer run, %s.\n", link)
	case c == ci.Cancelled && why.Reason != "":
		fmt.Fprintf(&b, "\nCancelled by Octomaton: %s.\n", why.Reason)
	case c != ci.Success && run.Outcome.Message != "":
		fmt.Fprintf(&b, "\n> %s\n", oneLine(run.Outcome.Message))
	}
	var failed []string
	for _, t := range details.Tasks {
		if t.State == ci.TaskFailed || t.State == ci.TaskTimedOut {
			failed = append(failed, "`"+t.Name+"`")
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\nFailed: %s.\n", strings.Join(failed, ", "))
	}
	if len(details.Tasks) > 0 {
		b.WriteString(table(details.Tasks, s.now()))
	}
	o.summary = b.String()
	if c == ci.Failure || c == ci.TimedOut {
		o.text = s.failureLogs(ctx, run, details.Tasks)
	}
	return o
}

// table renders the task table.
func table(tasks []ci.Task, now time.Time) string {
	var b strings.Builder
	b.WriteString("\n| Task | State | Time |\n| --- | --- | --- |\n")
	for _, t := range tasks {
		state := stateLabels[t.State]
		if t.Note != "" {
			state += " (" + t.Note + ")"
		}
		took := ""
		if t.State.Finished() && !t.Started.IsZero() {
			took = formatDuration(elapsed(t.Started, t.Finished, now))
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", cell(t.Name), cell(state), took)
	}
	return b.String()
}

// failureLogs renders the log tails of failed steps.
func (s *Service) failureLogs(ctx context.Context, run ci.Run, tasks []ci.Task) string {
	var b strings.Builder
	sections := 0
	for _, t := range tasks {
		if t.State != ci.TaskFailed && t.State != ci.TaskTimedOut {
			continue
		}
		if len(t.FailedSteps) == 0 {
			if t.Message != "" && sections < maxLogSections {
				fmt.Fprintf(&b, "### %s\n\n> %s\n\n", t.Name, oneLine(t.Message))
				sections++
			}
			continue
		}
		for _, step := range t.FailedSteps {
			if sections >= maxLogSections {
				b.WriteString("_Logs of further failed steps are omitted._\n")
				return b.String()
			}
			sections++
			fmt.Fprintf(&b, "### %s › %s (exit code %d)\n\n", t.Name, step.Name, step.ExitCode)
			logs, err := s.Runner.StepLogs(ctx, run.ID, step, logTailLines, logLimitBytes*2)
			if err != nil {
				fmt.Fprintf(&b, "_Logs unavailable: %s._\n\n", strings.TrimSuffix(oneLine(err.Error()), "."))
				continue
			}
			logs = truncateHead(logs, logLimitBytes)
			fence := codeFence(logs)
			fmt.Fprintf(&b, "%stext\n%s\n%s\n\n", fence, strings.TrimRight(logs, "\n"), fence)
		}
	}
	return b.String()
}

// elapsed is the time between start and end, or now when end is zero; zero without a start.
func elapsed(start, end, now time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	if end.IsZero() {
		end = now
	}
	return end.Sub(start)
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return d.Round(time.Second).String()
}

// truncateHead keeps the last bytes of s (at most limit), never splitting a UTF-8 sequence.
func truncateHead(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	start := len(s) - limit
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
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

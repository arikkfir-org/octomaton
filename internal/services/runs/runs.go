// Package runs turns events into runs: it reads .octomaton.yaml for a trigger, starts the pipelines
// it matches (weighing changed paths), lets runs go per their concurrency policy, re-runs reports,
// and runs pull request comment commands. It never acts on pull requests from forks.
package runs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
	"octomaton.dev/internal/system/metrics"
)

// ScheduleNotifier is told when a repository's default branch changes, so its schedules are read
// again; *schedules.Scheduler implements it.
type ScheduleNotifier interface {
	Notify(installationID int64, repo ci.Repository)
}

// Service turns events into runs.
type Service struct {
	Host   ci.CodeHost
	Runner ci.Runner
	// OrganizationRepository names the repository, in each owner, whose .octomaton.yaml declares
	// organization pipelines; empty disables them.
	OrganizationRepository string
	// Schedules, when set, is told about pushes to default branches.
	Schedules ScheduleNotifier
	Logger    *slog.Logger
	Metrics   *metrics.Metrics
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// Refusal is a pipeline Octomaton would not start. Its reason is reported (for comment commands, in
// the reply).
type Refusal struct {
	Pipeline string
	Reason   string
	// Cause is the failed call that refused the run or kept its report from opening, which a retry may
	// get past; nil when the run itself is refused.
	Cause error
}

func (r *Refusal) Error() string { return r.Pipeline + ": " + r.Reason }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logFor(t ci.Trigger) *slog.Logger {
	l := s.Logger.With("repository", t.Repository.FullName, "event", t.Event, "sha", t.Revision)
	if t.DeliveryID != "" {
		l = l.With("delivery", t.DeliveryID)
	}
	if t.Pipeline != "" {
		l = l.With("pipeline", t.Pipeline)
	}
	return l
}

// Handle does what an event asks for. It runs on a webhook worker. It returns an error when a call to
// the code host or the runner failed even after its retries and left part of the event undone, so
// that a redelivery tries again (every step finds what an earlier delivery did). Refused runs,
// invalid configurations and ignored events are done.
func (s *Service) Handle(ctx context.Context, ev ci.Event) error {
	switch e := ev.(type) {
	case *ci.TriggerEvent:
		t := e.Trigger
		if s.Schedules != nil && t.Event == ci.EventPush && t.Branch != "" && t.Branch == t.Repository.DefaultBranch {
			s.Schedules.Notify(t.InstallationID, t.Repository)
		}
		_, err := s.Evaluate(ctx, t, EvalOptions{Draft: e.Draft, ReportConfigErrors: reportsConfigErrors(t)})
		errs := []error{err}
		for _, reviewer := range e.PendingReviewers {
			if rt, ok := pendingReview(t, reviewer); ok {
				_, err := s.Evaluate(ctx, rt, EvalOptions{Draft: e.Draft, ReportConfigErrors: reportsConfigErrors(rt)})
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	case *ci.MergeGroupDestroyed:
		return s.CancelMergeGroup(ctx, e.Trigger, e.Reason)
	case *ci.CommandEvent:
		return s.HandleComment(ctx, e)
	case *ci.RerunEvent:
		return s.Rerun(ctx, e)
	}
	return nil
}

// undone is what err left undone that a retry may get past: a refused run is done, its report says
// why, unless a failed call refused it or kept that report from opening.
func undone(err error) error {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Cause
	}
	return err
}

// pendingReview is the review request still pending from reviewer when new commits arrive on a pull
// request (t): it runs again at the new head, read at the default branch like the request itself, and
// supersedes the run of the older commit.
func pendingReview(t ci.Trigger, reviewer string) (ci.Trigger, bool) {
	if t.Event != ci.EventPullRequest || t.Action != "synchronize" || t.PullRequest == nil || t.Repository.DefaultBranch == "" {
		return ci.Trigger{}, false
	}
	t.Event, t.Action, t.ConfigRef = ci.EventReviewRequest, "review_requested", t.Repository.DefaultBranch
	t.ReviewRequest = &ci.ReviewRequest{Reviewer: reviewer, Pending: true}
	return t, true
}

// reportsConfigErrors reports whether a trigger's configuration problems are reported. For pull
// requests, only the actions that normally run pipelines report them, so that labeling or editing a
// pull request does not add failures. Review requests never do: most are for people, not pipelines.
func reportsConfigErrors(t ci.Trigger) bool {
	switch t.Event {
	case ci.EventPullRequest:
		return slices.Contains(pipelines.DefaultPullRequestTypes, t.Action)
	case ci.EventReviewRequest:
		return false
	}
	return true
}

// EvalOptions tunes Evaluate.
type EvalOptions struct {
	// ReportConfigErrors reports an unreadable or invalid .octomaton.yaml on a failed
	// "octomaton" report.
	ReportConfigErrors bool
	// Draft is set for events of draft pull requests.
	Draft bool
	// RerunBy, when set, is the user who asked to evaluate the trigger again.
	RerunBy string
}

// Evaluate reads .octomaton.yaml where the trigger says (the commit under test, or the default
// branch for review requests) and starts every pipeline the trigger matches, and reports whether it
// read a usable configuration. A repository without the file is ignored, and so is a pull request
// from a fork: it gets no report and no run. A configuration the code host would not serve is always
// reported, on the re-runnable configuration report: no event is lost to an outage without a trace.
// The error is what failed calls left undone (see Handle).
func (s *Service) Evaluate(ctx context.Context, t ci.Trigger, opts EvalOptions) (bool, error) {
	log := s.logFor(t)
	if t.FromFork() {
		log.InfoContext(ctx, "Ignoring a pull request from a fork", "headRepository", t.PullRequest.HeadRepo)
		return false, nil
	}
	gh := s.Host.Installation(t.InstallationID)
	cfg, err := s.loadConfig(ctx, gh, t, reporting{invalid: opts.ReportConfigErrors, unreadable: s.unreadableOn(ctx, gh, t, ci.ConfigReportName)})
	if cfg == nil {
		return false, err
	}
	ev := matchEvent(t, opts.Draft)
	var (
		files *ci.ChangedFiles
		errs  []error
	)
	matched := 0
	for i := range cfg.Pipelines {
		p := &cfg.Pipelines[i]
		filter, ok := p.Match(ev)
		if !ok {
			continue
		}
		matched++
		pt := t
		pt.Pipeline, pt.DisplayName, pt.RerunBy = p.Name, p.DisplayName, opts.RerunBy
		if filter.Active() {
			if files == nil {
				f := s.changedFiles(ctx, gh, t)
				files = &f
			}
			if files.Complete && !filter.Matches(files.Files) {
				errs = append(errs, s.reportSkipped(ctx, gh, pt, filter, len(files.Files)))
				continue
			}
		}
		if _, err := s.start(ctx, gh, pt, p, false); err != nil {
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				log.ErrorContext(ctx, "Could not start the pipeline", "pipeline", p.Name, "error", err)
			}
			errs = append(errs, undone(err))
		}
	}
	log.InfoContext(ctx, "Evaluated event", "action", t.Action, "pipelines", len(cfg.Pipelines), "matched", matched)
	return true, errors.Join(errs...)
}

// LoadConfig reads the pipelines t's repository runs, like Evaluate; ok is false when it has none or
// a configuration is unusable. Problems are logged, not reported.
func (s *Service) LoadConfig(ctx context.Context, t ci.Trigger) (*pipelines.Config, bool) {
	cfg, _ := s.loadConfig(ctx, s.Host.Installation(t.InstallationID), t, reporting{})
	return cfg, cfg != nil
}

// LoadConfigToStart reads the pipelines t's repository runs, like LoadConfig, to start t's pipeline (a
// schedule's): a configuration the code host would not serve fails that pipeline's report, whose re-run
// starts it again.
func (s *Service) LoadConfigToStart(ctx context.Context, t ci.Trigger) (*pipelines.Config, bool) {
	gh := s.Host.Installation(t.InstallationID)
	cfg, _ := s.loadConfig(ctx, gh, t, reporting{unreadable: s.unreadableOn(ctx, gh, t, t.ReportName())})
	return cfg, cfg != nil
}

// reporting says which configuration problems loading a trigger's configuration reports.
type reporting struct {
	// invalid reports an invalid configuration: the repository's problem.
	invalid bool
	// unreadable reports a configuration the code host would not serve even after its retries:
	// Octomaton's. Nil only logs it.
	unreadable func(what, where string, err error)
}

// unreadableOn reports an unreadable configuration on t's report called name: the configuration's,
// or a pipeline's. It stores t, so re-running the report tries again.
func (s *Service) unreadableOn(ctx context.Context, gh ci.Installation, t ci.Trigger, name string) func(what, where string, err error) {
	return func(what, where string, err error) {
		if name == ci.ConfigReportName {
			t.Pipeline, t.DisplayName = "", ""
		}
		s.Metrics.RunCreated(ctx, metrics.RunFailed)
		s.openCompleted(ctx, gh, t, name, ci.Failure, "Could not read "+what,
			fmt.Sprintf("Octomaton could not read %s:\n\n```\n%v\n```\n\nRe-run this check to try again.", where, err))
	}
}

// loadConfig reads the pipelines t's repository runs: those of its .octomaton.yaml at t.ConfigAt(),
// and, when an OrganizationRepository is set, the organization pipelines its owner declares in that
// repository's .octomaton.yaml at its default branch. It returns no configuration when there is
// nothing to do (neither declares any) or a configuration is unusable, in which case the problem is
// reported as report says. The error is a configuration the code host would not serve, or a report
// of a problem it would not open.
func (s *Service) loadConfig(ctx context.Context, gh ci.Installation, t ci.Trigger, report reporting) (*pipelines.Config, error) {
	own := ownConfig(t)
	ownCfg, ok, err := s.readConfig(ctx, gh, t, own, report)
	if !ok {
		return nil, err
	}
	var orgCfg *pipelines.Config
	if s.OrganizationRepository != "" {
		if orgCfg, ok, err = s.readConfig(ctx, gh, t, organizationConfig(t, s.OrganizationRepository), report); !ok {
			return nil, err
		}
	}
	cfg, err := pipelines.ForRepository(t.Repository.Name, s.OrganizationRepository, ownCfg, orgCfg)
	if err != nil {
		return nil, s.configInvalid(ctx, gh, t, own, err, report)
	}
	if cfg == nil {
		s.logFor(t).DebugContext(ctx, "Repository has no "+pipelines.FileName+" and its owner no organization pipelines")
	}
	return cfg, nil
}

// configFile is a configuration read for a trigger: its repository's own, or its owner's
// organization pipelines.
type configFile struct {
	repo ci.Repository
	// ref is where the file is read; empty is the default branch.
	ref string
	// what names the configuration in titles and logs, and where locates it in summaries.
	what, where string
}

func ownConfig(t ci.Trigger) configFile {
	return configFile{
		repo: t.Repository, ref: t.ConfigAt(), what: pipelines.FileName,
		where: fmt.Sprintf("`%s` at `%s`", pipelines.FileName, ci.ShortSHA(t.ConfigAt())),
	}
}

func organizationConfig(t ci.Trigger, name string) configFile {
	repo := sibling(t.Repository, name)
	return configFile{
		repo: repo, what: "organization pipelines",
		where: fmt.Sprintf("`%s` at the default branch of `%s` (which declares the organization pipelines)", pipelines.FileName, repo.FullName),
	}
}

// sibling is the repository called name of repo's owner.
func sibling(repo ci.Repository, name string) ci.Repository {
	return ci.Repository{Owner: repo.Owner, Name: name, FullName: repo.Owner + "/" + name}
}

// readConfig reads and parses f; a missing file is a nil configuration. ok is false when f is
// unusable, and the error is as loadConfig's.
func (s *Service) readConfig(ctx context.Context, gh ci.Installation, t ci.Trigger, f configFile, report reporting) (cfg *pipelines.Config, ok bool, err error) {
	data, err := gh.ReadFile(ctx, f.repo, pipelines.FileName, f.ref)
	if errors.Is(err, ci.ErrNotFound) {
		return nil, true, nil
	}
	if err != nil {
		s.logFor(t).ErrorContext(ctx, "Could not read "+f.what, "configRepository", f.repo.FullName, "error", err)
		if report.unreadable != nil {
			report.unreadable(f.what, f.where, err)
		}
		return nil, false, err
	}
	cfg, err = pipelines.Parse(data, s.Host.CheckPermissions)
	if err != nil {
		return nil, false, s.configInvalid(ctx, gh, t, f, err, report)
	}
	return cfg, true, nil
}

// configInvalid logs an invalid configuration and reports it as report says; the error is a report
// the code host would not open.
func (s *Service) configInvalid(ctx context.Context, gh ci.Installation, t ci.Trigger, f configFile, err error, report reporting) error {
	s.logFor(t).WarnContext(ctx, "Invalid "+f.what, "configRepository", f.repo.FullName, "error", err)
	if !report.invalid {
		return nil
	}
	return s.reportConfigProblem(ctx, gh, t, "Invalid "+f.what, describeConfigError(f.where, err))
}

func describeConfigError(where string, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is invalid, so no pipeline was started:\n\n", where)
	var ce *pipelines.Error
	if errors.As(err, &ce) {
		for _, p := range ce.Problems {
			fmt.Fprintf(&b, "- %s\n", markdownLine(p))
		}
	} else {
		fmt.Fprintf(&b, "- %s\n", markdownLine(err.Error()))
	}
	return b.String()
}

// markdownLine keeps a message on one Markdown list line and renders it literally.
func markdownLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

func (s *Service) reportConfigProblem(ctx context.Context, gh ci.Installation, t ci.Trigger, title, summary string) error {
	t.Pipeline, t.DisplayName = "", ""
	s.Metrics.RunCreated(ctx, metrics.RunFailed)
	return s.openCompleted(ctx, gh, t, ci.ConfigReportName, ci.Failure, title, summary)
}

func (s *Service) reportSkipped(ctx context.Context, gh ci.Installation, t ci.Trigger, f pipelines.PathFilter, changed int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "None of the %d file(s) changed by this %s are relevant to pipeline `%s`, so it did not run.\n\n", changed, eventNoun(t), t.Pipeline)
	if len(f.Paths) > 0 {
		fmt.Fprintf(&b, "- `paths`: %s\n", codeList(f.Paths))
	}
	if len(f.PathsIgnore) > 0 {
		fmt.Fprintf(&b, "- `pathsIgnore`: %s\n", codeList(f.PathsIgnore))
	}
	b.WriteString("\nRe-run this check to run the pipeline anyway.")
	s.logFor(t).InfoContext(ctx, "Pipeline skipped: no relevant changes")
	s.Metrics.RunCreated(ctx, metrics.RunSkipped)
	return s.openCompleted(ctx, gh, t, t.ReportName(), ci.Skipped, "Skipped: no relevant changes", b.String())
}

// reportTimeout bounds a completed report. It runs on a context of its own: the failure it reports
// may be the job's deadline itself, and the code host retries the call for up to a few minutes.
const reportTimeout = 5 * time.Minute

// detached is ctx's values without its deadline or cancellation, bounded by reportTimeout.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
}

// openCompleted opens a completed report that stores the trigger, so it can be re-run. A report the
// code host refuses even after its retries is logged, the one trace left, and returned.
func (s *Service) openCompleted(ctx context.Context, gh ci.Installation, t ci.Trigger, name string, conclusion ci.Conclusion, title, summary string) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	now := s.now()
	r := ci.Report{
		Name: name, Revision: t.Revision, Status: ci.StatusCompleted, Conclusion: conclusion, Started: now, Completed: now,
		Title: title, Summary: summary, Trigger: &t,
	}
	_, err := gh.OpenReport(ctx, t.Repository, r)
	if err != nil {
		s.logFor(t).ErrorContext(ctx, "Could not open the report", "report", name, "conclusion", conclusion, "error", err)
	}
	return err
}

// failReport completes an open report with a failure, like openCompleted.
func (s *Service) failReport(ctx context.Context, gh ci.Installation, t ci.Trigger, id ci.ReportID, title, summary string) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	r := ci.Report{Status: ci.StatusCompleted, Conclusion: ci.Failure, Completed: s.now(), Title: title, Summary: summary, Trigger: &t}
	err := gh.UpdateReport(ctx, t.Repository, id, r)
	if err != nil {
		s.logFor(t).ErrorContext(ctx, "Could not update the report", "reportID", id, "error", err)
	}
	return err
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func eventNoun(t ci.Trigger) string {
	switch t.Event {
	case ci.EventPullRequest:
		return "pull request"
	case ci.EventMergeGroup:
		return "merge group"
	case ci.EventReviewRequest:
		return "review request"
	default:
		return "push"
	}
}

func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "`" + item + "`"
	}
	return strings.Join(quoted, ", ")
}

// matchEvent extracts what pipeline matching looks at.
func matchEvent(t ci.Trigger, draft bool) pipelines.Event {
	ev := pipelines.Event{Name: t.Event, Action: t.Action, Draft: draft}
	if t.ReviewRequest != nil {
		ev.Reviewer = t.ReviewRequest.Reviewer
	}
	switch {
	case t.PullRequest != nil:
		ev.Branch = t.PullRequest.BaseRef
	case t.MergeGroup != nil:
		ev.Branch = strings.TrimPrefix(t.MergeGroup.BaseRef, "refs/heads/")
	default:
		ev.Branch, ev.Tag = t.Branch, t.Tag
	}
	return ev
}

func isZeroSHA(sha string) bool {
	return sha == "" || strings.Trim(sha, "0") == ""
}

// changedFiles lists the files a trigger changes. On errors, and for new refs (where every path
// counts as changed), the list is incomplete, so path filters fail open.
func (s *Service) changedFiles(ctx context.Context, gh ci.Installation, t ci.Trigger) ci.ChangedFiles {
	var (
		files ci.ChangedFiles
		err   error
	)
	switch {
	case t.PullRequest != nil:
		files, err = gh.PullRequestFiles(ctx, t.Repository, t.PullRequest.Number)
	case t.MergeGroup != nil:
		files, err = gh.CompareFiles(ctx, t.Repository, t.MergeGroup.BaseSHA, t.MergeGroup.HeadSHA)
	case t.Push != nil:
		if t.Push.Created || isZeroSHA(t.Push.Before) {
			return ci.ChangedFiles{}
		}
		files, err = gh.CompareFiles(ctx, t.Repository, t.Push.Before, t.Push.After)
	default:
		return ci.ChangedFiles{}
	}
	if err != nil {
		s.logFor(t).WarnContext(ctx, "Could not determine the changed files; path filters are ignored", "error", err)
		return ci.ChangedFiles{}
	}
	return files
}

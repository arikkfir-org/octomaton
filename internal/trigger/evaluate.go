package trigger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/services/pipelines"
	"octomaton.dev/internal/system/metrics"
)

// EvalOptions tunes Evaluate.
type EvalOptions struct {
	// ReportConfigErrors creates a failed "octomaton" check run when
	// .octomaton.yaml cannot be read or is invalid.
	ReportConfigErrors bool
	// Draft is set for events of draft pull requests.
	Draft bool
	// ApprovedBy, when set, is a user with write access who approved running
	// pipelines for an untrusted pull request.
	ApprovedBy string
	// RerunBy, when set, is the user who asked to re-run the evaluation.
	RerunBy string
}

// Evaluate reads .octomaton.yaml at the commit under test and starts every
// pipeline the event matches. A repository without the file is ignored.
func (s *Service) Evaluate(ctx context.Context, c checkrun.Context, opts EvalOptions) {
	log := s.logFor(c)
	gh := s.GitHub.Installation(c.InstallationID)
	cfg, ok := s.loadConfig(ctx, gh, c, opts.ReportConfigErrors)
	if !ok {
		return
	}
	ev := matchEvent(c, opts.Draft)
	if pr := c.PullRequest; pr != nil && pr.AuthorAssociation == "" && !Trusted(pr, c.Repository.FullName) {
		// Webhook payloads carry the author's association; should one lack it,
		// read it from the pull request rather than treat a member as a stranger.
		if current, err := gh.PullRequest(ctx, c.Repository.Owner, c.Repository.Name, pr.Number); err == nil {
			withAssociation := *pr
			withAssociation.AuthorAssociation = current.AuthorAssociation
			c.PullRequest = &withAssociation
		} else {
			log.Warn("Could not read the pull request's author association", "error", err)
		}
	}
	var files *githubapp.ChangedFiles
	matched := 0
	for i := range cfg.Pipelines {
		p := &cfg.Pipelines[i]
		filter, ok := p.Match(ev)
		if !ok {
			continue
		}
		matched++
		pc := c
		pc.Pipeline = p.Name
		pc.ApprovedBy = opts.ApprovedBy
		pc.RerunBy = opts.RerunBy

		if filter.Active() {
			if files == nil {
				f := s.changedFiles(ctx, gh, c)
				files = &f
			}
			if files.Complete && !filter.Matches(files.Files) {
				s.reportSkipped(ctx, gh, pc, filter, len(files.Files))
				continue
			}
		}
		if pc.ApprovedBy == "" && !Trusted(c.PullRequest, c.Repository.FullName) {
			s.reportApprovalRequired(ctx, gh, pc)
			continue
		}
		if _, _, err := s.start(ctx, gh, pc, p, startOptions{}); err != nil {
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				log.Error("Could not start pipeline", "pipeline", p.Name, "error", err)
			}
		}
	}
	log.Info("Evaluated event", "action", c.Action, "pipelines", len(cfg.Pipelines), "matched", matched)
}

// loadConfig fetches and parses .octomaton.yaml at c.ConfigAt(). It returns
// false when there is nothing to do (no file) or the file is unusable, in which
// case the problem is reported on a "octomaton" check run when report is set.
func (s *Service) loadConfig(ctx context.Context, gh githubapp.Client, c checkrun.Context, report bool) (*pipelines.Config, bool) {
	log := s.logFor(c)
	data, err := gh.GetFile(ctx, c.Repository.Owner, c.Repository.Name, pipelines.FileName, c.ConfigAt())
	if errors.Is(err, githubapp.ErrNotFound) {
		log.Debug("Repository has no " + pipelines.FileName)
		return nil, false
	}
	if err != nil {
		log.Error("Could not read "+pipelines.FileName, "error", err)
		if report {
			s.reportConfigProblem(ctx, gh, c, "Could not read "+pipelines.FileName,
				fmt.Sprintf("Octomaton could not read `%s` at `%s`:\n\n```\n%v\n```\n\nRe-run this check to try again.",
					pipelines.FileName, checkrun.ShortSHA(c.ConfigAt()), err))
		}
		return nil, false
	}
	cfg, err := pipelines.Parse(data, githubapp.CheckPermissions)
	if err != nil {
		log.Warn("Invalid "+pipelines.FileName, "error", err)
		if report {
			s.reportConfigProblem(ctx, gh, c, "Invalid "+pipelines.FileName, describeConfigError(c, err))
		}
		return nil, false
	}
	return cfg, true
}

func describeConfigError(c checkrun.Context, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` at `%s` is invalid, so no pipeline was started:\n\n", pipelines.FileName, checkrun.ShortSHA(c.ConfigAt()))
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

func (s *Service) reportConfigProblem(ctx context.Context, gh githubapp.Client, c checkrun.Context, title, summary string) {
	c.Pipeline = ""
	s.Metrics.RunCreated(ctx, metrics.RunFailed)
	s.createCompleted(ctx, gh, c, checkrun.ConfigCheckName, "failure", title, summary, nil, "")
}

func (s *Service) reportSkipped(ctx context.Context, gh githubapp.Client, c checkrun.Context, f pipelines.PathFilter, changed int) {
	var b strings.Builder
	fmt.Fprintf(&b, "None of the %d file(s) changed by this %s are relevant to pipeline `%s`, so it did not run.\n\n", changed, eventNoun(c), c.Pipeline)
	if len(f.Paths) > 0 {
		fmt.Fprintf(&b, "- `paths`: %s\n", codeList(f.Paths))
	}
	if len(f.PathsIgnore) > 0 {
		fmt.Fprintf(&b, "- `pathsIgnore`: %s\n", codeList(f.PathsIgnore))
	}
	b.WriteString("\nRe-run this check to run the pipeline anyway.")
	s.logFor(c).Info("Pipeline skipped: no relevant changes")
	s.Metrics.RunCreated(ctx, metrics.RunSkipped)
	s.createCompleted(ctx, gh, c, c.Pipeline, "skipped", "Skipped: no relevant changes", b.String(), nil, "")
}

func (s *Service) reportApprovalRequired(ctx context.Context, gh githubapp.Client, c checkrun.Context) {
	pr := c.PullRequest
	summary := fmt.Sprintf("Pull request #%d was opened by @%s (association: `%s`) from `%s`, so pipeline `%s` does not run automatically.\n\n"+
		"A user with write access to %s should review the changes and then click **Approve and run** to run the pipeline for commit `%s`. "+
		"Every new commit requires a new approval.",
		pr.Number, pr.Author, strings.ToLower(orNone(pr.AuthorAssociation)), orNone(pr.HeadRepo), c.Pipeline,
		c.Repository.FullName, checkrun.ShortSHA(c.Revision))
	actions := []*github.CheckRunAction{{
		Label:       "Approve and run",
		Description: "Run the pipeline for this commit",
		Identifier:  ApproveAction,
	}}
	s.logFor(c).Info("Pipeline requires approval", "author", pr.Author, "association", pr.AuthorAssociation)
	s.Metrics.RunCreated(ctx, metrics.RunActionRequired)
	s.createCompleted(ctx, gh, c, c.Pipeline, "action_required", "Approval required", summary, actions, pr.HTMLURL)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func eventNoun(c checkrun.Context) string {
	switch c.Event {
	case checkrun.EventPullRequest:
		return "pull request"
	case checkrun.EventMergeGroup:
		return "merge group"
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

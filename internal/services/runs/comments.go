package runs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/pipelines"
)

// HandleComment runs the pipelines whose comment trigger matches a pull request comment's first
// line, or the built-in /rerun command. The configuration and pipeline definitions are read from the
// default branch (so a pull request cannot change what its own commands run), at the pull request's
// head commit. The commenter must be in the command's allow-list, and the pull request must be open
// (and, for a pipeline's command, not a draft). Started commands get an "eyes" reaction; declined
// ones a thumbs-down and a reply saying why. Commands on pull requests from forks are ignored without
// a reaction or a reply. The error is as Handle's.
func (s *Service) HandleComment(ctx context.Context, e *ci.CommandEvent) error {
	log := s.Logger.With("repository", e.Repository.FullName, "pullRequest", e.Number, "comment", e.CommentID, "author", e.Author, "delivery", e.DeliveryID)
	gh := s.Host.Installation(e.InstallationID)
	if e.Repository.DefaultBranch == "" {
		log.WarnContext(ctx, "The repository has no default branch in the payload; ignoring the comment")
		return nil
	}
	// A decline is the command's failure report: like a check's, it outlives the job.
	decline := func(reason string) error {
		ctx, cancel := detached(ctx)
		defer cancel()
		log.InfoContext(ctx, "Comment command declined", "command", e.Line, "reason", reason)
		reacted := gh.React(ctx, e.Repository, e.CommentID, "-1")
		if reacted != nil {
			log.WarnContext(ctx, "Could not react to the comment", "error", reacted)
		}
		body := fmt.Sprintf("@%s `%s` was not run: %s.", e.Author, strings.ReplaceAll(e.Line, "`", "'"), strings.TrimSuffix(reason, "."))
		replied := gh.Comment(ctx, e.Repository, e.Number, body)
		if replied != nil {
			log.WarnContext(ctx, "Could not reply to the comment", "error", replied)
		}
		return errors.Join(reacted, replied)
	}

	// A fork's commands get no answer, not even a failure's, so the pull request is read first.
	pr, err := gh.PullRequest(ctx, e.Repository, e.Number)
	switch {
	case err != nil:
		log.ErrorContext(ctx, "Could not read the pull request", "error", err)
		return errors.Join(err, decline("Octomaton could not read the pull request; see its logs"))
	case pr.FromFork(e.Repository.FullName):
		log.InfoContext(ctx, "Ignoring a comment command on a pull request from a fork", "headRepository", pr.HeadRepo)
		return nil
	}

	base := ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventComment, Action: "created", DeliveryID: e.DeliveryID,
		InstallationID: e.InstallationID, Repository: e.Repository, Sender: e.Author, ConfigRef: e.Repository.DefaultBranch,
	}
	// A command has no check: a configuration the code host would not serve is said in a reply.
	cfg, err := s.loadConfig(ctx, gh, base, reporting{unreadable: func(what, _ string, _ error) {
		_ = decline("Octomaton could not read " + what + "; comment again to try again")
	}})
	if cfg == nil {
		return err
	}
	command, arguments, _ := strings.Cut(e.Line, " ")
	if command == pipelines.RerunCommand {
		return s.rerunCommand(ctx, gh, e, pr, cfg, strings.TrimSpace(arguments), decline)
	}
	var asked []*pipelines.Pipeline
	for i := range cfg.Pipelines {
		if cfg.Pipelines[i].IsCommand(e.Line) {
			asked = append(asked, &cfg.Pipelines[i])
		}
	}
	if len(asked) == 0 {
		log.DebugContext(ctx, "The comment matches no command")
		return nil
	}

	switch {
	case pr.State != "open":
		return decline("the pull request is closed")
	case pr.Draft:
		return decline("the pull request is a draft")
	}

	t := base
	t.Revision = pr.HeadSHA
	t.Ref = fmt.Sprintf("refs/pull/%d/head", pr.Number)
	t.Branch = pr.HeadRef
	t.PullRequest = &pr.PullRequest
	t.Comment = &ci.Comment{ID: e.CommentID, Author: e.Author, Command: command, Arguments: strings.TrimSpace(arguments)}

	started := false
	var errs []error
	for _, p := range asked {
		if _, declined := p.MatchComment(e.Line, pr.BaseRef); declined != "" {
			errs = append(errs, decline(declined))
			continue
		}
		if ok, err := s.allowed(ctx, gh, e, p.On.Comment.Allow); !ok {
			errs = append(errs, err, decline(notAllowed(e.Author, command, err)))
			continue
		}
		pt := t
		pt.Pipeline = p.Name
		_, err := s.start(ctx, gh, pt, p, false)
		var refusal *Refusal
		switch {
		case errors.As(err, &refusal):
			errs = append(errs, refusal.Cause, decline(refusal.Reason))
		case err != nil:
			errs = append(errs, err, decline("Octomaton could not start it; see its logs"))
		default:
			started = true
		}
	}
	if started {
		if err := gh.React(ctx, e.Repository, e.CommentID, "eyes"); err != nil {
			log.WarnContext(ctx, "Could not react to the comment", "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// allowed reports whether the commenter is in a command's allow-list: named as a user, or an active
// member of a named team of the repository's owner. The error is the code host's when a membership
// could not be read, and the commenter is then not allowed.
func (s *Service) allowed(ctx context.Context, gh ci.Installation, e *ci.CommandEvent, allow *pipelines.Allow) (bool, error) {
	if allow.AllowsUser(e.Author) {
		return true, nil
	}
	if allow == nil {
		return false, nil
	}
	for _, team := range allow.Teams {
		member, err := gh.TeamMember(ctx, e.Repository.Owner, team, e.Author)
		if err != nil {
			s.Logger.ErrorContext(ctx, "Could not verify the commenter's team membership", "repository", e.Repository.FullName, "author", e.Author, "team", team, "error", err)
			return false, err
		}
		if member {
			return true, nil
		}
	}
	return false, nil
}

// notAllowed is why a commenter outside a command's allow-list is declined.
func notAllowed(author, command string, err error) string {
	if err != nil {
		return "Octomaton could not verify your team membership; see its logs"
	}
	return fmt.Sprintf("%s is not in the allow-list of `%s`", author, strings.ReplaceAll(command, "`", "'"))
}

// rerunCommand handles "/rerun [check...]": the failed reports at the pull request's head, or the
// named ones (a check's name, or a pipeline's name, case-insensitive, separated by spaces), run
// again as the code host's re-run would. The command is enabled by cfg's commands.rerun.allow.
func (s *Service) rerunCommand(ctx context.Context, gh ci.Installation, e *ci.CommandEvent, pr ci.PullRequestState, cfg *pipelines.Config, names string, decline func(string) error) error {
	allow := cfg.RerunAllow()
	if allow == nil {
		return decline("`/rerun` is not enabled: `commands.rerun.allow` in `" + pipelines.FileName + "` names nobody")
	}
	if ok, err := s.allowed(ctx, gh, e, allow); !ok {
		return errors.Join(err, decline(notAllowed(e.Author, pipelines.RerunCommand, err)))
	}
	if pr.State != "open" {
		return decline("the pull request is closed")
	}
	refs, err := gh.RevisionReports(ctx, e.Repository, pr.HeadSHA)
	if err != nil {
		s.Logger.ErrorContext(ctx, "Could not list the reports at the pull request's head", "repository", e.Repository.FullName, "sha", pr.HeadSHA, "error", err)
		return errors.Join(err, decline("Octomaton could not list the checks; see its logs"))
	}
	var selected []ci.ReportRef
	if names == "" {
		for _, ref := range refs {
			if ref.Conclusion == ci.Failure || ref.Conclusion == ci.TimedOut || ref.Conclusion == ci.Cancelled {
				selected = append(selected, ref)
			}
		}
		if len(selected) == 0 {
			return decline(fmt.Sprintf("no check failed at `%s`", ci.ShortSHA(pr.HeadSHA)))
		}
	} else {
		var unknown string
		if selected, unknown = namedReports(refs, cfg, names); unknown != "" {
			return decline(fmt.Sprintf("there is no check named `%s` at `%s`", strings.ReplaceAll(unknown, "`", "'"), ci.ShortSHA(pr.HeadSHA)))
		}
	}
	re := &ci.RerunEvent{InstallationID: e.InstallationID, Repository: e.Repository, Reports: selected, Requester: e.Author, DeliveryID: e.DeliveryID}
	started, err := s.rerunReports(ctx, gh, re)
	if started == 0 {
		return errors.Join(err, decline("Octomaton could not re-run any of them; see its logs"))
	}
	if reacted := gh.React(ctx, e.Repository, e.CommentID, "eyes"); reacted != nil {
		s.Logger.WarnContext(ctx, "Could not react to the comment", "comment", e.CommentID, "error", reacted)
		err = errors.Join(err, reacted)
	}
	return err
}

// namedReports selects the reports the words of names ask for: at each word, the longest run of
// words that equals a report's name, its pipeline's check name or its pipeline's name
// (case-insensitive), so that "Continuous Integration docs" names a check with a space and a
// pipeline. unknown is the first word that starts no name, or "" when every word was used.
func namedReports(refs []ci.ReportRef, cfg *pipelines.Config, names string) (selected []ci.ReportRef, unknown string) {
	pipelineOfCheck := map[string]string{}
	for i := range cfg.Pipelines {
		pipelineOfCheck[cfg.Pipelines[i].CheckName()] = cfg.Pipelines[i].Name
	}
	matches := func(ref ci.ReportRef, name string) bool {
		check := checkOf(ref.Name)
		return strings.EqualFold(ref.Name, name) || strings.EqualFold(check, name) || strings.EqualFold(pipelineOfCheck[check], name)
	}
	words := strings.Fields(names)
	for i := 0; i < len(words); {
		var found []ci.ReportRef
		used := 0
		for k := 1; i+k <= len(words); k++ {
			name := strings.Join(words[i:i+k], " ")
			var named []ci.ReportRef
			for _, ref := range refs {
				if matches(ref, name) {
					named = append(named, ref)
				}
			}
			if len(named) > 0 {
				found, used = named, k
			}
		}
		if used == 0 {
			return nil, words[i]
		}
		selected = append(selected, found...)
		i += used
	}
	return selected, ""
}

// checkOf is the pipeline's check name of a report's name: the part before " / " of a task
// report's, else the name itself.
func checkOf(report string) string {
	name, _, _ := strings.Cut(report, " / ")
	return name
}

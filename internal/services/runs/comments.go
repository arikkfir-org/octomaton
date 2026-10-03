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
// line. The configuration and pipeline definitions are read from the default branch (so a pull
// request cannot change what its own commands run), at the pull request's head commit. The commenter
// needs write access, and the pull request must be open and not a draft. Started commands get an
// "eyes" reaction; declined ones a thumbs-down and a reply saying why. Commands on pull requests from
// forks are ignored without a reaction or a reply. The error is as Handle's.
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
	level, err := gh.Permission(ctx, e.Repository, e.Author)
	if err != nil {
		log.ErrorContext(ctx, "Could not verify the commenter's permission", "error", err)
		return errors.Join(err, decline("Octomaton could not verify your permission; see its logs"))
	}
	if !level.CanWrite() {
		return decline(fmt.Sprintf("%s does not have write access to %s", e.Author, e.Repository.FullName))
	}

	command, arguments, _ := strings.Cut(e.Line, " ")
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

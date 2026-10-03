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
// forks are ignored without a reaction or a reply.
func (s *Service) HandleComment(ctx context.Context, e *ci.CommandEvent) {
	log := s.Logger.With("repository", e.Repository.FullName, "pullRequest", e.Number, "comment", e.CommentID, "author", e.Author, "delivery", e.DeliveryID)
	gh := s.Host.Installation(e.InstallationID)
	if e.Repository.DefaultBranch == "" {
		log.WarnContext(ctx, "The repository has no default branch in the payload; ignoring the comment")
		return
	}
	base := ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventComment, Action: "created", DeliveryID: e.DeliveryID,
		InstallationID: e.InstallationID, Repository: e.Repository, Sender: e.Author, ConfigRef: e.Repository.DefaultBranch,
	}
	cfg, ok := s.loadConfig(ctx, gh, base, reporting{})
	if !ok {
		return
	}
	var asked []*pipelines.Pipeline
	for i := range cfg.Pipelines {
		if cfg.Pipelines[i].IsCommand(e.Line) {
			asked = append(asked, &cfg.Pipelines[i])
		}
	}
	if len(asked) == 0 {
		log.DebugContext(ctx, "The comment matches no command")
		return
	}

	decline := func(reason string) {
		log.InfoContext(ctx, "Comment command declined", "command", e.Line, "reason", reason)
		if err := gh.React(ctx, e.Repository, e.CommentID, "-1"); err != nil {
			log.WarnContext(ctx, "Could not react to the comment", "error", err)
		}
		body := fmt.Sprintf("@%s `%s` was not run: %s.", e.Author, strings.ReplaceAll(e.Line, "`", "'"), strings.TrimSuffix(reason, "."))
		if err := gh.Comment(ctx, e.Repository, e.Number, body); err != nil {
			log.WarnContext(ctx, "Could not reply to the comment", "error", err)
		}
	}

	pr, err := gh.PullRequest(ctx, e.Repository, e.Number)
	switch {
	case err != nil:
		log.ErrorContext(ctx, "Could not read the pull request", "error", err)
		decline("Octomaton could not read the pull request; see its logs")
		return
	case pr.FromFork(e.Repository.FullName):
		log.InfoContext(ctx, "Ignoring a comment command on a pull request from a fork", "headRepository", pr.HeadRepo)
		return
	case pr.State != "open":
		decline("the pull request is closed")
		return
	case pr.Draft:
		decline("the pull request is a draft")
		return
	}
	level, err := gh.Permission(ctx, e.Repository, e.Author)
	if err != nil {
		log.ErrorContext(ctx, "Could not verify the commenter's permission", "error", err)
		decline("Octomaton could not verify your permission; see its logs")
		return
	}
	if !level.CanWrite() {
		decline(fmt.Sprintf("%s does not have write access to %s", e.Author, e.Repository.FullName))
		return
	}

	command, arguments, _ := strings.Cut(e.Line, " ")
	t := base
	t.Revision = pr.HeadSHA
	t.Ref = fmt.Sprintf("refs/pull/%d/head", pr.Number)
	t.Branch = pr.HeadRef
	t.PullRequest = &pr.PullRequest
	t.Comment = &ci.Comment{ID: e.CommentID, Author: e.Author, Command: command, Arguments: strings.TrimSpace(arguments)}

	started := false
	for _, p := range asked {
		if _, declined := p.MatchComment(e.Line, pr.BaseRef); declined != "" {
			decline(declined)
			continue
		}
		pt := t
		pt.Pipeline = p.Name
		_, err := s.start(ctx, gh, pt, p, false)
		var refusal *Refusal
		switch {
		case errors.As(err, &refusal):
			decline(refusal.Reason)
		case err != nil:
			decline("Octomaton could not start it; see its logs")
		default:
			started = true
		}
	}
	if started {
		if err := gh.React(ctx, e.Repository, e.CommentID, "eyes"); err != nil {
			log.WarnContext(ctx, "Could not react to the comment", "error", err)
		}
	}
}

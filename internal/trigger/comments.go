package trigger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/githubapp"
	"octomaton.dev/internal/services/pipelines"
	"octomaton.dev/internal/tekton"
)

func commentDedupe(id int64) map[string]string {
	return map[string]string{tekton.LabelComment: strconv.FormatInt(id, 10)}
}

// HandleComment runs the pipelines whose comment trigger matches a pull request
// comment's first line. The configuration and PipelineRun files are read from
// the default branch (so a pull request cannot change what its own commands
// run), at the pull request's head commit. The commenter needs write access and
// the pull request must be open and not a draft. Started commands get an "eyes"
// reaction; declined ones a thumbs-down and a reply saying why.
func (s *Service) HandleComment(ctx context.Context, req CommentRequest) {
	log := s.Logger.With("repository", req.Repository.FullName, "pullRequest", req.Number, "comment", req.CommentID, "author", req.Author, "delivery", req.DeliveryID)
	gh := s.GitHub.Installation(req.InstallationID)
	owner, repo := req.Repository.Owner, req.Repository.Name
	if req.Repository.DefaultBranch == "" {
		log.Warn("Repository has no default branch in the payload; ignoring comment")
		return
	}
	base := checkrun.Context{
		Version:        checkrun.ContextVersion,
		Event:          checkrun.EventComment,
		Action:         "created",
		DeliveryID:     req.DeliveryID,
		InstallationID: req.InstallationID,
		Repository:     req.Repository,
		Sender:         req.Author,
		ConfigRef:      req.Repository.DefaultBranch,
	}
	cfg, ok := s.loadConfig(ctx, gh, base, false)
	if !ok {
		return
	}
	var asked []*pipelines.Pipeline
	for i := range cfg.Pipelines {
		if cfg.Pipelines[i].IsCommand(req.Line) {
			asked = append(asked, &cfg.Pipelines[i])
		}
	}
	if len(asked) == 0 {
		log.Debug("Comment matches no command")
		return
	}

	decline := func(reason string) {
		log.Info("Comment command declined", "command", req.Line, "reason", reason)
		if err := gh.React(ctx, owner, repo, req.CommentID, "-1"); err != nil {
			log.Warn("Could not react to comment", "error", err)
		}
		body := fmt.Sprintf("@%s `%s` was not run: %s.", req.Author, strings.ReplaceAll(req.Line, "`", "'"), strings.TrimSuffix(reason, "."))
		if err := gh.Comment(ctx, owner, repo, req.Number, body); err != nil {
			log.Warn("Could not reply to comment", "error", err)
		}
	}

	pr, err := gh.PullRequest(ctx, owner, repo, req.Number)
	switch {
	case err != nil:
		log.Error("Could not read the pull request", "error", err)
		decline("Octomaton could not read the pull request; see its logs")
		return
	case pr.State != "open":
		decline("the pull request is closed")
		return
	case pr.Draft:
		decline("the pull request is a draft")
		return
	}
	level, err := gh.PermissionLevel(ctx, owner, repo, req.Author)
	if err != nil {
		log.Error("Could not verify the commenter's permission", "error", err)
		decline("Octomaton could not verify your permission; see its logs")
		return
	}
	if !githubapp.CanWrite(level) {
		decline(fmt.Sprintf("%s does not have write access to %s", req.Author, req.Repository.FullName))
		return
	}

	command, arguments, _ := strings.Cut(req.Line, " ")
	c := base
	c.Revision = pr.HeadSHA
	c.Ref = fmt.Sprintf("refs/pull/%d/head", pr.Number)
	c.Branch = pr.HeadRef
	c.PullRequest = &checkrun.PullRequest{
		Number: pr.Number, HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, BaseSHA: pr.BaseSHA,
		HeadRepo: pr.HeadRepo, Author: pr.Author, AuthorAssociation: pr.AuthorAssociation, HTMLURL: pr.HTMLURL,
	}
	c.Comment = &checkrun.Comment{ID: req.CommentID, Author: req.Author, Command: command, Arguments: strings.TrimSpace(arguments)}

	started := false
	for _, p := range asked {
		if _, declined := p.MatchComment(req.Line, pr.BaseRef); declined != "" {
			decline(declined)
			continue
		}
		pc := c
		pc.Pipeline = p.Name
		_, _, err := s.start(ctx, gh, pc, p, startOptions{Dedupe: commentDedupe(req.CommentID)})
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
		if err := gh.React(ctx, owner, repo, req.CommentID, "eyes"); err != nil {
			log.Warn("Could not react to comment", "error", err)
		}
	}
}

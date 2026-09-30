package pipelines

import "octomaton.dev/internal/services/ci"

// RepositoryOf returns a repository as templates see it.
func RepositoryOf(r ci.Repository) Repository {
	return Repository{
		Owner:         r.Owner,
		Name:          r.Name,
		FullName:      r.FullName,
		CloneURL:      r.CloneURL,
		HTMLURL:       r.HTMLURL,
		DefaultBranch: r.DefaultBranch,
		Private:       r.Private,
	}
}

// ContextOf returns what templates see of a trigger.
func ContextOf(t ci.Trigger) TemplateContext {
	c := TemplateContext{
		Event:      t.Event,
		Action:     t.Action,
		Repository: RepositoryOf(t.Repository),
		Revision:   t.Revision,
		Ref:        t.Ref,
		Branch:     t.Branch,
		Tag:        t.Tag,
		Sender:     t.Sender,
		Pipeline:   t.Pipeline,
	}
	if p := t.Push; p != nil {
		c.Push = &Push{Before: p.Before, After: p.After}
	}
	if p := t.PullRequest; p != nil {
		c.PullRequest = &PullRequest{Number: p.Number, HeadRef: p.HeadRef, HeadSHA: p.HeadSHA, BaseRef: p.BaseRef, BaseSHA: p.BaseSHA}
	}
	if m := t.MergeGroup; m != nil {
		c.MergeGroup = &MergeGroup{HeadRef: m.HeadRef, HeadSHA: m.HeadSHA, BaseRef: m.BaseRef, BaseSHA: m.BaseSHA}
	}
	if m := t.Comment; m != nil {
		c.Comment = &Comment{ID: m.ID, Author: m.Author, Command: m.Command, Arguments: m.Arguments}
	}
	if r := t.ReviewRequest; r != nil {
		c.ReviewRequest = &ReviewRequest{Reviewer: r.Reviewer}
	}
	if s := t.Schedule; s != nil {
		c.Schedule = &Schedule{Cron: s.Cron, Slot: s.Slot}
	}
	return c
}

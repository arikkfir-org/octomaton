package trigger

import (
	"testing"

	"github.com/google/go-github/v92/github"
)

func ghRepo(ownerLogin string) *github.Repository {
	return &github.Repository{
		ID: new(int64(repoID)), Name: new(repoName), FullName: new(ownerLogin + "/" + repoName),
		Owner: &github.User{Login: new(ownerLogin)}, DefaultBranch: new("main"),
		CloneURL: new("https://github.com/" + ownerLogin + "/" + repoName + ".git"),
	}
}

func pushEvent(ref, after string, deleted bool) *github.PushEvent {
	return &github.PushEvent{
		Ref: new(ref), Before: new(baseSHA), After: new(after), Deleted: new(deleted),
		Repo:         &github.PushEventRepository{ID: new(int64(repoID)), Name: new(repoName), FullName: new(fullName), Owner: &github.User{Login: new(owner)}, DefaultBranch: new("main")},
		Installation: &github.Installation{ID: new(int64(installationID))},
		Sender:       &github.User{Login: new("alice")},
	}
}

func TestRoute(t *testing.T) {
	h := newHarness(t)
	appCheck := &github.CheckRun{ID: new(int64(1)), App: &github.App{ID: new(int64(appID))}}
	otherCheck := &github.CheckRun{ID: new(int64(1)), App: &github.App{ID: new(int64(1))}}
	installation := &github.Installation{ID: new(int64(installationID))}
	sender := &github.User{Login: new("maintainer")}
	prPayload := func(ownerLogin string) *github.PullRequestEvent {
		return &github.PullRequestEvent{
			Action: new("opened"), Number: new(5), Repo: ghRepo(ownerLogin), Installation: installation, Sender: sender,
			PullRequest: &github.PullRequest{Head: &github.PullRequestBranch{SHA: new(sha1), Ref: new("feature")}, Base: &github.PullRequestBranch{Ref: new("main")}},
		}
	}
	comment := func(action, body string, onPR bool) *github.IssueCommentEvent {
		issue := &github.Issue{Number: new(5)}
		if onPR {
			issue.PullRequestLinks = &github.PullRequestLinks{URL: new("u")}
		}
		return &github.IssueCommentEvent{Action: new(action), Issue: issue, Repo: ghRepo(owner), Installation: installation, Sender: sender,
			Comment: &github.IssueComment{ID: new(int64(9)), Body: new(body), User: sender}}
	}
	tests := []struct {
		name    string
		event   string
		payload any
		reason  string // "" = a job
	}{
		{"push to a branch", "push", pushEvent("refs/heads/main", sha1, false), ""},
		{"push of a tag", "push", pushEvent("refs/tags/v1", sha1, false), ""},
		{"deleted branch", "push", pushEvent("refs/heads/main", "0000000000000000000000000000000000000000", true), "ref was deleted"},
		{"merge queue branch", "push", pushEvent("refs/heads/gh-readonly-queue/main/pr-1-abc", sha1, false), "push to a merge queue branch (merge_group events cover it)"},
		{"notes ref", "push", pushEvent("refs/notes/x", sha1, false), "ref is neither a branch nor a tag"},
		{"pull request", "pull_request", prPayload(owner), ""},
		{"owner not allowed", "pull_request", prPayload("stranger"), "repository owner is not allowed"},
		{"merge group checks requested", "merge_group", &github.MergeGroupEvent{Action: new("checks_requested"), MergeGroup: &github.MergeGroup{HeadSHA: new(sha1), HeadRef: new("refs/heads/gh-readonly-queue/main/x"), BaseRef: new("refs/heads/main")}, Repo: ghRepo(owner), Installation: installation}, ""},
		{"merge group destroyed", "merge_group", &github.MergeGroupEvent{Action: new("destroyed"), MergeGroup: &github.MergeGroup{HeadSHA: new(sha1)}, Repo: ghRepo(owner), Installation: installation}, ""},
		{"check run rerequested", "check_run", &github.CheckRunEvent{Action: new("rerequested"), CheckRun: appCheck, Repo: ghRepo(owner), Installation: installation, Sender: sender}, ""},
		{"check run of another app", "check_run", &github.CheckRunEvent{Action: new("rerequested"), CheckRun: otherCheck, Repo: ghRepo(owner), Installation: installation, Sender: sender}, "check run belongs to another app"},
		{"approve action", "check_run", &github.CheckRunEvent{Action: new("requested_action"), RequestedAction: &github.RequestedAction{Identifier: ApproveAction}, CheckRun: appCheck, Repo: ghRepo(owner), Installation: installation, Sender: sender}, ""},
		{"unknown action", "check_run", &github.CheckRunEvent{Action: new("requested_action"), RequestedAction: &github.RequestedAction{Identifier: "other"}, CheckRun: appCheck, Repo: ghRepo(owner), Installation: installation, Sender: sender}, "unknown requested action"},
		{"check run created", "check_run", &github.CheckRunEvent{Action: new("created"), CheckRun: appCheck}, "unhandled check_run action created"},
		{"check suite rerequested", "check_suite", &github.CheckSuiteEvent{Action: new("rerequested"), CheckSuite: &github.CheckSuite{ID: new(int64(3)), App: &github.App{ID: new(int64(appID))}}, Repo: ghRepo(owner), Installation: installation, Sender: sender}, ""},
		{"check suite requested", "check_suite", &github.CheckSuiteEvent{Action: new("requested")}, "unhandled check_suite action requested"},
		{"comment command", "issue_comment", comment("created", "/deploy now", true), ""},
		{"comment on an issue", "issue_comment", comment("created", "/deploy", false), "comment is not on a pull request"},
		{"plain comment", "issue_comment", comment("created", "looks good", true), "comment is not a command"},
		{"edited comment", "issue_comment", comment("edited", "/deploy", true), "unhandled issue_comment action edited"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, reason := h.svc.Route(tt.event, "d", tt.payload)
			if reason != tt.reason {
				t.Fatalf("reason = %q, want %q", reason, tt.reason)
			}
			if (job.Run != nil) != (tt.reason == "") {
				t.Fatalf("job = %+v, want a job: %v", job, tt.reason == "")
			}
		})
	}
}

func TestPushContextUsesTheTaggedCommit(t *testing.T) {
	ev := pushEvent("refs/tags/v1.0.0", "7777777777777777777777777777777777777777", false)
	ev.Created = new(true)
	ev.HeadCommit = &github.HeadCommit{ID: new(sha1)}
	c, reason := pushContext(ev, "d")
	if reason != "" || c.Revision != sha1 || c.Tag != "v1.0.0" || c.Branch != "" || !c.Push.Created || c.Push.After != "7777777777777777777777777777777777777777" {
		t.Fatalf("pushContext = %+v, %q", c, reason)
	}
}

func TestPullRequestContext(t *testing.T) {
	ev := &github.PullRequestEvent{
		Action: new("ready_for_review"), Number: new(12), Repo: ghRepo(owner),
		Installation: &github.Installation{ID: new(int64(installationID))}, Sender: &github.User{Login: new("bob")},
		PullRequest: &github.PullRequest{
			Draft: new(true),
			//lint:ignore SA1019 webhook payloads (unlike the Events API) carry author_association
			AuthorAssociation: new("CONTRIBUTOR"), User: &github.User{Login: new("carol")},
			Head: &github.PullRequestBranch{SHA: new(sha1), Ref: new("topic"), Repo: &github.Repository{FullName: new("carol/demo")}},
			Base: &github.PullRequestBranch{SHA: new(baseSHA), Ref: new("main")},
		},
	}
	c, draft, reason := pullRequestContext(ev, "d")
	if reason != "" || !draft || c.Revision != sha1 || c.Ref != "refs/pull/12/head" || c.Branch != "topic" ||
		c.PullRequest.HeadRepo != "carol/demo" || c.PullRequest.AuthorAssociation != "CONTRIBUTOR" || c.PullRequest.BaseRef != "main" {
		t.Fatalf("pullRequestContext = %+v, %v, %q", c, draft, reason)
	}
	if m := matchEvent(c, draft); m.Branch != "main" || !m.Draft || m.Action != "ready_for_review" {
		t.Fatalf("matchEvent = %+v", m)
	}
}

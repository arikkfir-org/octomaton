package github

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/services/ci"
)

const (
	repoID   = 1001
	owner    = "octo-org"
	repoName = "demo"
	sha1     = "1111111111111111111111111111111111111111"
	baseSHA  = "9999999999999999999999999999999999999999"
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
		Repo:         &github.PushEventRepository{ID: new(int64(repoID)), Name: new(repoName), FullName: new(owner + "/" + repoName), Owner: &github.User{Login: new(owner)}, DefaultBranch: new("main")},
		Installation: &github.Installation{ID: new(int64(installationID))},
		Sender:       &github.User{Login: new("alice")},
	}
}

func body(t *testing.T, payload any) []byte {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDecodeReasons(t *testing.T) {
	app, _ := newApp(t, WithOwners([]string{owner}))
	appCheck := &github.CheckRun{ID: new(int64(1)), App: &github.App{ID: new(int64(appID))}}
	otherCheck := &github.CheckRun{ID: new(int64(1)), App: &github.App{ID: new(int64(1))}}
	installation := &github.Installation{ID: new(int64(installationID))}
	sender := &github.User{Login: new("maintainer")}
	// prPayload is a pull request into ownerLogin's repository from headRepo (nil: a deleted one).
	prPayload := func(ownerLogin string, headRepo *github.Repository) *github.PullRequestEvent {
		return &github.PullRequestEvent{
			Action: new("opened"), Number: new(5), Repo: ghRepo(ownerLogin), Installation: installation, Sender: sender,
			PullRequest: &github.PullRequest{Head: &github.PullRequestBranch{SHA: new(sha1), Ref: new("feature"), Repo: headRepo}, Base: &github.PullRequestBranch{Ref: new("main")}},
		}
	}
	forkRepo := func() *github.Repository {
		r := ghRepo(owner)
		r.Fork = new(true)
		return r
	}
	forkPush := pushEvent("refs/heads/main", sha1, false)
	forkPush.Repo.Fork = new(true)
	comment := func(action, text string, onPR bool) *github.IssueCommentEvent {
		issue := &github.Issue{Number: new(5)}
		if onPR {
			issue.PullRequestLinks = &github.PullRequestLinks{URL: new("u")}
		}
		return &github.IssueCommentEvent{Action: new(action), Issue: issue, Repo: ghRepo(owner), Installation: installation, Sender: sender,
			Comment: &github.IssueComment{ID: new(int64(9)), Body: new(text), User: sender}}
	}
	forkComment := comment("created", "/deploy", true)
	forkComment.Repo = forkRepo()
	// reviewRequest asks for a review on open pull request #5 from a user or a team (or neither).
	reviewRequest := func(reviewer, team, state string) *github.PullRequestEvent {
		ev := prPayload(owner, ghRepo(owner))
		ev.Action = new("review_requested")
		ev.PullRequest.State = new(state)
		if reviewer != "" {
			ev.RequestedReviewer = &github.User{Login: new(reviewer)}
		}
		if team != "" {
			ev.RequestedTeam = &github.Team{Slug: new(team)}
		}
		return ev
	}
	forkReviewRequest := reviewRequest("octo-reviewer", "", "open")
	forkReviewRequest.PullRequest.Head.Repo = ghRepo("stranger")
	noDefaultBranch := reviewRequest("octo-reviewer", "", "open")
	noDefaultBranch.Repo.DefaultBranch = nil
	mergeGroup := func(action string) *github.MergeGroupEvent {
		return &github.MergeGroupEvent{Action: new(action), Reason: new("dequeued"), Repo: ghRepo(owner), Installation: installation,
			MergeGroup: &github.MergeGroup{HeadSHA: new(sha1), HeadRef: new("refs/heads/gh-readonly-queue/main/x"), BaseRef: new("refs/heads/main")}}
	}
	tests := []struct {
		name    string
		event   string
		payload any
		reason  string
		want    string // the event's String when it is decoded
	}{
		{"push to a branch", "push", pushEvent("refs/heads/main", sha1, false), "", "push octo-org/demo@1111111"},
		{"push of a tag", "push", pushEvent("refs/tags/v1", sha1, false), "", "push octo-org/demo@1111111"},
		{"deleted branch", "push", pushEvent("refs/heads/main", "0000000000000000000000000000000000000000", true), "ref was deleted", ""},
		{"merge queue branch", "push", pushEvent("refs/heads/gh-readonly-queue/main/pr-1-abc", sha1, false), "push to a merge queue branch (merge_group events cover it)", ""},
		{"notes ref", "push", pushEvent("refs/notes/x", sha1, false), "ref is neither a branch nor a tag", ""},
		{"pull request", "pull_request", prPayload(owner, ghRepo(owner)), "", "pull_request octo-org/demo@1111111"},
		{"pull request from a fork", "pull_request", prPayload(owner, ghRepo("stranger")), "pull request from a fork", ""},
		{"pull request from a deleted repository", "pull_request", prPayload(owner, nil), "pull request from a fork", ""},
		{"owner not allowed", "pull_request", prPayload("stranger", ghRepo("stranger")), "repository owner is not allowed", ""},
		{"push to a fork", "push", forkPush, "repository is a fork", ""},
		{"comment in a fork", "issue_comment", forkComment, "repository is a fork", ""},
		{"check run in a fork", "check_run", &github.CheckRunEvent{Action: new("rerequested"), CheckRun: appCheck, Repo: forkRepo(), Installation: installation, Sender: sender}, "repository is a fork", ""},
		{"pull request without its pull request", "pull_request", &github.PullRequestEvent{Action: new("opened"), Repo: ghRepo(owner)}, "no pull request in payload", ""},
		{"review requested", "pull_request", reviewRequest("octo-reviewer", "", "open"), "", "review_request octo-org/demo@1111111"},
		{"review requested from a team", "pull_request", reviewRequest("", "reviewers", "open"), "review requested from a team", ""},
		{"review requested without a reviewer", "pull_request", reviewRequest("", "", "open"), "no requested reviewer in payload", ""},
		{"review requested on a closed pull request", "pull_request", reviewRequest("octo-reviewer", "", "closed"), "review requested on a pull request that is not open", ""},
		{"review requested on a fork's pull request", "pull_request", forkReviewRequest, "pull request from a fork", ""},
		{"review requested without a default branch", "pull_request", noDefaultBranch, "no default branch in payload", ""},
		{"merge group checks requested", "merge_group", mergeGroup("checks_requested"), "", "merge_group octo-org/demo@1111111"},
		{"merge group destroyed", "merge_group", mergeGroup("destroyed"), "", "merge_group destroyed octo-org/demo"},
		{"merge group of another owner", "merge_group", &github.MergeGroupEvent{Action: new("destroyed"), MergeGroup: &github.MergeGroup{HeadSHA: new(sha1)}, Repo: ghRepo("stranger"), Installation: installation}, "repository owner is not allowed", ""},
		{"other merge group action", "merge_group", mergeGroup("created"), "unhandled merge_group action created", ""},
		{"check run rerequested", "check_run", &github.CheckRunEvent{Action: new("rerequested"), CheckRun: appCheck, Repo: ghRepo(owner), Installation: installation, Sender: sender}, "", "re-run octo-org/demo"},
		{"check run of another app", "check_run", &github.CheckRunEvent{Action: new("rerequested"), CheckRun: otherCheck, Repo: ghRepo(owner), Installation: installation, Sender: sender}, "check run belongs to another app", ""},
		{"requested action", "check_run", &github.CheckRunEvent{Action: new("requested_action"), RequestedAction: &github.RequestedAction{Identifier: "approve"}, CheckRun: appCheck, Repo: ghRepo(owner), Installation: installation, Sender: sender}, "unhandled check_run action requested_action", ""},
		{"check run created", "check_run", &github.CheckRunEvent{Action: new("created"), CheckRun: appCheck}, "unhandled check_run action created", ""},
		{"check run without a sender", "check_run", &github.CheckRunEvent{Action: new("rerequested"), CheckRun: appCheck, Repo: ghRepo(owner), Installation: installation}, "no sender in payload", ""},
		{"check suite rerequested", "check_suite", &github.CheckSuiteEvent{Action: new("rerequested"), CheckSuite: &github.CheckSuite{ID: new(int64(3)), App: &github.App{ID: new(int64(appID))}}, Repo: ghRepo(owner), Installation: installation, Sender: sender}, "", "re-run octo-org/demo"},
		{"check suite of another app", "check_suite", &github.CheckSuiteEvent{Action: new("rerequested"), CheckSuite: &github.CheckSuite{ID: new(int64(3)), App: &github.App{ID: new(int64(1))}}, Repo: ghRepo(owner), Installation: installation, Sender: sender}, "check suite belongs to another app", ""},
		{"check suite requested", "check_suite", &github.CheckSuiteEvent{Action: new("requested")}, "unhandled check_suite action requested", ""},
		{"comment command", "issue_comment", comment("created", "/deploy now\nplease", true), "", "comment octo-org/demo"},
		{"comment on an issue", "issue_comment", comment("created", "/deploy", false), "comment is not on a pull request", ""},
		{"plain comment", "issue_comment", comment("created", "looks good", true), "comment is not a command", ""},
		{"edited comment", "issue_comment", comment("edited", "/deploy", true), "unhandled issue_comment action edited", ""},
		{"unhandled event", "star", &github.StarEvent{Action: new("created")}, "unhandled event star", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, reason, err := app.Decode(tt.event, "d", body(t, tt.payload))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if reason != tt.reason {
				t.Fatalf("reason = %q, want %q", reason, tt.reason)
			}
			if (ev == nil) != (tt.reason != "") || (ev != nil && ev.String() != tt.want) {
				t.Fatalf("event = %v, want %q", ev, tt.want)
			}
		})
	}
}

func TestDecodeEvents(t *testing.T) {
	app, _ := newApp(t)
	tag := pushEvent("refs/tags/v1.0.0", "7777777777777777777777777777777777777777", false)
	tag.Created = new(true)
	tag.HeadCommit = &github.HeadCommit{ID: new(sha1)}
	pr := &github.PullRequestEvent{
		Action: new("ready_for_review"), Number: new(12), Repo: ghRepo(owner),
		Installation: &github.Installation{ID: new(int64(installationID))}, Sender: &github.User{Login: new("bob")},
		PullRequest: &github.PullRequest{
			Draft: new(true), User: &github.User{Login: new("carol")}, HTMLURL: new("https://github.com/octo-org/demo/pull/12"),
			Head: &github.PullRequestBranch{SHA: new(sha1), Ref: new("topic"), Repo: &github.Repository{FullName: new(owner + "/" + repoName)}},
			Base: &github.PullRequestBranch{SHA: new(baseSHA), Ref: new("main")},
		},
	}
	review := &github.PullRequestEvent{
		Action: new("review_requested"), Number: new(12), Repo: ghRepo(owner),
		Installation: &github.Installation{ID: new(int64(installationID))}, Sender: &github.User{Login: new("bob")},
		RequestedReviewer: &github.User{Login: new("Octo-Reviewer")},
		PullRequest: &github.PullRequest{
			State: new("open"), Draft: new(true), User: &github.User{Login: new("carol")}, HTMLURL: new("https://github.com/octo-org/demo/pull/12"),
			Head: &github.PullRequestBranch{SHA: new(sha1), Ref: new("topic"), Repo: &github.Repository{FullName: new(owner + "/" + repoName)}},
			Base: &github.PullRequestBranch{SHA: new(baseSHA), Ref: new("main")},
		},
	}
	trigger := sampleTrigger()
	marker, _ := Marker(trigger)
	checkRun := &github.CheckRunEvent{
		Action: new("rerequested"),
		CheckRun: &github.CheckRun{ID: new(int64(55)), Name: new("ci"), HeadSHA: new(sha1), Conclusion: new("failure"),
			App: &github.App{ID: new(int64(appID))}, Output: &github.CheckRunOutput{Text: new(marker)}},
		Repo: ghRepo(owner), Installation: &github.Installation{ID: new(int64(installationID))}, Sender: &github.User{Login: new("maintainer")},
	}
	comment := &github.IssueCommentEvent{
		Action: new("created"), Issue: &github.Issue{Number: new(5), PullRequestLinks: &github.PullRequestLinks{URL: new("u")}}, Repo: ghRepo(owner),
		Installation: &github.Installation{ID: new(int64(installationID))}, Sender: &github.User{Login: new("sender")},
		Comment: &github.IssueComment{ID: new(int64(9)), Body: new("  /deploy staging  \r\nthanks"), User: &github.User{Login: new("alice")}},
	}
	repository := ci.Repository{ID: repoID, Owner: owner, Name: repoName, FullName: owner + "/" + repoName, CloneURL: "https://github.com/octo-org/demo.git", DefaultBranch: "main"}
	tests := []struct {
		name    string
		event   string
		payload any
		want    ci.Event
	}{
		{
			name: "a tag push runs the tagged commit", event: "push", payload: tag,
			want: &ci.TriggerEvent{Trigger: ci.Trigger{
				Version: ci.TriggerVersion, Event: ci.EventPush, DeliveryID: "d", InstallationID: installationID,
				Repository: ci.Repository{ID: repoID, Owner: owner, Name: repoName, FullName: owner + "/" + repoName, DefaultBranch: "main"},
				Revision:   sha1, Ref: "refs/tags/v1.0.0", Tag: "v1.0.0", Sender: "alice",
				Push: &ci.Push{Before: baseSHA, After: "7777777777777777777777777777777777777777", Created: true},
			}},
		},
		{
			name: "a draft pull request", event: "pull_request", payload: pr,
			want: &ci.TriggerEvent{Draft: true, Trigger: ci.Trigger{
				Version: ci.TriggerVersion, Event: ci.EventPullRequest, Action: "ready_for_review", DeliveryID: "d", InstallationID: installationID,
				Repository: repository, Revision: sha1, Ref: "refs/pull/12/head", Branch: "topic", Sender: "bob",
				PullRequest: &ci.PullRequest{Number: 12, HeadRef: "topic", HeadSHA: sha1, BaseRef: "main", BaseSHA: baseSHA, HeadRepo: owner + "/" + repoName,
					Author: "carol", HTMLURL: "https://github.com/octo-org/demo/pull/12"},
			}},
		},
		{
			name: "a review request runs the head commit with the default branch's configuration", event: "pull_request", payload: review,
			want: &ci.TriggerEvent{Draft: true, Trigger: ci.Trigger{
				Version: ci.TriggerVersion, Event: ci.EventReviewRequest, Action: "review_requested", DeliveryID: "d", InstallationID: installationID,
				Repository: repository, Revision: sha1, Ref: "refs/pull/12/head", Branch: "topic", Sender: "bob",
				PullRequest: &ci.PullRequest{Number: 12, HeadRef: "topic", HeadSHA: sha1, BaseRef: "main", BaseSHA: baseSHA, HeadRepo: owner + "/" + repoName,
					Author: "carol", HTMLURL: "https://github.com/octo-org/demo/pull/12"},
				ReviewRequest: &ci.ReviewRequest{Reviewer: "Octo-Reviewer"},
				ConfigRef:     "main",
			}},
		},
		{
			name: "a re-run carries the check run's trigger", event: "check_run", payload: checkRun,
			want: &ci.RerunEvent{
				InstallationID: installationID, Repository: repository, Requester: "maintainer", DeliveryID: "d",
				Reports: []ci.ReportRef{{ID: 55, Name: "ci", Revision: sha1, Conclusion: ci.Failure, Trigger: &trigger}},
			},
		},
		{
			name: "a comment command is its first line", event: "issue_comment", payload: comment,
			want: &ci.CommandEvent{InstallationID: installationID, Repository: repository, Number: 5, CommentID: 9, Author: "alice", Line: "/deploy staging", DeliveryID: "d"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, reason, err := app.Decode(tt.event, "d", body(t, tt.payload))
			if err != nil || reason != "" || !reflect.DeepEqual(ev, tt.want) {
				t.Fatalf("Decode = %#v, %q, %v\nwant %#v", ev, reason, err, tt.want)
			}
		})
	}
}

func TestDecodeInvalidPayload(t *testing.T) {
	app, _ := newApp(t)
	if _, _, err := app.Decode("push", "d", []byte("{")); err == nil {
		t.Fatalf("Decode of an invalid payload must fail")
	}
}

func TestHandles(t *testing.T) {
	app, _ := newApp(t)
	for event, want := range map[string]bool{"push": true, "pull_request": true, "merge_group": true, "check_run": true, "check_suite": true, "issue_comment": true, "ping": false, "star": false} {
		if got := app.Handles(event); got != want {
			t.Errorf("Handles(%s) = %v, want %v", event, got, want)
		}
	}
}

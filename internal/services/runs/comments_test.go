package runs

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/ci/citest"
)

const commentConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: deploy
    pipelineRun: .tekton/ci.yaml
    on:
      comment: {pattern: "^/deploy\\b", branches: [main]}
    params:
      revision: "{{ .Revision }}"
      target: "{{ .Comment.Arguments }}"
      by: "{{ .Comment.Author }}"
`

// setupComment serves the comment configuration from the default branch only, and an open pull
// request #5 into main whose head is sha1.
func setupComment(h *harness) {
	h.host.SetFile(repo, "main", ".octomaton.yaml", commentConfig)
	h.host.SetFile(repo, "main", ".tekton/ci.yaml", ciRun)
	h.host.SetFile(repo, sha1, ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n") // the pull request's own copy is ignored
	h.host.SetPullRequest(repo, openPR(func(*ci.PullRequestState) {}))
}

func openPR(change func(*ci.PullRequestState)) ci.PullRequestState {
	pr := ci.PullRequestState{
		PullRequest: ci.PullRequest{Number: 5, HeadRef: "feature", HeadSHA: sha1, BaseRef: "main", BaseSHA: baseSHA,
			HeadRepo: repo.FullName, Author: "alice"},
		State: "open",
	}
	change(&pr)
	return pr
}

func command(id int64, author, line string) *ci.CommandEvent {
	return &ci.CommandEvent{InstallationID: installationID, Repository: repo, Number: 5, CommentID: id, Author: author, Line: line, DeliveryID: "comment"}
}

func TestCommentCommandRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setupComment(h)
	h.svc.Handle(ctx, command(101, "maintainer", "/deploy staging --fast"))

	runs := h.runner.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	tr := runs[0].Trigger
	if tr.Event != ci.EventComment || tr.Revision != sha1 || tr.ConfigRef != "main" || tr.Ref != "refs/pull/5/head" || tr.Branch != "feature" ||
		tr.Comment == nil || *tr.Comment != (ci.Comment{ID: 101, Author: "maintainer", Command: "/deploy", Arguments: "staging --fast"}) || tr.PullRequest.Number != 5 {
		t.Fatalf("comment trigger = %+v", tr)
	}
	if params := h.runner.Spec(runs[0].ID).Params; !reflect.DeepEqual(params, map[string]string{"target": "staging --fast", "by": "maintainer", "revision": sha1}) {
		t.Fatalf("params = %v", params)
	}
	if r := h.onlyReport("deploy"); r.Revision != sha1 {
		t.Fatalf("the report is on the pull request head: %+v", r)
	}
	if reactions := h.host.Reactions(); !reflect.DeepEqual(reactions, []citest.Reaction{{Repository: repo.FullName, CommentID: 101, Reaction: "eyes"}}) {
		t.Fatalf("reactions = %+v", reactions)
	}
	// A redelivery of the same comment finds its run; another comment runs again.
	h.svc.Handle(ctx, command(101, "maintainer", "/deploy staging --fast"))
	if len(h.runner.Runs()) != 1 {
		t.Fatalf("a comment runs once")
	}
	h.svc.Handle(ctx, command(102, "maintainer", "/deploy staging --fast"))
	if len(h.runner.Runs()) != 2 {
		t.Fatalf("another comment runs again")
	}
}

func TestADeclineIsSaidPastTheJobsDeadline(t *testing.T) {
	h := newHarness(t)
	setupComment(h)
	h.host.FailFile(repo, "main", ".octomaton.yaml", context.DeadlineExceeded)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.svc.HandleComment(ctx, command(203, "maintainer", "/deploy"))
	reactions, comments := h.host.Reactions(), h.host.Comments()
	if len(reactions) != 1 || reactions[0].Reaction != "-1" || len(comments) != 1 {
		t.Fatalf("reactions = %+v, comments = %+v; want the decline even after the job's context ended", reactions, comments)
	}
	mustContain(t, comments[0].Body, "Octomaton could not read .octomaton.yaml")
}

func TestCommentCommandDeclines(t *testing.T) {
	tests := []struct {
		name   string
		author string
		pr     func(*ci.PullRequestState)
		setup  func(h *harness)
		want   string
	}{
		{name: "a closed pull request", author: "maintainer", pr: func(p *ci.PullRequestState) { p.State = "closed" }, want: "the pull request is closed"},
		{name: "a draft", author: "maintainer", pr: func(p *ci.PullRequestState) { p.Draft = true }, want: "the pull request is a draft"},
		{name: "no write access", author: "reader", want: "reader does not have write access to octo-org/demo"},
		{name: "an unknown user", author: "stranger", want: "stranger does not have write access"},
		{name: "another base branch", author: "maintainer", pr: func(p *ci.PullRequestState) { p.BaseRef = "release" },
			want: "deploy runs only on pull requests into main, and this one is into release"},
		// A command has no check: its reply says so.
		{name: "a configuration GitHub would not serve", author: "maintainer", want: "Octomaton could not read .octomaton.yaml; comment again to try again",
			setup: func(h *harness) { h.host.FailFile(repo, "main", ".octomaton.yaml", gitHubDown) }},
		{name: "a refused run", author: "maintainer", want: "the PipelineRun references Secret",
			setup: func(h *harness) {
				h.runner.Fail("Create", &ci.Refusal{Title: "Refused", Reason: "the PipelineRun references Secret \"x\""})
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupComment(h)
			if tt.pr != nil {
				h.host.SetPullRequest(repo, openPR(tt.pr))
			}
			if tt.setup != nil {
				tt.setup(h)
			}
			h.svc.HandleComment(context.Background(), command(202, tt.author, "/deploy"))
			if len(h.runner.Runs()) != 0 || len(h.host.Reports()) != 0 {
				t.Fatalf("a declined command must not run, nor report")
			}
			reactions, comments := h.host.Reactions(), h.host.Comments()
			if len(reactions) != 1 || reactions[0].Reaction != "-1" {
				t.Fatalf("reactions = %+v, want a thumbs-down", reactions)
			}
			if len(comments) != 1 || comments[0].Number != 5 || !strings.HasPrefix(comments[0].Body, "@"+tt.author+" `/deploy` was not run: ") {
				t.Fatalf("comments = %+v", comments)
			}
			mustContain(t, comments[0].Body, tt.want)
		})
	}
}

func TestCommentsThatAreNotCommandsAreIgnored(t *testing.T) {
	h := newHarness(t)
	setupComment(h)
	h.svc.HandleComment(context.Background(), command(303, "maintainer", "/deployment please"))
	if len(h.runner.Runs()) != 0 || len(h.host.Reactions()) != 0 || len(h.host.Comments()) != 0 {
		t.Fatalf("a comment matching no command gets no answer")
	}
}

func TestHandle(t *testing.T) {
	labeled := branchPR(sha1)
	labeled.Action = "labeled"
	tests := []struct {
		name         string
		event        ci.Event
		wantRuns     int
		wantReports  []string
		wantNotified []string
	}{
		{name: "a push to the default branch runs and has its schedules read", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "main")},
			wantRuns: 1, wantReports: []string{"ci"}, wantNotified: []string{repo.FullName}},
		{name: "a push to another branch", event: &ci.TriggerEvent{Trigger: pushTrigger(sha1, "feature")}},
		{name: "a pull request labeled has no configuration problems reported", event: &ci.TriggerEvent{Trigger: labeled}},
		{name: "a draft pull request runs by default", event: &ci.TriggerEvent{Trigger: branchPR(sha1), Draft: true}, wantRuns: 1, wantReports: []string{"ci"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.files(sha1, "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: ci, pipelineRun: .tekton/ci.yaml, on: {push: {branches: [main]}, pull_request: {}}}\n", ciRun)
			if strings.Contains(tt.name, "configuration problems") {
				h.files(sha1, "apiVersion: nope\n", "")
			}
			h.svc.Handle(context.Background(), tt.event)
			var reports []string
			for _, r := range h.host.Reports() {
				reports = append(reports, r.Name)
			}
			if len(h.runner.Runs()) != tt.wantRuns || !reflect.DeepEqual(reports, tt.wantReports) || !reflect.DeepEqual(h.notified, tt.wantNotified) {
				t.Fatalf("runs %d, reports %v, notified %v; want %d, %v, %v", len(h.runner.Runs()), reports, h.notified, tt.wantRuns, tt.wantReports, tt.wantNotified)
			}
		})
	}
}

func TestCommentCommandOnForkIsIgnored(t *testing.T) {
	tests := []struct {
		name       string
		headRepo   string
		unreadable bool
	}{
		{name: "pull request from a fork", headRepo: "stranger/demo"},
		{name: "pull request from a deleted repository", headRepo: ""},
		// The reply that says so is an answer too.
		{name: "a configuration GitHub would not serve", headRepo: "stranger/demo", unreadable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupComment(h)
			h.host.SetPullRequest(repo, openPR(func(pr *ci.PullRequestState) { pr.HeadRepo = tt.headRepo }))
			if tt.unreadable {
				h.host.FailFile(repo, "main", ".octomaton.yaml", gitHubDown)
			}
			h.svc.Handle(context.Background(), command(101, "maintainer", "/deploy staging"))
			if len(h.runner.Runs()) != 0 || len(h.host.Reactions()) != 0 || len(h.host.Comments()) != 0 {
				t.Fatalf("runs = %d, reactions = %+v, comments = %+v, want none", len(h.runner.Runs()), h.host.Reactions(), h.host.Comments())
			}
		})
	}
}

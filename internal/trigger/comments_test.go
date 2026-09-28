package trigger

import (
	"context"
	"strings"
	"testing"

	"github.com/arikkfir-org/octomatron/internal/checkrun"
	"github.com/arikkfir-org/octomatron/internal/githubapp/githubtest"
	"github.com/arikkfir-org/octomatron/internal/tekton"
	"github.com/google/go-github/v92/github"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const commentConfig = `
apiVersion: octomatron.kfirs.com/v1
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

// setupComment serves the comment configuration from the default branch only,
// and an open pull request #5 into main whose head is sha1.
func setupComment(h *harness) {
	h.gh.AddFile(fullName, "main", ".octomatron.yaml", commentConfig)
	h.gh.AddFile(fullName, "main", ".tekton/ci.yaml", ciRun)
	h.gh.AddFile(fullName, sha1, ".octomatron.yaml", "apiVersion: octomatron.kfirs.com/v1\npipelines: []\n") // the pull request's own copy is ignored
	h.gh.SetPullRequest(fullName, githubtest.PullRequest{Number: 5, State: "open", HeadSHA: sha1, HeadRef: "feature", BaseRef: "main", BaseSHA: baseSHA, HeadRepo: "stranger/demo", Author: "stranger", AuthorAssociation: "NONE"})
}

func commentReq(id int64, author, line string) CommentRequest {
	return CommentRequest{InstallationID: installationID, Repository: repo(), Number: 5, CommentID: id, Author: author, Line: line, DeliveryID: "comment"}
}

func TestCommentCommandRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setupComment(h)
	h.svc.HandleComment(ctx, commentReq(101, "maintainer", "/deploy staging --fast"))

	runs := h.allRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	pr := &runs[0]
	c := contextAnnotation(t, pr)
	if c.Event != checkrun.EventComment || c.Revision != sha1 || c.ConfigRef != "main" || c.Comment == nil ||
		c.Comment.Command != "/deploy" || c.Comment.Arguments != "staging --fast" || c.Comment.ID != 101 || c.PullRequest.Number != 5 {
		t.Fatalf("comment context = %+v", c)
	}
	if pr.GetLabels()[tekton.LabelComment] != "101" || pr.GetLabels()[tekton.LabelEvent] != "comment" {
		t.Fatalf("labels = %v", pr.GetLabels())
	}
	params := map[string]string{}
	list, _, _ := unstructured.NestedSlice(pr.Object, "spec", "params")
	for _, p := range list {
		m := p.(map[string]any)
		params[m["name"].(string)] = m["value"].(string)
	}
	if params["target"] != "staging --fast" || params["by"] != "maintainer" || params["revision"] != sha1 {
		t.Fatalf("params = %v", params)
	}
	if check := h.onlyCheck("deploy"); check.HeadSHA != sha1 {
		t.Fatalf("the check runs on the pull request head: %+v", check)
	}
	reactions := h.gh.Reactions()
	if len(reactions) != 1 || reactions[0].Content != "eyes" || reactions[0].CommentID != 101 {
		t.Fatalf("reactions = %+v", reactions)
	}
	// A redelivery of the same comment finds the run.
	h.svc.HandleComment(ctx, commentReq(101, "maintainer", "/deploy staging --fast"))
	if len(h.allRuns()) != 1 {
		t.Fatalf("a comment runs once")
	}
}

func TestCommentCommandDeclines(t *testing.T) {
	tests := []struct {
		name   string
		author string
		line   string
		pr     func(*githubtest.PullRequest)
		want   string
	}{
		{name: "closed pull request", author: "maintainer", line: "/deploy", pr: func(p *githubtest.PullRequest) { p.State = "closed" }, want: "the pull request is closed"},
		{name: "draft", author: "maintainer", line: "/deploy", pr: func(p *githubtest.PullRequest) { p.Draft = true }, want: "the pull request is a draft"},
		{name: "no write access", author: "reader", line: "/deploy", want: "reader does not have write access to octo-org/demo"},
		{name: "unknown user", author: "stranger", line: "/deploy", want: "stranger does not have write access"},
		{name: "other base branch", author: "maintainer", line: "/deploy", pr: func(p *githubtest.PullRequest) { p.BaseRef = "release" }, want: "deploy runs only on pull requests into main, and this one is into release"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupComment(h)
			pr := githubtest.PullRequest{Number: 5, State: "open", HeadSHA: sha1, HeadRef: "feature", BaseRef: "main"}
			if tt.pr != nil {
				tt.pr(&pr)
			}
			h.gh.SetPullRequest(fullName, pr)
			h.svc.HandleComment(context.Background(), commentReq(202, tt.author, tt.line))
			if len(h.allRuns()) != 0 {
				t.Fatalf("a declined command must not run")
			}
			reactions, comments := h.gh.Reactions(), h.gh.Comments()
			if len(reactions) != 1 || reactions[0].Content != "-1" {
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
	h.svc.HandleComment(context.Background(), commentReq(303, "maintainer", "/deployment please"))
	if len(h.allRuns()) != 0 || len(h.gh.Reactions()) != 0 || len(h.gh.Comments()) != 0 {
		t.Fatalf("a comment matching no command gets no answer")
	}
}

func TestCommentRequestFromPayload(t *testing.T) {
	ev := &github.IssueCommentEvent{
		Action:       new("created"),
		Issue:        &github.Issue{Number: new(5), PullRequestLinks: &github.PullRequestLinks{URL: new("x")}},
		Comment:      &github.IssueComment{ID: new(int64(11)), Body: new("  /deploy prod\r\nsecond line"), User: &github.User{Login: new("alice")}},
		Repo:         &github.Repository{ID: new(int64(repoID)), Name: new(repoName), FullName: new(fullName), Owner: &github.User{Login: new(owner)}, DefaultBranch: new("main")},
		Installation: &github.Installation{ID: new(int64(installationID))},
	}
	req, reason := commentRequest(ev, "d")
	if reason != "" || req.Line != "/deploy prod" || req.Author != "alice" || req.CommentID != 11 || req.Number != 5 || req.Repository.DefaultBranch != "main" {
		t.Fatalf("commentRequest = %+v, %q", req, reason)
	}
	ev.Issue.PullRequestLinks = nil
	if _, reason := commentRequest(ev, "d"); reason != "comment is not on a pull request" {
		t.Fatalf("issue comment reason = %q", reason)
	}
}

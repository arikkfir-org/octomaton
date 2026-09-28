package githubapp_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arikkfir-org/octomatron/internal/githubapp"
	"github.com/arikkfir-org/octomatron/internal/githubapp/githubtest"
	"github.com/google/go-github/v92/github"
)

const (
	appID          = 42
	installationID = 7
	repo           = "octo-org/octo-repo"
)

func newApp(t *testing.T) (*githubapp.App, *githubtest.Server) {
	t.Helper()
	srv := githubtest.NewServer(t, appID)
	app, err := githubapp.New(appID, githubtest.Key(), githubapp.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return app, srv
}

func TestInstallationTokenIsCached(t *testing.T) {
	app, srv := newApp(t)
	srv.AddFile(repo, "abc", ".octomatron.yaml", "hello")
	gh := app.Installation(installationID)
	for range 3 {
		if _, err := gh.GetFile(context.Background(), "octo-org", "octo-repo", ".octomatron.yaml", "abc"); err != nil {
			t.Fatalf("GetFile: %v", err)
		}
	}
	if got := len(srv.TokenRequests()); got != 1 {
		t.Fatalf("installation token requests = %d, want 1 (token must be cached)", got)
	}
	if app.Installation(installationID) != gh {
		t.Fatalf("Installation() must return the cached client")
	}
}

func TestGetFile(t *testing.T) {
	app, srv := newApp(t)
	srv.AddFile(repo, "abc", ".tekton/ci.yaml", "kind: PipelineRun\n")
	gh := app.Installation(installationID)
	ctx := context.Background()

	data, err := gh.GetFile(ctx, "octo-org", "octo-repo", ".tekton/ci.yaml", "abc")
	if err != nil || string(data) != "kind: PipelineRun\n" {
		t.Fatalf("GetFile = %q, %v", data, err)
	}
	if _, err := gh.GetFile(ctx, "octo-org", "octo-repo", ".tekton/ci.yaml", "other"); !errors.Is(err, githubapp.ErrNotFound) {
		t.Fatalf("GetFile at unknown ref: err = %v, want ErrNotFound", err)
	}
	srv.SetFailFiles(true)
	if _, err := gh.GetFile(ctx, "octo-org", "octo-repo", ".tekton/ci.yaml", "abc"); err == nil || errors.Is(err, githubapp.ErrNotFound) {
		t.Fatalf("GetFile on server error: err = %v, want a non-NotFound error", err)
	}
}

func TestPullRequestFiles(t *testing.T) {
	app, srv := newApp(t)
	var many []string
	for i := range 250 {
		many = append(many, fmt.Sprintf("dir/file-%03d.go", i))
	}
	srv.SetPullRequestFiles(repo, 1, many)
	var tooMany []string
	for i := range 3000 {
		tooMany = append(tooMany, fmt.Sprintf("f%d", i))
	}
	srv.SetPullRequestFiles(repo, 2, tooMany)
	gh := app.Installation(installationID)

	files, err := gh.PullRequestFiles(context.Background(), "octo-org", "octo-repo", 1)
	if err != nil {
		t.Fatalf("PullRequestFiles: %v", err)
	}
	if len(files.Files) != 250 || !files.Complete {
		t.Fatalf("got %d files (complete=%v), want 250 complete (all pages)", len(files.Files), files.Complete)
	}
	files, err = gh.PullRequestFiles(context.Background(), "octo-org", "octo-repo", 2)
	if err != nil {
		t.Fatalf("PullRequestFiles: %v", err)
	}
	if files.Complete {
		t.Fatalf("a list of 3000 files must be reported as incomplete")
	}
}

func TestCompareFiles(t *testing.T) {
	app, srv := newApp(t)
	srv.SetComparison(repo, "a", "b", []string{"x.go", "y.go"})
	var many []string
	for i := range 300 {
		many = append(many, fmt.Sprintf("f%d", i))
	}
	srv.SetComparison(repo, "a", "c", many)
	gh := app.Installation(installationID)

	files, err := gh.CompareFiles(context.Background(), "octo-org", "octo-repo", "a", "b")
	if err != nil || !files.Complete || !slices.Equal(files.Files, []string{"x.go", "y.go"}) {
		t.Fatalf("CompareFiles = %+v, %v", files, err)
	}
	files, err = gh.CompareFiles(context.Background(), "octo-org", "octo-repo", "a", "c")
	if err != nil || files.Complete {
		t.Fatalf("CompareFiles with 300 files = complete %v, %v; want incomplete", files.Complete, err)
	}
	if _, err := gh.CompareFiles(context.Background(), "octo-org", "octo-repo", "a", "missing"); err == nil {
		t.Fatalf("CompareFiles of unknown commits must fail")
	}
}

func TestCheckRuns(t *testing.T) {
	app, srv := newApp(t)
	gh := app.Installation(installationID)
	ctx := context.Background()

	cr, err := gh.CreateCheckRun(ctx, "octo-org", "octo-repo", github.CreateCheckRunOptions{
		Name: "ci", HeadSHA: "abc", Status: new("queued"), ExternalID: new("ns/run"),
	})
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := gh.UpdateCheckRun(ctx, "octo-org", "octo-repo", cr.GetID(), githubapp.CheckRunUpdate{
		Status:    new("in_progress"),
		StartedAt: &github.Timestamp{Time: started},
		Output:    &github.CheckRunOutput{Title: new("Running"), Summary: new("s")},
	}); err != nil {
		t.Fatalf("UpdateCheckRun: %v", err)
	}
	stored, _ := srv.CheckRun(cr.GetID())
	if stored.Status != "in_progress" || stored.StartedAt != "2026-01-02T03:04:05Z" || stored.Title != "Running" || stored.ExternalID != "ns/run" {
		t.Fatalf("stored check run = %+v", stored)
	}
	got, err := gh.GetCheckRun(ctx, "octo-org", "octo-repo", cr.GetID())
	if err != nil || got.GetName() != "ci" {
		t.Fatalf("GetCheckRun = %v, %v", got, err)
	}
	if _, err := gh.GetCheckRun(ctx, "octo-org", "octo-repo", 999999); !errors.Is(err, githubapp.ErrNotFound) {
		t.Fatalf("GetCheckRun(unknown) err = %v, want ErrNotFound", err)
	}
	runs, err := gh.SuiteCheckRuns(ctx, "octo-org", "octo-repo", srv.SuiteID(repo, "abc"))
	if err != nil || len(runs) != 1 || runs[0].GetID() != cr.GetID() {
		t.Fatalf("SuiteCheckRuns = %v, %v", runs, err)
	}
}

func TestPermissionLevel(t *testing.T) {
	app, srv := newApp(t)
	srv.SetPermission(repo, "alice", "write")
	gh := app.Installation(installationID)
	for user, want := range map[string]string{"alice": "write", "mallory": "none"} {
		got, err := gh.PermissionLevel(context.Background(), "octo-org", "octo-repo", user)
		if err != nil || got != want {
			t.Errorf("PermissionLevel(%s) = %q, %v; want %q", user, got, err, want)
		}
	}
}

func TestCanWrite(t *testing.T) {
	for level, want := range map[string]bool{"admin": true, "maintain": true, "write": true, "triage": false, "read": false, "none": false, "": false} {
		if got := githubapp.CanWrite(level); got != want {
			t.Errorf("CanWrite(%q) = %v, want %v", level, got, want)
		}
	}
}

func TestRepositoryToken(t *testing.T) {
	app, srv := newApp(t)
	tok, err := app.RepositoryToken(context.Background(), installationID, 1234, nil)
	if err != nil {
		t.Fatalf("RepositoryToken: %v", err)
	}
	if !strings.HasPrefix(tok.Value, "ghs_") || tok.ExpiresAt.Before(time.Now()) {
		t.Fatalf("token = %+v", tok)
	}
	reqs := srv.TokenRequests()
	if len(reqs) != 1 {
		t.Fatalf("token requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.InstallationID != installationID || !slices.Equal(r.RepositoryIDs, []int64{1234}) || len(r.Permissions) != 1 || r.Permissions["contents"] != "read" {
		t.Fatalf("token request = %+v, want repository 1234 with contents:read only", r)
	}

	if _, err := app.RepositoryToken(context.Background(), installationID, 1234, map[string]string{"checks": "write", "contents": "read"}); err != nil {
		t.Fatalf("RepositoryToken with permissions: %v", err)
	}
	if got := srv.TokenRequests()[1].Permissions; got["checks"] != "write" || got["contents"] != "read" {
		t.Fatalf("permissions sent = %v", got)
	}
}

func TestParsePermissions(t *testing.T) {
	tests := []struct {
		name    string
		in      map[string]string
		wantErr string
	}{
		{name: "contents read", in: map[string]string{"contents": "read"}},
		{name: "several", in: map[string]string{"contents": "write", "pull_requests": "read", "checks": "write"}},
		{name: "unknown permission", in: map[string]string{"contnets": "read"}, wantErr: "contnets"},
		{name: "bad level", in: map[string]string{"contents": "all"}, wantErr: "access level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := githubapp.ParsePermissions(tt.in)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPullRequestAndBranchHead(t *testing.T) {
	app, srv := newApp(t)
	srv.SetPullRequest(repo, githubtest.PullRequest{Number: 3, State: "open", Draft: true, HeadRef: "topic", HeadSHA: "abc", HeadRepo: "fork/octo-repo", BaseRef: "main", Author: "carol", AuthorAssociation: "NONE"})
	srv.SetBranch(repo, "feature/x", "def")
	gh := app.Installation(installationID)
	ctx := context.Background()

	pr, err := gh.PullRequest(ctx, "octo-org", "octo-repo", 3)
	if err != nil || pr.State != "open" || !pr.Draft || pr.HeadSHA != "abc" || pr.HeadRepo != "fork/octo-repo" || pr.BaseRef != "main" || pr.Author != "carol" {
		t.Fatalf("PullRequest = %+v, %v", pr, err)
	}
	if _, err := gh.PullRequest(ctx, "octo-org", "octo-repo", 4); !errors.Is(err, githubapp.ErrNotFound) {
		t.Fatalf("PullRequest(missing) err = %v", err)
	}
	if sha, err := gh.BranchHead(ctx, "octo-org", "octo-repo", "feature/x"); err != nil || sha != "def" {
		t.Fatalf("BranchHead = %q, %v", sha, err)
	}
	if _, err := gh.BranchHead(ctx, "octo-org", "octo-repo", "gone"); !errors.Is(err, githubapp.ErrNotFound) {
		t.Fatalf("BranchHead(missing) err = %v", err)
	}
}

func TestFindCheckRun(t *testing.T) {
	app, srv := newApp(t)
	srv.AddCheckRun(githubtest.CheckRun{Repo: repo, Name: "ci", HeadSHA: "abc", ExternalID: "ns/other"})
	want := srv.AddCheckRun(githubtest.CheckRun{Repo: repo, Name: "ci", HeadSHA: "abc", ExternalID: "ns/run"})
	srv.AddCheckRun(githubtest.CheckRun{Repo: repo, Name: "lint", HeadSHA: "abc", ExternalID: "ns/run"})
	gh := app.Installation(installationID)
	id, err := gh.FindCheckRun(context.Background(), "octo-org", "octo-repo", "abc", "ci", "ns/run")
	if err != nil || id != want {
		t.Fatalf("FindCheckRun = %d, %v; want %d", id, err, want)
	}
	if id, err := gh.FindCheckRun(context.Background(), "octo-org", "octo-repo", "abc", "ci", "ns/none"); err != nil || id != 0 {
		t.Fatalf("FindCheckRun(none) = %d, %v", id, err)
	}
}

func TestReactAndComment(t *testing.T) {
	app, srv := newApp(t)
	gh := app.Installation(installationID)
	if err := gh.React(context.Background(), "octo-org", "octo-repo", 77, "eyes"); err != nil {
		t.Fatalf("React: %v", err)
	}
	if err := gh.Comment(context.Background(), "octo-org", "octo-repo", 3, "hello"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if r := srv.Reactions(); len(r) != 1 || r[0].CommentID != 77 || r[0].Content != "eyes" {
		t.Fatalf("reactions = %+v", r)
	}
	if c := srv.Comments(); len(c) != 1 || c[0].Number != 3 || c[0].Body != "hello" {
		t.Fatalf("comments = %+v", c)
	}
}

func TestInstallationsAndRepositories(t *testing.T) {
	app, srv := newApp(t)
	srv.AddInstallation(installationID, "octo-org", githubtest.Repository{ID: 1, Owner: "octo-org", Name: "octo-repo", DefaultBranch: "main"}, githubtest.Repository{ID: 2, Owner: "octo-org", Name: "old", DefaultBranch: "master", Archived: true})
	insts, err := app.Installations(context.Background())
	if err != nil || len(insts) != 1 || insts[0].ID != installationID || insts[0].Account != "octo-org" {
		t.Fatalf("Installations = %+v, %v", insts, err)
	}
	repos, err := app.Installation(installationID).Repositories(context.Background())
	if err != nil || len(repos) != 2 || repos[0].FullName != "octo-org/octo-repo" || repos[0].DefaultBranch != "main" || !repos[1].Archived {
		t.Fatalf("Repositories = %+v, %v", repos, err)
	}
}

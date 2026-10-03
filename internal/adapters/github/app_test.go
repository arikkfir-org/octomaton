package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"octomaton.dev/internal/adapters/github/githubtest"
	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/system/metrics/metricstest"
)

const (
	appID          = 42
	installationID = 7
	fullName       = "octo-org/octo-repo"
)

var repo = ci.Repository{ID: 1, Owner: "octo-org", Name: "octo-repo", FullName: fullName}

func newApp(t *testing.T, opts ...Option) (*App, *githubtest.Server) {
	t.Helper()
	srv := githubtest.NewServer(t, appID)
	app, err := New(appID, githubtest.Key(), append([]Option{WithBaseURL(srv.URL), WithRetries(fastRetries)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return app, srv
}

// fastRetries retries as DefaultRetries does, without the waits.
var fastRetries = Retries{Max: DefaultRetries.Max, WaitMin: time.Millisecond, WaitMax: time.Millisecond}

func TestInstallationTokenIsCached(t *testing.T) {
	app, srv := newApp(t)
	srv.AddFile(fullName, "abc", ".octomaton.yaml", "hello")
	gh := app.Installation(installationID)
	for range 3 {
		if _, err := gh.ReadFile(context.Background(), repo, ".octomaton.yaml", "abc"); err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
	}
	if got := len(srv.TokenRequests()); got != 1 {
		t.Fatalf("installation token requests = %d, want 1 (the token must be cached)", got)
	}
	if app.Installation(installationID) != gh {
		t.Fatalf("Installation() must return the cached client")
	}
}

func TestReadFile(t *testing.T) {
	app, srv := newApp(t)
	srv.AddFile(fullName, "abc", ".tekton/ci.yaml", "kind: PipelineRun\n")
	gh := app.Installation(installationID)
	ctx := context.Background()

	data, err := gh.ReadFile(ctx, repo, ".tekton/ci.yaml", "abc")
	if err != nil || string(data) != "kind: PipelineRun\n" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	if _, err := gh.ReadFile(ctx, repo, ".tekton/ci.yaml", "other"); !errors.Is(err, ci.ErrNotFound) {
		t.Fatalf("ReadFile at an unknown ref: err = %v, want ErrNotFound", err)
	}
	// An empty ref reads the default branch, as for a pipelineRun in another repository.
	tooling := ci.Repository{Owner: "octo-org", Name: "tooling", FullName: "octo-org/tooling"}
	srv.AddFile(tooling.FullName, "", "reviewer/pipelinerun.yaml", "kind: PipelineRun\n")
	if data, err := gh.ReadFile(ctx, tooling, "reviewer/pipelinerun.yaml", ""); err != nil || string(data) != "kind: PipelineRun\n" {
		t.Fatalf("ReadFile at the default branch = %q, %v", data, err)
	}
	srv.SetFailFiles(true)
	if _, err := gh.ReadFile(ctx, repo, ".tekton/ci.yaml", "abc"); err == nil || errors.Is(err, ci.ErrNotFound) || !strings.Contains(err.Error(), ".tekton/ci.yaml@abc") {
		t.Fatalf("ReadFile on a server error: err = %v, want another error than ErrNotFound, naming the file and ref", err)
	}
	if _, err := gh.ReadFile(ctx, tooling, "reviewer/pipelinerun.yaml", ""); err == nil || !strings.Contains(err.Error(), "reviewer/pipelinerun.yaml on the default branch") {
		t.Fatalf("ReadFile at the default branch on a server error: err = %v, want one naming the default branch", err)
	}
}

func TestChangedFiles(t *testing.T) {
	app, srv := newApp(t)
	files := func(n int) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("dir/file-%04d.go", i))
		}
		return out
	}
	srv.SetPullRequestFiles(fullName, 1, files(250))
	srv.SetPullRequestFiles(fullName, 2, files(3000))
	srv.SetComparison(fullName, "a", "b", []string{"x.go", "y.go"})
	srv.SetComparison(fullName, "a", "c", files(300))
	gh := app.Installation(installationID)
	ctx := context.Background()
	tests := []struct {
		name         string
		get          func() (ci.ChangedFiles, error)
		wantCount    int
		wantComplete bool
	}{
		{"every page of a pull request's files", func() (ci.ChangedFiles, error) { return gh.PullRequestFiles(ctx, repo, 1) }, 250, true},
		{"a pull request at the API's limit", func() (ci.ChangedFiles, error) { return gh.PullRequestFiles(ctx, repo, 2) }, 3000, false},
		{"a comparison", func() (ci.ChangedFiles, error) { return gh.CompareFiles(ctx, repo, "a", "b") }, 2, true},
		{"a comparison at the API's limit", func() (ci.ChangedFiles, error) { return gh.CompareFiles(ctx, repo, "a", "c") }, 300, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.get()
			if err != nil || len(got.Files) != tt.wantCount || got.Complete != tt.wantComplete {
				t.Fatalf("got %d files (complete %v), %v; want %d (complete %v)", len(got.Files), got.Complete, err, tt.wantCount, tt.wantComplete)
			}
		})
	}
	if _, err := gh.CompareFiles(ctx, repo, "a", "missing"); err == nil {
		t.Fatalf("CompareFiles of unknown commits must fail")
	}
}

func TestPermission(t *testing.T) {
	app, srv := newApp(t)
	srv.SetPermission(fullName, "alice", "write")
	gh := app.Installation(installationID)
	for user, want := range map[string]ci.Permission{"alice": "write", "mallory": "none"} {
		got, err := gh.Permission(context.Background(), repo, user)
		if err != nil || got != want {
			t.Errorf("Permission(%s) = %q, %v; want %q", user, got, err, want)
		}
	}
}

func TestTokens(t *testing.T) {
	app, srv := newApp(t)
	ctx := context.Background()
	tests := []struct {
		name        string
		all         bool // InstallationToken rather than RepositoryToken
		permissions map[string]string
		want        map[string]string
		wantRepos   []int64 // nil: no repository_ids, so every repository of the installation
	}{
		{"contents:read by default", false, nil, map[string]string{"contents": "read"}, []int64{1234}},
		{"the permissions asked for", false, map[string]string{"checks": "write", "contents": "read"}, map[string]string{"checks": "write", "contents": "read"}, []int64{1234}},
		{"every repository, contents:read by default", true, nil, map[string]string{"contents": "read"}, nil},
		{"every repository, the permissions asked for", true, map[string]string{"contents": "read", "pull_requests": "read"}, map[string]string{"contents": "read", "pull_requests": "read"}, nil},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tok ci.Token
			var err error
			if tt.all {
				tok, err = app.InstallationToken(ctx, installationID, tt.permissions)
			} else {
				tok, err = app.RepositoryToken(ctx, installationID, 1234, tt.permissions)
			}
			if err != nil || !strings.HasPrefix(tok.Value, "ghs_") || tok.ExpiresAt.Before(time.Now()) || fmt.Sprint(tok.Permissions) != fmt.Sprint(tt.want) {
				t.Fatalf("token = %+v, %v", tok, err)
			}
			r := srv.TokenRequests()[i]
			if r.InstallationID != installationID || !slices.Equal(r.RepositoryIDs, tt.wantRepos) || fmt.Sprint(r.Permissions) != fmt.Sprint(tt.want) {
				t.Fatalf("token request = %+v, want repositories %v with %v", r, tt.wantRepos, tt.want)
			}
		})
	}
	if _, err := app.RepositoryToken(ctx, installationID, 1234, map[string]string{"contents": "all"}); err == nil {
		t.Fatalf("RepositoryToken must refuse invalid permissions")
	}
	if _, err := app.InstallationToken(ctx, installationID, map[string]string{"contents": "all"}); err == nil {
		t.Fatalf("InstallationToken must refuse invalid permissions")
	}
}

func TestCheckPermissions(t *testing.T) {
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
	app, _ := newApp(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := app.CheckPermissions(tt.in)
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
	srv.SetPullRequest(fullName, githubtest.PullRequest{Number: 3, State: "open", Draft: true, HeadRef: "topic", HeadSHA: "abc", HeadRepo: "fork/octo-repo", BaseRef: "main", BaseSHA: "def", Author: "carol", AuthorAssociation: "NONE"})
	srv.SetBranch(fullName, "feature/x", "def")
	gh := app.Installation(installationID)
	ctx := context.Background()

	pr, err := gh.PullRequest(ctx, repo, 3)
	want := ci.PullRequestState{
		PullRequest: ci.PullRequest{Number: 3, HeadRef: "topic", HeadSHA: "abc", BaseRef: "main", BaseSHA: "def", HeadRepo: "fork/octo-repo",
			Author: "carol", HTMLURL: "https://github.com/octo-org/octo-repo/pull/3"},
		State: "open", Draft: true,
	}
	if err != nil || pr != want {
		t.Fatalf("PullRequest = %+v, %v\nwant %+v", pr, err, want)
	}
	if _, err := gh.PullRequest(ctx, repo, 4); !errors.Is(err, ci.ErrNotFound) {
		t.Fatalf("PullRequest(missing) err = %v", err)
	}
	if sha, err := gh.BranchHead(ctx, repo, "feature/x"); err != nil || sha != "def" {
		t.Fatalf("BranchHead = %q, %v", sha, err)
	}
	if _, err := gh.BranchHead(ctx, repo, "gone"); !errors.Is(err, ci.ErrNotFound) {
		t.Fatalf("BranchHead(missing) err = %v", err)
	}
}

func TestReactAndComment(t *testing.T) {
	app, srv := newApp(t)
	gh := app.Installation(installationID)
	ctx := context.Background()
	if err := gh.React(ctx, repo, 77, "eyes"); err != nil {
		t.Fatalf("React: %v", err)
	}
	if err := gh.Comment(ctx, repo, 3, "hello"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := gh.Comment(ctx, repo, 3, strings.Repeat("x", MaxSummaryLength+1)); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if r := srv.Reactions(); len(r) != 1 || r[0].CommentID != 77 || r[0].Content != "eyes" {
		t.Fatalf("reactions = %+v", r)
	}
	c := srv.Comments()
	if len(c) != 2 || c[0].Number != 3 || c[0].Body != "hello" {
		t.Fatalf("comments = %+v", c)
	}
	if len(c[1].Body) > MaxSummaryLength || !strings.HasSuffix(c[1].Body, "_(truncated)_") {
		t.Fatalf("a long comment is %d bytes, want it truncated to %d", len(c[1].Body), MaxSummaryLength)
	}
}

func TestAccountsAndRepositories(t *testing.T) {
	tests := []struct {
		name         string
		owners       []string
		wantAccounts []ci.Account
		wantRepos    []string
	}{
		{
			name:         "every owner",
			wantAccounts: []ci.Account{{InstallationID: installationID, Login: "octo-org"}, {InstallationID: 8, Login: "stranger"}},
			wantRepos:    []string{"octo-org/octo-repo", "stranger/fork"},
		},
		{
			name:         "allowed owners only",
			owners:       []string{"Octo-Org"},
			wantAccounts: []ci.Account{{InstallationID: installationID, Login: "octo-org"}},
			wantRepos:    []string{"octo-org/octo-repo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, srv := newApp(t, WithOwners(tt.owners))
			srv.AddInstallation(installationID, "octo-org",
				githubtest.Repository{ID: 1, Owner: "octo-org", Name: "octo-repo", DefaultBranch: "main"},
				githubtest.Repository{ID: 2, Owner: "octo-org", Name: "old", DefaultBranch: "master", Archived: true},
				githubtest.Repository{ID: 3, Owner: "stranger", Name: "fork", DefaultBranch: "main"})
			srv.AddInstallation(8, "stranger")
			accounts, err := app.Accounts(context.Background())
			if err != nil || !slices.Equal(accounts, tt.wantAccounts) {
				t.Fatalf("Accounts = %+v, %v; want %+v", accounts, err, tt.wantAccounts)
			}
			repos, err := app.Installation(installationID).Repositories(context.Background())
			var names []string
			for _, r := range repos {
				names = append(names, r.FullName)
			}
			if err != nil || !slices.Equal(names, tt.wantRepos) {
				t.Fatalf("Repositories = %v, %v; want %v (archived repositories are left out)", names, err, tt.wantRepos)
			}
			if r := repos[0]; r.ID != 1 || r.Owner != "octo-org" || r.DefaultBranch != "main" || r.CloneURL != "https://github.com/octo-org/octo-repo.git" {
				t.Fatalf("repository = %+v", r)
			}
		})
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestMetricsCountFailedCheckRunCalls(t *testing.T) {
	m := metricstest.New(t)
	app, _ := newApp(t, WithMetrics(m.Metrics), WithTransport(failingTransport{}))
	gh := app.Installation(installationID)
	ctx := context.Background()
	calls := []struct {
		operation string
		call      func() error
	}{
		{"create", func() error { _, err := gh.OpenReport(ctx, repo, ci.Report{Name: "ci", Revision: "abc"}); return err }},
		{"update", func() error { return gh.UpdateReport(ctx, repo, 1, ci.Report{Status: ci.StatusInProgress}) }},
		{"get", func() error { _, err := gh.ReportTrigger(ctx, repo, 1); return err }},
		{"list", func() error { _, err := gh.SuiteReports(ctx, repo, 1); return err }},
	}
	for _, c := range calls {
		if err := c.call(); err == nil {
			t.Fatalf("%s: want an error", c.operation)
		}
		if got := m.Count(t, "octomaton.github.checkrun.errors", attributeOperation(c.operation)); got != 1 {
			t.Errorf("check-run errors for %s = %d, want 1", c.operation, got)
		}
	}
}

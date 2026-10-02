package upkeep

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/ci/citest"
)

func TestRefreshTokensOnce(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	repo := ci.Repository{ID: 1001, Owner: "octo-org", Name: "demo", FullName: "octo-org/demo"}
	token := &ci.TokenSettings{Workspace: "github-token", Permissions: map[string]string{"contents": "read"}}
	tests := []struct {
		name        string
		expires     time.Duration // from now; 0 stores no token
		token       bool
		all         bool // a token for every repository of the installation
		finished    bool
		wantRefresh bool
	}{
		{name: "a token about to expire", expires: 5 * time.Minute, token: true, wantRefresh: true},
		{name: "a token for every repository about to expire", expires: 5 * time.Minute, token: true, all: true, wantRefresh: true},
		{name: "an expired token", expires: -time.Minute, token: true, wantRefresh: true},
		{name: "a fresh token", expires: 50 * time.Minute, token: true},
		{name: "a finished run's token", expires: time.Minute, token: true, finished: true},
		{name: "a run whose start has not stored its token yet", token: true},
		{name: "a run without a token", expires: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, runner := citest.NewHost(clock), citest.NewRunner(clock)
			spec := ci.RunSpec{Trigger: ci.Trigger{InstallationID: 7, Repository: repo, Revision: "abc", Pipeline: "ci"}}
			if tt.token {
				settings := *token
				settings.AllRepositories = tt.all
				spec.Token = &settings
			}
			run, err := runner.Create(context.Background(), spec, 1)
			if err != nil {
				t.Fatal(err)
			}
			if tt.expires != 0 {
				_ = runner.SetToken(context.Background(), run, ci.Token{Value: "old", ExpiresAt: now.Add(tt.expires)})
			}
			if tt.finished {
				runner.Finish(run.ID, ci.Outcome{Conclusion: ci.Success}, now)
			}
			s := &Service{Host: host, Runner: runner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clock}
			n, err := s.RefreshTokensOnce(context.Background(), 20*time.Minute)
			if err != nil || (n == 1) != tt.wantRefresh {
				t.Fatalf("RefreshTokensOnce = %d, %v; want a refresh: %v", n, err, tt.wantRefresh)
			}
			if !tt.wantRefresh {
				return
			}
			if tok, _ := runner.Token(run.ID); tok.Value == "old" || !tok.ExpiresAt.Equal(now.Add(time.Hour)) {
				t.Fatalf("token = %+v, want a new one", tok)
			}
			want := []citest.TokenRequest{{InstallationID: 7, RepositoryID: 1001, Permissions: map[string]string{"contents": "read"}}}
			if tt.all {
				want[0].RepositoryID = 0
			}
			if got := host.TokenRequests(); !reflect.DeepEqual(got, want) {
				t.Fatalf("token requests = %+v, want %+v", got, want)
			}
		})
	}
}

func TestRefreshTokensOnceReportsFailures(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	host, runner := citest.NewHost(clock), citest.NewRunner(clock)
	spec := ci.RunSpec{Trigger: ci.Trigger{InstallationID: 7, Repository: ci.Repository{ID: 1, Name: "demo"}, Revision: "abc", Pipeline: "ci"}, Token: &ci.TokenSettings{}}
	run, _ := runner.Create(context.Background(), spec, 1)
	_ = runner.SetToken(context.Background(), run, ci.Token{Value: "old", ExpiresAt: now})
	host.Fail("RepositoryToken", errors.New("GitHub is down"))
	s := &Service{Host: host, Runner: runner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clock}
	if n, err := s.RefreshTokensOnce(context.Background(), 20*time.Minute); n != 0 || err == nil {
		t.Fatalf("RefreshTokensOnce = %d, %v; want the failure", n, err)
	}
}

func TestFreeResources(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	runner := citest.NewRunner(func() time.Time { return now })
	s := &Service{Runner: runner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // runs once, then stops
	s.FreeResources(ctx, time.Hour)
	if got := runner.FreedBefore(); len(got) != 1 || !got[0].Equal(now.Add(-time.Hour)) {
		t.Fatalf("FreeResources asked for %v, want runs finished before %v", got, now.Add(-time.Hour))
	}
}

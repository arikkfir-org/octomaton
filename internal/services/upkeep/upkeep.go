// Package upkeep keeps runs' surroundings in order: it refreshes the code-host tokens of live runs
// before they expire, and frees what finished runs no longer need. Only the leader does upkeep.
package upkeep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"octomaton.dev/internal/services/ci"
)

// Defaults.
const (
	DefaultTokenRefreshEvery  = 5 * time.Minute
	DefaultTokenRefreshWithin = 20 * time.Minute
	DefaultRetentionEvery     = time.Hour
)

// Service does upkeep.
type Service struct {
	Host   ci.CodeHost
	Runner ci.Runner
	Logger *slog.Logger
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// every runs fn now and then every interval until ctx ends.
func every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RefreshTokens keeps the tokens of live runs valid until ctx ends: tokens last an hour, runs may
// last longer, and a run reads its token where the runner stores it, so a new one reaches running
// steps within about a minute.
func (s *Service) RefreshTokens(ctx context.Context) {
	every(ctx, DefaultTokenRefreshEvery, func(ctx context.Context) {
		if n, err := s.RefreshTokensOnce(ctx, DefaultTokenRefreshWithin); err != nil {
			s.Logger.ErrorContext(ctx, "Token refresh incomplete", "refreshed", n, "error", err)
		} else if n > 0 {
			s.Logger.InfoContext(ctx, "Refreshed tokens", "count", n)
		}
	})
}

// RefreshTokensOnce mints a new token for every live run whose token expires within the window.
// Runs whose start has not stored a token yet are left to it.
func (s *Service) RefreshTokensOnce(ctx context.Context, within time.Duration) (int, error) {
	runs, err := s.Runner.List(ctx, ci.RunQuery{Live: true})
	if err != nil {
		return 0, err
	}
	var errs []error
	refreshed := 0
	for _, run := range runs {
		if run.Phase == ci.Finished || run.Token == nil {
			continue
		}
		expires, ok, err := s.Runner.TokenExpiry(ctx, run.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok || expires.Sub(s.now()) > within {
			continue
		}
		t := run.Trigger
		tok, err := s.Host.RepositoryToken(ctx, t.InstallationID, t.Repository.ID, run.Token.Permissions)
		if err == nil {
			err = s.Runner.SetToken(ctx, run, tok)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", run.ID, err))
			continue
		}
		refreshed++
		s.Logger.InfoContext(ctx, "Refreshed a run's token", "run", run.ID.String(), "expires", tok.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return refreshed, errors.Join(errs...)
}

// FreeResources frees, every hour until ctx ends, what runs that finished more than after ago still
// hold.
func (s *Service) FreeResources(ctx context.Context, after time.Duration) {
	every(ctx, DefaultRetentionEvery, func(ctx context.Context) {
		if n, err := s.Runner.FreeResources(ctx, s.now().Add(-after)); err != nil {
			s.Logger.ErrorContext(ctx, "Freeing finished runs' resources incomplete", "freed", n, "error", err)
		} else if n > 0 {
			s.Logger.InfoContext(ctx, "Freed finished runs' resources", "freed", n)
		}
	})
}

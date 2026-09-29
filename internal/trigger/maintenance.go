package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"octomaton.dev/internal/adapters/tekton"
)

// Maintenance defaults.
const (
	DefaultTokenRefreshEvery  = 5 * time.Minute
	DefaultTokenRefreshWithin = 20 * time.Minute
	DefaultRetentionEvery     = time.Hour
)

const (
	liveRuns     = tekton.LabelManagedBy + "=" + tekton.ManagedByValue + ",!" + tekton.LabelDone
	finishedRuns = tekton.LabelManagedBy + "=" + tekton.ManagedByValue + "," + tekton.LabelDone + ",!" + tekton.LabelPVCsFreed
)

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

// RefreshTokens keeps the GitHub tokens of unfinished runs alive until ctx
// ends: tokens last an hour, runs may last longer, and a pipeline reads the
// token from its mounted Secret, so re-minting it into the Secret reaches
// running steps within about a minute. It runs on the leader only.
func (s *Service) RefreshTokens(ctx context.Context) {
	every(ctx, DefaultTokenRefreshEvery, func(ctx context.Context) {
		if n, err := s.RefreshTokensOnce(ctx, DefaultTokenRefreshWithin); err != nil {
			s.Logger.Error("Token refresh incomplete", "refreshed", n, "error", err)
		} else if n > 0 {
			s.Logger.Info("Refreshed tokens", "count", n)
		}
	})
}

// RefreshTokensOnce re-mints every live run's token that expires within the given window.
func (s *Service) RefreshTokensOnce(ctx context.Context, within time.Duration) (int, error) {
	runs, err := s.Runs.List(ctx, "", liveRuns)
	if err != nil {
		return 0, err
	}
	var errs []error
	refreshed := 0
	for i := range runs {
		run := &runs[i]
		if tekton.IsDone(run) || run.GetAnnotations()[tekton.AnnotationToken] == "" {
			continue
		}
		ns, name := run.GetNamespace(), run.GetName()
		secret, err := s.Runs.TokenSecret(ctx, ns, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if secret == nil {
			continue
		}
		if expires, err := time.Parse(time.RFC3339, secret.Annotations[tekton.AnnotationExpiresAt]); err == nil && expires.Sub(s.now()) > within {
			continue
		}
		c, ok := ContextOf(run)
		if !ok {
			continue
		}
		var perms map[string]string
		if err := json.Unmarshal([]byte(secret.Annotations[tekton.AnnotationPermissions]), &perms); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: reading token permissions: %w", ns, name, err))
			continue
		}
		tok, err := s.GitHub.RepositoryToken(ctx, c.InstallationID, c.Repository.ID, perms)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", ns, name, err))
			continue
		}
		if err := s.Runs.UpdateTokenSecret(ctx, secret, tekton.Token{Value: tok.Value, ExpiresAt: tok.ExpiresAt, Permissions: perms}); err != nil {
			errs = append(errs, err)
			continue
		}
		refreshed++
		s.Logger.Info("Refreshed run token", "namespace", ns, "name", name, "expires", tok.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return refreshed, errors.Join(errs...)
}

// FreePVCs deletes, every hour until ctx ends, the PVCs owned by runs that
// finished more than after ago. Pods are kept (the Dashboard reads logs from
// them); Tekton's pruner deletes old runs. It runs on the leader only.
func (s *Service) FreePVCs(ctx context.Context, after time.Duration) {
	every(ctx, DefaultRetentionEvery, func(ctx context.Context) {
		if n, err := s.FreePVCsOnce(ctx, after); err != nil {
			s.Logger.Error("Freeing PVCs incomplete", "deleted", n, "error", err)
		} else if n > 0 {
			s.Logger.Info("Freed PVCs of finished runs", "deleted", n)
		}
	})
}

// FreePVCsOnce deletes the PVCs of runs that finished more than after ago and
// marks those runs so they are not examined again.
func (s *Service) FreePVCsOnce(ctx context.Context, after time.Duration) (int, error) {
	runs, err := s.Runs.List(ctx, "", finishedRuns)
	if err != nil {
		return 0, err
	}
	cutoff := s.now().Add(-after)
	owned := map[string]map[types.UID]string{} // namespace → run UID → run name
	for i := range runs {
		run := &runs[i]
		st, err := tekton.GetPipelineRunStatus(run)
		if err != nil || st.CompletionTime == nil || st.CompletionTime.After(cutoff) {
			continue
		}
		if owned[run.GetNamespace()] == nil {
			owned[run.GetNamespace()] = map[types.UID]string{}
		}
		owned[run.GetNamespace()][run.GetUID()] = run.GetName()
	}
	var errs []error
	deleted := 0
	for ns, byUID := range owned {
		pvcs, err := s.Runs.PVCs(ctx, ns)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		failed := false
		for _, pvc := range pvcs {
			for _, ref := range pvc.OwnerReferences {
				if ref.Kind != tekton.KindPipelineRun || byUID[ref.UID] == "" {
					continue
				}
				if err := s.Runs.DeletePVC(ctx, ns, pvc.Name); err != nil {
					errs = append(errs, err)
					failed = true
				} else {
					deleted++
				}
				break
			}
		}
		if failed {
			continue // try the namespace's runs again next time
		}
		for _, name := range byUID {
			if err := s.Runs.Label(ctx, ns, name, map[string]string{tekton.LabelPVCsFreed: "true"}, nil); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return deleted, errors.Join(errs...)
}

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arikkfir-org/switchboard/internal/reporter"
)

func TestDispatchSubcommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := dispatch([]string{"version"}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("version: %d %q", code, stdout.String())
	}
	stdout.Reset()
	if code := dispatch([]string{"help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "switchboard lint") {
		t.Fatalf("help: %d %q", code, stdout.String())
	}
	if code := dispatch([]string{"lint"}, &stdout, &stderr); code != 2 {
		t.Fatalf("lint without paths: exit %d, want 2", code)
	}

	dir := t.TempDir()
	cfg := "apiVersion: switchboard.kfirs.com/v1\npipelines:\n  - {name: ci, pipelineRun: run.yaml, on: {push: {branches: [main]}}, params: {revision: \"{{ .Revision }}\"}}\n"
	run := "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: {pipelineRef: {name: p}}\n"
	if err := os.WriteFile(filepath.Join(dir, ".switchboard.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.yaml"), []byte(run), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := dispatch([]string{"lint", "--render", dir}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "kind: PipelineRun") {
		t.Fatalf("lint --render: %d %q %q", code, stdout.String(), stderr.String())
	}
	if code := dispatch([]string{"serve", "--no-such-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad serve flag: exit %d, want 2", code)
	}
}

func TestReadiness(t *testing.T) {
	var leader atomic.Bool
	pingErr := errors.New("unreachable")
	var fail atomic.Bool
	rd := &readiness{
		ping: func(context.Context) error {
			if fail.Load() {
				return pingErr
			}
			return nil
		},
		leader:   &leader,
		reporter: &reporter.Reporter{},
		ttl:      0,
	}
	status := func() int {
		rec := httptest.NewRecorder()
		rd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	if code := status(); code != http.StatusOK {
		t.Fatalf("ready replica: %d", code)
	}
	leader.Store(true)
	if code := status(); code != http.StatusServiceUnavailable {
		t.Fatalf("a leader whose informer has not synced is not ready: %d", code)
	}
	leader.Store(false)
	fail.Store(true)
	if code := status(); code != http.StatusServiceUnavailable {
		t.Fatalf("an unreachable API server makes the replica unready: %d", code)
	}
	fail.Store(false)
	rd.shuttingDown.Store(true)
	if code := status(); code != http.StatusServiceUnavailable {
		t.Fatalf("a replica shutting down is not ready: %d", code)
	}
}

func TestReadinessCachesThePing(t *testing.T) {
	var calls atomic.Int32
	var leader atomic.Bool
	rd := &readiness{ping: func(context.Context) error { calls.Add(1); return nil }, leader: &leader, reporter: &reporter.Reporter{}, ttl: time.Hour}
	for range 3 {
		if err := rd.check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("pings = %d, want 1 within the TTL", calls.Load())
	}
}

func TestNewLogger(t *testing.T) {
	ctx := context.Background()
	for level, want := range map[string]slog.Level{"debug": slog.LevelDebug, "": slog.LevelInfo, "WARN": slog.LevelWarn, "error": slog.LevelError, "bogus": slog.LevelInfo} {
		l := newLogger(level)
		if !l.Enabled(ctx, want) || (want > slog.LevelDebug && l.Enabled(ctx, want-1)) {
			t.Errorf("newLogger(%q) does not log at %v exactly", level, want)
		}
	}
}

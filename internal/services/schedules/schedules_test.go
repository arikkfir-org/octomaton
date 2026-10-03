package schedules

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/ci/citest"
	"octomaton.dev/internal/services/runs"
	"octomaton.dev/internal/system/metrics"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
)

var repo = ci.Repository{ID: 1001, Owner: "octo-org", Name: "demo", FullName: "octo-org/demo", DefaultBranch: "main"}

const scheduleConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: nightly
    pipelineRun: .tekton/ci.yaml
    on:
      schedule: [{cron: "*/5 * * * *"}]
    params:
      slot: "{{ .Schedule.Slot }}"
`

type harness struct {
	host   *citest.Host
	runner *citest.Runner
	sc     *Scheduler
	mu     sync.Mutex
	now    time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{now: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	h.host = citest.NewHost(h.clock)
	h.runner = citest.NewRunner(h.clock)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &runs.Service{Host: h.host, Runner: h.runner, Logger: logger, Metrics: metrics.Discard(), Now: h.clock}
	h.sc = &Scheduler{Host: h.host, Runner: h.runner, Runs: svc, Logger: logger, Now: h.clock, Tick: 10 * time.Millisecond}
	h.host.AddAccount(7, "octo-org", repo, ci.Repository{ID: 3, Owner: "octo-org", Name: "no-branch", FullName: "octo-org/no-branch"})
	h.host.SetFile(repo, "main", ".octomaton.yaml", scheduleConfig)
	h.host.SetBranch(repo, "main", sha1)
	h.host.SetFile(repo, sha1, ".octomaton.yaml", scheduleConfig)
	h.host.SetFile(repo, sha1, ".tekton/ci.yaml", "kind: PipelineRun\n")
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) setNow(t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = t
}

func TestSchedulerFiresOncePerSlot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.sc.RefreshAll(ctx)
	if n := h.sc.ScheduledRepositories(); n != 1 {
		t.Fatalf("scheduled repositories = %d, want 1 (one without a default branch is skipped)", n)
	}

	h.setNow(time.Date(2026, 5, 1, 10, 2, 0, 0, time.UTC))
	h.sc.FireDue(ctx)
	runs := h.runner.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	tr := runs[0].Trigger
	if tr.Event != ci.EventSchedule || tr.Revision != sha1 || tr.Branch != "main" || tr.Ref != "refs/heads/main" || tr.DeliveryID != "schedule-1777629600" ||
		tr.Schedule == nil || *tr.Schedule != (ci.Schedule{Cron: "*/5 * * * *", Slot: "2026-05-01T10:00:00Z"}) {
		t.Fatalf("schedule trigger = %+v", tr)
	}
	if params := h.runner.Spec(runs[0].ID).Params; params["slot"] != "2026-05-01T10:00:00Z" {
		t.Fatalf("params = %v", params)
	}

	h.sc.FireDue(ctx)
	newLeader := &Scheduler{Host: h.host, Runner: h.runner, Runs: h.sc.Runs, Logger: h.sc.Logger, Now: h.clock}
	newLeader.RefreshAll(ctx)
	newLeader.FireDue(ctx)
	if len(h.runner.Runs()) != 1 {
		t.Fatalf("a slot fires once, even on a new leader")
	}

	h.setNow(time.Date(2026, 5, 1, 10, 6, 0, 0, time.UTC))
	h.sc.FireDue(ctx)
	if len(h.runner.Runs()) != 2 {
		t.Fatalf("the next slot fires a new run")
	}

	h.setNow(time.Date(2026, 5, 1, 11, 9, 0, 0, time.UTC))
	h.host.SetBranch(repo, "main", sha2)
	h.host.SetFile(repo, sha2, ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n")
	h.sc.FireDue(ctx)
	if len(h.runner.Runs()) != 2 {
		t.Fatalf("a schedule removed from the head's configuration does not fire")
	}
}

func TestAFiringScheduleReportsAnUnreadableConfiguration(t *testing.T) {
	tests := []struct {
		name       string
		config     string
		wantReport string
	}{
		{name: "on the pipeline's check", config: scheduleConfig, wantReport: "nightly"},
		// The pipeline's own runs report there too, so a re-run replaces the failure.
		{name: "on its display name", config: strings.Replace(scheduleConfig, "  - name: nightly\n", "  - name: nightly\n    displayName: Nightly build\n", 1), wantReport: "Nightly build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.host.SetFile(repo, "main", ".octomaton.yaml", tt.config)
			h.sc.RefreshAll(ctx)
			// GitHub goes down between reading the schedules and firing one.
			h.host.FailFile(repo, sha1, ".octomaton.yaml", errors.New("GET .octomaton.yaml: 504 Gateway Timeout"))
			h.setNow(time.Date(2026, 5, 1, 10, 2, 0, 0, time.UTC))
			h.sc.FireDue(ctx)
			if runs := h.runner.Runs(); len(runs) != 0 {
				t.Fatalf("runs = %+v, want none", runs)
			}
			reports := h.host.Reports()
			if len(reports) != 1 || reports[0].Name != tt.wantReport {
				t.Fatalf("reports = %+v, want one failure named %q", reports, tt.wantReport)
			}
			r := reports[0]
			// It stores the schedule's trigger, so re-running it fires the pipeline again.
			if r.Conclusion != ci.Failure || r.Title != "Could not read .octomaton.yaml" || r.Revision != sha1 ||
				r.Trigger == nil || r.Trigger.Pipeline != "nightly" || r.Trigger.Schedule == nil {
				t.Fatalf("report = %+v", r)
			}
		})
	}
}

func TestSchedulerNotify(t *testing.T) {
	h := newHarness(t)
	h.sc.Notify(7, repo) // not running: ignored
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.sc.Run(ctx); close(done) }()
	waitFor := func(what string, want int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); h.sc.ScheduledRepositories() != want; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting until %s", what)
			}
		}
	}
	waitFor("the schedules are read", 1)
	h.host.SetFile(repo, "main", ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n")
	h.sc.Notify(7, repo)
	waitFor("a notified repository is read again", 0)
	cancel()
	<-done
}

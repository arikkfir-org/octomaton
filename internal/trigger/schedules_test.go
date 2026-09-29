package trigger

import (
	"context"
	"strconv"
	"testing"
	"time"

	"octomaton.dev/internal/adapters/github/githubtest"
	"octomaton.dev/internal/checkrun"
	"octomaton.dev/internal/tekton"
)

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

func setupSchedules(h *harness) {
	h.gh.AddInstallation(installationID, owner,
		githubtest.Repository{ID: repoID, Owner: owner, Name: repoName, DefaultBranch: "main"},
		githubtest.Repository{ID: 2, Owner: owner, Name: "archived", DefaultBranch: "main", Archived: true},
	)
	h.gh.AddInstallation(8, "someone-else", githubtest.Repository{ID: 3, Owner: "someone-else", Name: "x", DefaultBranch: "main"})
	h.gh.AddFile(fullName, "main", ".octomaton.yaml", scheduleConfig)
	h.gh.SetBranch(fullName, "main", sha1)
	h.files(sha1, scheduleConfig, ciRun)
}

func TestSchedulerFiresOncePerSlot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setupSchedules(h)
	sc := &Scheduler{Service: h.svc}
	sc.RefreshAll(ctx)
	if len(sc.repos) != 1 {
		t.Fatalf("scheduled repositories = %d, want 1 (archived and foreign ones are skipped)", len(sc.repos))
	}

	h.setNow(time.Date(2026, 5, 1, 10, 2, 0, 0, time.UTC))
	sc.FireDue(ctx)
	runs := h.allRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	pr := &runs[0]
	c := contextAnnotation(t, pr)
	if c.Event != checkrun.EventSchedule || c.Revision != sha1 || c.Branch != "main" || c.Schedule == nil || c.Schedule.Slot != "2026-05-01T10:00:00Z" {
		t.Fatalf("schedule context = %+v", c)
	}
	slot := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC).Unix()
	if pr.GetLabels()[tekton.LabelSlot] != strconv.FormatInt(slot, 10) {
		t.Fatalf("slot label = %q", pr.GetLabels()[tekton.LabelSlot])
	}

	sc.FireDue(ctx)
	(&Scheduler{Service: h.svc, repos: sc.repos}).FireDue(ctx) // a new leader
	if len(h.allRuns()) != 1 {
		t.Fatalf("a slot fires once")
	}

	h.setNow(time.Date(2026, 5, 1, 10, 6, 0, 0, time.UTC))
	sc.FireDue(ctx)
	if len(h.allRuns()) != 2 {
		t.Fatalf("the next slot fires a new run")
	}

	h.setNow(time.Date(2026, 5, 1, 11, 20, 0, 0, time.UTC).Add(-11 * time.Minute)) // 11:09: slot 11:05 due
	h.gh.SetBranch(fullName, "main", sha2)
	h.gh.AddFile(fullName, sha2, ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n")
	sc.FireDue(ctx)
	if len(h.allRuns()) != 2 {
		t.Fatalf("a schedule removed from the head's configuration does not fire")
	}
}

func TestSchedulerNotify(t *testing.T) {
	h := newHarness(t)
	setupSchedules(h)
	sc := &Scheduler{Service: h.svc}
	sc.Notify(installationID, repo()) // not running: ignored
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sc.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sc.mu.Lock()
		n := len(sc.repos)
		sc.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.gh.AddFile(fullName, "main", ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n")
	sc.Notify(installationID, repo())
	for {
		sc.mu.Lock()
		n := len(sc.repos)
		sc.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a notified repository must be re-read")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

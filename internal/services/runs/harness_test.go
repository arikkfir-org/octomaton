package runs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/services/ci/citest"
	"octomaton.dev/internal/system/metrics/metricstest"
)

const (
	installationID = 7
	sha1           = "1111111111111111111111111111111111111111"
	sha2           = "2222222222222222222222222222222222222222"
	sha3           = "3333333333333333333333333333333333333333"
	baseSHA        = "9999999999999999999999999999999999999999"
)

var repo = ci.Repository{
	ID: 1001, Owner: "octo-org", Name: "demo", FullName: "octo-org/demo",
	CloneURL: "https://github.com/octo-org/demo.git", HTMLURL: "https://github.com/octo-org/demo", DefaultBranch: "main",
}

// ciConfig runs "ci" on pull requests into main, pushes to main and the merge queue.
const ciConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {branches: [main]}
      push: {branches: [main]}
      merge_group: {}
    params:
      repo-url: "{{ .Repository.CloneURL }}"
      revision: "{{ .Revision }}"
    githubToken: {workspace: github-token}
`

// ciRun stands for the PipelineRun file: the in-memory runner does not read it.
const ciRun = "kind: PipelineRun\n"

type harness struct {
	t      *testing.T
	host   *citest.Host
	runner *citest.Runner
	svc    *Service
	m      *metricstest.Metrics

	mu       sync.Mutex
	now      time.Time
	notified []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, now: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	h.host = citest.NewHost(h.clock)
	h.runner = citest.NewRunner(h.clock)
	h.runner.Tasks = func(spec ci.RunSpec) []string {
		if spec.TaskReports {
			return []string{"build", "test"}
		}
		return nil
	}
	h.m = metricstest.New(t)
	h.svc = &Service{Host: h.host, Runner: h.runner, Schedules: h, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: h.m.Metrics, Now: h.clock}
	h.host.SetPermission(repo, "maintainer", "write")
	h.host.SetPermission(repo, "reader", "read")
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// Notify records the repositories whose schedules are to be read again.
func (h *harness) Notify(_ int64, r ci.Repository) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notified = append(h.notified, r.FullName)
}

// files stores .octomaton.yaml and the ci pipeline's definition at a ref.
func (h *harness) files(ref, cfg, run string) {
	h.host.SetFile(repo, ref, ".octomaton.yaml", cfg)
	if run != "" {
		h.host.SetFile(repo, ref, ".tekton/ci.yaml", run)
	}
}

func prTrigger(sha string, number int, headRepo string) ci.Trigger {
	return ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventPullRequest, Action: "synchronize", DeliveryID: "delivery-" + sha[:4],
		InstallationID: installationID, Repository: repo, Revision: sha, Ref: fmt.Sprintf("refs/pull/%d/head", number), Branch: "feature",
		Sender: "alice",
		PullRequest: &ci.PullRequest{Number: number, HeadRef: "feature", HeadSHA: sha, BaseRef: "main", BaseSHA: baseSHA,
			HeadRepo: headRepo, Author: "alice", HTMLURL: fmt.Sprintf("https://github.com/%s/pull/%d", repo.FullName, number)},
	}
}

// branchPR is pull request #5 from a branch of the repository itself.
func branchPR(sha string) ci.Trigger { return prTrigger(sha, 5, repo.FullName) }

func pushTrigger(sha, branch string) ci.Trigger {
	return ci.Trigger{
		Version: ci.TriggerVersion, Event: ci.EventPush, DeliveryID: "push-" + sha[:4], InstallationID: installationID,
		Repository: repo, Revision: sha, Ref: "refs/heads/" + branch, Branch: branch, Sender: "alice",
		Push: &ci.Push{Before: baseSHA, After: sha},
	}
}

// run returns the run with a name.
func (h *harness) run(name string) ci.Run {
	h.t.Helper()
	run, ok := h.runner.Named(name)
	if !ok {
		var names []string
		for _, r := range h.runner.Runs() {
			names = append(names, r.ID.Name)
		}
		h.t.Fatalf("run %s not found (runs: %v)", name, names)
	}
	return run
}

// onlyReport returns the one report with a name.
func (h *harness) onlyReport(name string) citest.Report {
	h.t.Helper()
	reports := h.host.ReportsNamed(name)
	if len(reports) != 1 {
		h.t.Fatalf("reports named %q = %d, want 1 (all: %+v)", name, len(reports), h.host.Reports())
	}
	return reports[0]
}

// finish ends a run the way Tekton would.
func (h *harness) finish(name string, c ci.Conclusion) {
	h.runner.Finish(h.run(name).ID, ci.Outcome{Conclusion: c}, h.clock())
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("%q does not contain %q", got, w)
		}
	}
}

func (h *harness) evaluate(t ci.Trigger, opts EvalOptions) {
	h.svc.Evaluate(context.Background(), t, opts)
}

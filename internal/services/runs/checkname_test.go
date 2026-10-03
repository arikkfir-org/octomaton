package runs

import (
	"context"
	"strings"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// checkName is how the "ci" pipeline of these tests shows on the code host.
const checkName = "Continuous Integration"

// withDisplayName gives the "ci" pipeline of a configuration the display name checkName.
func withDisplayName(cfg string) string {
	return strings.Replace(cfg, "  - name: ci\n", "  - name: ci\n    displayName: "+checkName+"\n", 1)
}

// TestDisplayNameNamesTheReports: every report of a pipeline with a displayName carries it, while the
// pipeline's name keeps identifying its runs.
func TestDisplayNameNamesTheReports(t *testing.T) {
	taskChecks := strings.Replace(ciConfig, "githubToken: {workspace: github-token}", "taskChecks: true", 1)
	tests := []struct {
		name string
		act  func(t *testing.T, h *harness)
		want []string // the reports' names, oldest first
	}{
		{
			name: "a run and its task reports",
			act: func(t *testing.T, h *harness) {
				h.files(sha1, withDisplayName(taskChecks), ciRun)
				h.evaluate(branchPR(sha1), EvalOptions{})
				if tr := h.run("demo-ci-1111111-1").Trigger; tr.Pipeline != "ci" || tr.DisplayName != checkName {
					t.Fatalf("the run's trigger names pipeline %q and display name %q", tr.Pipeline, tr.DisplayName)
				}
			},
			want: []string{checkName, checkName + " / build", checkName + " / test"},
		},
		{
			name: "a pipeline skipped by its path filters",
			act: func(t *testing.T, h *harness) {
				h.files(sha1, withDisplayName(pathsConfig), ciRun)
				h.host.SetPullRequestFiles(repo, 5, ci.ChangedFiles{Files: []string{"README.md"}, Complete: true})
				h.evaluate(branchPR(sha1), EvalOptions{})
				if r := h.onlyReport(checkName); r.Conclusion != ci.Skipped || r.Trigger == nil || r.Trigger.Pipeline != "ci" {
					t.Fatalf("report = %+v, want skipped with its trigger", r)
				}
			},
			want: []string{checkName},
		},
		{
			name: "a refused pipeline",
			act: func(t *testing.T, h *harness) {
				h.files(sha1, withDisplayName(ciConfig), ciRun)
				h.runner.Fail("Create", &ci.Refusal{Title: "Refused", Reason: "the PipelineRun references Secret \"x\""})
				h.evaluate(branchPR(sha1), EvalOptions{})
				if r := h.onlyReport(checkName); r.Conclusion != ci.Failure {
					t.Fatalf("report = %+v, want a failure", r)
				}
			},
			want: []string{checkName},
		},
		{
			name: "a re-run",
			act: func(t *testing.T, h *harness) {
				h.files(sha1, withDisplayName(ciConfig), ciRun)
				h.evaluate(pushTrigger(sha1, "main"), EvalOptions{})
				h.finish("demo-ci-1111111-1", ci.Failure)
				h.svc.Rerun(context.Background(), rerun("maintainer", h.onlyReport(checkName)))
				if tr := h.run("demo-ci-1111111-2").Trigger; tr.Pipeline != "ci" || tr.DisplayName != checkName {
					t.Fatalf("the re-run's trigger names pipeline %q and display name %q", tr.Pipeline, tr.DisplayName)
				}
			},
			want: []string{checkName, checkName},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.act(t, h)
			var got []string
			for _, r := range h.host.Reports() {
				got = append(got, r.Name)
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("reports = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResumeFindsTheReportByItsDisplayName: a replica that stopped after GitHub had the report takes
// it over by the name it was opened with.
func TestResumeFindsTheReportByItsDisplayName(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, withDisplayName(ciConfig), ciRun)
	tr := pushTrigger(sha1, "main")
	tr.Pipeline, tr.DisplayName = "ci", checkName
	cfg, _ := h.svc.LoadConfig(ctx, tr)
	spec, refusal, _ := h.svc.prepare(ctx, h.host.Installation(installationID), tr, cfg.Pipeline("ci"))
	if refusal != nil {
		t.Fatal(refusal)
	}
	run, err := h.runner.Create(ctx, spec, 1)
	if err != nil {
		t.Fatal(err)
	}
	existing := h.host.AddReport(repo, ci.Report{Name: checkName, Revision: sha1, ExternalID: run.ID.String()})

	if err := h.svc.Resume(ctx, run); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := h.run(run.ID.Name); got.ReportID != existing {
		t.Fatalf("resumed run = %+v, want report %d", got, existing)
	}
	if n, old := len(h.host.ReportsNamed(checkName)), len(h.host.ReportsNamed("ci")); n != 1 || old != 0 {
		t.Fatalf("reports named %q: %d, named ci: %d; want the existing one taken over", checkName, n, old)
	}
}

// TestReportNameOfTriggersStoredBeforeDisplayNames: a trigger without a check name reports under its
// pipeline's name, as it did when it was stored.
func TestReportNameOfTriggersStoredBeforeDisplayNames(t *testing.T) {
	tests := []struct {
		trigger ci.Trigger
		want    string
	}{
		{trigger: ci.Trigger{Pipeline: "ci"}, want: "ci"},
		{trigger: ci.Trigger{Pipeline: "ci", DisplayName: checkName}, want: checkName},
		{trigger: ci.Trigger{}, want: ""},
	}
	for _, tt := range tests {
		if got := tt.trigger.ReportName(); got != tt.want {
			t.Errorf("ReportName(%+v) = %q, want %q", tt.trigger, got, tt.want)
		}
	}
}

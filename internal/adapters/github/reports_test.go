package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"octomaton.dev/internal/adapters/github/githubtest"
	"octomaton.dev/internal/services/ci"
)

func attributeOperation(op string) attribute.KeyValue { return attribute.String("operation", op) }

func TestReportLifecycle(t *testing.T) {
	app, srv := newApp(t)
	gh := app.Installation(installationID)
	ctx := context.Background()
	trigger := sampleTrigger()
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	completed := started.Add(time.Minute)

	id, err := gh.OpenReport(ctx, repo, ci.Report{
		Name: "ci", Revision: "abc", Status: ci.StatusQueued, Title: "Queued", Summary: "**PipelineRun:** `ns/run`",
		URL: "https://tekton.example/run", ExternalID: "ns/run", Trigger: &trigger,
	})
	if err != nil {
		t.Fatalf("OpenReport: %v", err)
	}
	steps := []struct {
		name   string
		update ci.Report
		check  func(githubtest.CheckRun) bool
	}{
		{
			name: "opened",
			check: func(cr githubtest.CheckRun) bool {
				got, found, err := DecodeMarker(cr.Text)
				return cr.Name == "ci" && cr.HeadSHA == "abc" && cr.Status == "queued" && cr.Title == "Queued" && cr.ExternalID == "ns/run" &&
					cr.DetailsURL == "https://tekton.example/run" && found && err == nil && reflect.DeepEqual(got, trigger)
			},
		},
		{
			name:   "started",
			update: ci.Report{Status: ci.StatusInProgress, Started: started, Title: "Running", Summary: "s", Trigger: &trigger},
			check: func(cr githubtest.CheckRun) bool {
				return cr.Status == "in_progress" && cr.StartedAt == "2026-01-02T03:04:05Z" && cr.Title == "Running" && cr.ExternalID == "ns/run"
			},
		},
		{
			name:   "a status alone leaves the output",
			update: ci.Report{Status: ci.StatusInProgress},
			check:  func(cr githubtest.CheckRun) bool { return cr.Title == "Running" && cr.Summary == "s" },
		},
		{
			name: "completed",
			update: ci.Report{Status: ci.StatusCompleted, Conclusion: ci.Failure, Completed: completed, Title: "Failed", Summary: strings.Repeat("s", MaxOutputLength),
				Text: strings.Repeat("l", MaxOutputLength), Trigger: &trigger},
			check: func(cr githubtest.CheckRun) bool {
				got, found, err := DecodeMarker(cr.Text)
				return cr.Status == "completed" && cr.Conclusion == "failure" && cr.CompletedAt == "2026-01-02T03:05:05Z" &&
					len(cr.Summary) <= MaxSummaryLength && strings.HasSuffix(cr.Summary, "_(truncated)_") &&
					len(cr.Text) <= MaxOutputLength && found && err == nil && got.Pipeline == "ci"
			},
		},
	}
	for _, step := range steps {
		if step.name != "opened" {
			if err := gh.UpdateReport(ctx, repo, id, step.update); err != nil {
				t.Fatalf("%s: UpdateReport: %v", step.name, err)
			}
		}
		if cr, _ := srv.CheckRun(int64(id)); !step.check(cr) {
			t.Fatalf("%s: check run = %+v", step.name, cr)
		}
	}
}

func TestOpenCompletedReportWithActions(t *testing.T) {
	app, srv := newApp(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id, err := app.Installation(installationID).OpenReport(context.Background(), repo, ci.Report{
		Name: "ci", Revision: "abc", Status: ci.StatusCompleted, Conclusion: ci.ActionRequired, Started: now, Completed: now,
		Title: "Approval required", Summary: "s", Actions: []ci.Action{{Label: "Approve and run", Description: "Run it", ID: ci.ApproveAction}},
	})
	if err != nil {
		t.Fatalf("OpenReport: %v", err)
	}
	cr, _ := srv.CheckRun(int64(id))
	if cr.Conclusion != "action_required" || cr.StartedAt != "2026-01-02T03:04:05Z" || cr.DetailsURL != "" || cr.Text != "" ||
		len(cr.Actions) != 1 || cr.Actions[0]["identifier"] != ci.ApproveAction || cr.Actions[0]["label"] != "Approve and run" {
		t.Fatalf("check run = %+v", cr)
	}
}

func TestFindReport(t *testing.T) {
	app, srv := newApp(t)
	srv.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci", HeadSHA: "abc", ExternalID: "ns/other"})
	want := srv.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci", HeadSHA: "abc", ExternalID: "ns/run"})
	srv.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "lint", HeadSHA: "abc", ExternalID: "ns/run"})
	gh := app.Installation(installationID)
	tests := []struct {
		externalID string
		want       ci.ReportID
	}{
		{"ns/run", ci.ReportID(want)},
		{"ns/none", 0},
	}
	for _, tt := range tests {
		if got, err := gh.FindReport(context.Background(), repo, "abc", "ci", tt.externalID); err != nil || got != tt.want {
			t.Errorf("FindReport(%s) = %d, %v; want %d", tt.externalID, got, err, tt.want)
		}
	}
}

func TestReportTriggerAndSuiteReports(t *testing.T) {
	app, srv := newApp(t)
	trigger := sampleTrigger()
	marker, _ := Marker(trigger)
	withTrigger := srv.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "ci", HeadSHA: "abc", Conclusion: "failure", Text: "logs\n\n" + marker})
	plain := srv.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "other", HeadSHA: "abc", Text: "no marker"})
	broken := srv.AddCheckRun(githubtest.CheckRun{Repo: fullName, Name: "broken", HeadSHA: "abc", Text: "<!-- octomaton:context:!!! -->"})
	gh := app.Installation(installationID)
	ctx := context.Background()

	tests := []struct {
		name    string
		id      int64
		want    *ci.Trigger
		wantErr bool
	}{
		{"a marker", withTrigger, &trigger, false},
		{"no marker", plain, nil, false},
		{"an unreadable marker", broken, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gh.ReportTrigger(ctx, repo, ci.ReportID(tt.id))
			if (err != nil) != tt.wantErr || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ReportTrigger = %+v, %v; want %+v (error %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}

	refs, err := gh.SuiteReports(ctx, repo, srv.SuiteID(fullName, "abc"))
	if err != nil || len(refs) != 3 {
		t.Fatalf("SuiteReports = %+v, %v", refs, err)
	}
	byName := map[string]ci.ReportRef{}
	for _, ref := range refs {
		byName[ref.Name] = ref
	}
	if ref := byName["ci"]; ref.ID != ci.ReportID(withTrigger) || ref.Revision != "abc" || ref.Conclusion != ci.Failure || !reflect.DeepEqual(ref.Trigger, &trigger) {
		t.Fatalf("report ci = %+v", ref)
	}
	if byName["other"].Trigger != nil || byName["broken"].Trigger != nil {
		t.Fatalf("reports without a readable marker must have no trigger: %+v", refs)
	}
}

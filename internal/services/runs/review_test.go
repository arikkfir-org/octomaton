package runs

import (
	"context"
	"reflect"
	"testing"

	"octomaton.dev/internal/services/ci"
)

// reviewConfig runs "review" when a review is requested from octo-reviewer on a pull request into main. Its definition
// lives in repository shared, and its runs may mount Secret api-key.
const reviewConfig = `
apiVersion: octomaton.dev/v1
pipelines:
  - name: review
    displayName: AI Review
    pipelineRun: {repository: shared, path: review/pipelinerun.yaml}
    on:
      review_request: {reviewers: [octo-reviewer], branches: [main]}
    params:
      number: "{{ .PullRequest.Number }}"
      reviewer: "{{ .ReviewRequest.Reviewer }}"
      revision: "{{ .Revision }}"
    secrets: [api-key]
`

var shared = ci.Repository{Owner: repo.Owner, Name: "shared", FullName: repo.Owner + "/shared"}

// reviewTrigger is a review requested from reviewer on pull request #5, as the code host decodes it.
func reviewTrigger(delivery, reviewer string) ci.Trigger {
	t := branchPR(sha1)
	t.Event, t.Action, t.DeliveryID, t.ConfigRef = ci.EventReviewRequest, "review_requested", delivery, "main"
	t.ReviewRequest = &ci.ReviewRequest{Reviewer: reviewer}
	return t
}

// setupReview serves the configuration from the default branch, and the definition from the default branch of
// repository shared. The pull request's own copy of the configuration, which runs nothing, must be ignored.
func setupReview(h *harness, cfg string) {
	h.host.SetFile(repo, "main", ".octomaton.yaml", cfg)
	h.host.SetFile(shared, "", "review/pipelinerun.yaml", ciRun)
	h.host.SetFile(repo, sha1, ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n")
	h.host.SetPullRequest(repo, openPR(func(*ci.PullRequestState) {}))
}

func (h *harness) requestReview(delivery, reviewer string) {
	h.svc.Handle(context.Background(), &ci.TriggerEvent{Trigger: reviewTrigger(delivery, reviewer)})
}

func TestReviewRequestRunsThePipeline(t *testing.T) {
	h := newHarness(t)
	setupReview(h, reviewConfig)
	h.requestReview("d-1", "octo-reviewer")

	runs := h.runner.Runs()
	if len(runs) != 1 || runs[0].ID.Name != "demo-review-1111111-1" || runs[0].Phase != ci.Released {
		t.Fatalf("runs = %+v, want demo-review-1111111-1 released", runs)
	}
	spec := h.runner.Spec(runs[0].ID)
	want := ci.RunSpec{
		Trigger: func() ci.Trigger {
			w := reviewTrigger("d-1", "octo-reviewer")
			w.Pipeline, w.DisplayName = "review", "AI Review"
			return w
		}(),
		Definition: []byte(ciRun), Path: "shared:review/pipelinerun.yaml",
		Params:  map[string]string{"number": "5", "reviewer": "octo-reviewer", "revision": sha1},
		Secrets: []string{"api-key"},
		// Like pull_request runs, requests on the same pull request supersede each other.
		Concurrency: ci.Concurrency{Group: "pr-5", Key: "review/pr-5", Policy: ci.Supersede},
	}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("spec = %+v\nwant %+v", spec, want)
	}
	report := h.onlyReport("AI Review")
	if report.Revision != sha1 || report.Trigger == nil || report.Trigger.ReviewRequest == nil {
		t.Fatalf("report = %+v", report)
	}
	mustContain(t, report.Summary, "**Trigger:** Review requested from @octo-reviewer on pull request #5")
}

func TestReviewRequestsThatRunNothing(t *testing.T) {
	tests := []struct {
		name     string
		cfg      string
		reviewer string
	}{
		{name: "a reviewer the pipeline doesn't list", cfg: reviewConfig, reviewer: "alice"},
		// Most review requests are for people: an invalid configuration is not reported on them.
		{name: "an invalid configuration", cfg: "apiVersion: octomaton.dev/v1\npipelines:\n  - {bogus: 1}\n", reviewer: "octo-reviewer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupReview(h, tt.cfg)
			h.requestReview("d-1", tt.reviewer)
			if runs, reports := h.runner.Runs(), h.host.Reports(); len(runs) != 0 || len(reports) != 0 {
				t.Fatalf("runs = %+v, reports = %+v; want none", runs, reports)
			}
		})
	}
}

func TestEachReviewRequestIsItsOwnRun(t *testing.T) {
	h := newHarness(t)
	setupReview(h, reviewConfig)
	h.requestReview("d-1", "octo-reviewer")
	// A redelivery finds its run.
	h.requestReview("d-1", "octo-reviewer")
	if runs := h.runner.Runs(); len(runs) != 1 {
		t.Fatalf("runs after a redelivery = %d, want 1", len(runs))
	}
	// Requesting the review again, at the same commit, reviews again: the next attempt supersedes the first.
	h.requestReview("d-2", "octo-reviewer")
	first, second := h.run("demo-review-1111111-1"), h.run("demo-review-1111111-2")
	if !first.CancelRequested || first.Cancellation.SupersededBy != second.ID.Name {
		t.Fatalf("the first review must be superseded by the second: %+v", first)
	}
	if second.CancelRequested || second.Phase != ci.Released || second.Trigger.DeliveryID != "d-2" {
		t.Fatalf("the second review must run: %+v", second)
	}
}

// newCommits moves pull request #5 to sha2 while a review is still requested from reviewers.
func (h *harness) newCommits(delivery string, reviewers ...string) {
	h.host.SetFile(repo, sha2, ".octomaton.yaml", "apiVersion: octomaton.dev/v1\npipelines: []\n")
	h.host.SetPullRequest(repo, openPR(func(pr *ci.PullRequestState) { pr.HeadSHA = sha2 }))
	t := branchPR(sha2)
	t.DeliveryID = delivery
	h.svc.Handle(context.Background(), &ci.TriggerEvent{Trigger: t, PendingReviewers: reviewers})
}

func TestNewCommitsRunPendingReviewRequests(t *testing.T) {
	tests := []struct {
		name       string
		reviewers  []string
		deliveries int
		want       []string // the review runs of the new head
	}{
		{name: "the pending request runs again at the new head", reviewers: []string{"Octo-Reviewer"}, deliveries: 1, want: []string{"demo-review-2222222-1"}},
		{name: "a redelivery finds its run", reviewers: []string{"Octo-Reviewer"}, deliveries: 2, want: []string{"demo-review-2222222-1"}},
		{name: "a request pending from someone else runs nothing", reviewers: []string{"alice"}, deliveries: 1},
		{name: "no pending request runs nothing", deliveries: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupReview(h, reviewConfig)
			h.requestReview("d-1", "octo-reviewer")
			for range tt.deliveries {
				h.newCommits("d-2", tt.reviewers...)
			}
			var got []string
			for _, r := range h.runner.Runs() {
				if r.Trigger.Revision == sha2 {
					got = append(got, r.ID.Name)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("runs of the new head = %v, want %v", got, tt.want)
			}
			first := h.run("demo-review-1111111-1")
			if len(tt.want) == 0 {
				if first.CancelRequested {
					t.Fatalf("nothing may stop the first review: %+v", first)
				}
				return
			}
			// The review of the older commit could no longer be posted: the new head's supersedes it.
			if !first.CancelRequested {
				t.Fatalf("the older commit's review must be superseded: %+v", first)
			}
			second := h.run(tt.want[0])
			tr := second.Trigger
			if second.Phase != ci.Released || tr.Event != ci.EventReviewRequest || tr.Action != "review_requested" || tr.ConfigRef != "main" ||
				tr.ReviewRequest == nil || tr.ReviewRequest.Reviewer != "Octo-Reviewer" || !tr.ReviewRequest.Pending {
				t.Fatalf("the new head's review = %+v", second)
			}
			if spec := h.runner.Spec(second.ID); spec.Path != "shared:review/pipelinerun.yaml" || spec.Params["revision"] != sha2 || spec.Params["reviewer"] != "Octo-Reviewer" {
				t.Fatalf("the new head's spec = %+v", spec)
			}
			for _, r := range h.host.Reports() {
				if r.Revision == sha2 {
					mustContain(t, r.Summary, "**Trigger:** Review still requested from @Octo-Reviewer on pull request #5, after new commits")
				}
			}
		})
	}
}

func TestReviewRequestDefinitions(t *testing.T) {
	tests := []struct {
		name      string
		cfg       string
		files     func(h *harness)
		wantPath  string
		wantTitle string
		wantText  string
	}{
		{
			name:     "in the repository itself, read at the default branch",
			cfg:      "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: review, pipelineRun: .tekton/review.yaml, on: {review_request: {reviewers: [octo-reviewer]}}}\n",
			files:    func(h *harness) { h.host.SetFile(repo, "main", ".tekton/review.yaml", ciRun) },
			wantPath: ".tekton/review.yaml",
		},
		{
			name:      "in the repository itself, only at the pull request's head",
			cfg:       "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: review, pipelineRun: .tekton/review.yaml, on: {review_request: {reviewers: [octo-reviewer]}}}\n",
			files:     func(h *harness) { h.host.SetFile(repo, sha1, ".tekton/review.yaml", ciRun) },
			wantTitle: "Could not start the pipeline",
			wantText:  "The pipelineRun file `.tekton/review.yaml` does not exist at `main`.",
		},
		{
			name:      "missing from the other repository",
			cfg:       "apiVersion: octomaton.dev/v1\npipelines:\n  - {name: review, pipelineRun: {repository: shared, path: review/gone.yaml}, on: {review_request: {reviewers: [octo-reviewer]}}}\n",
			files:     func(*harness) {},
			wantTitle: "Could not start the pipeline",
			wantText:  "The pipelineRun file `shared:review/gone.yaml` does not exist at the default branch of `octo-org/shared`.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			setupReview(h, tt.cfg)
			tt.files(h)
			h.requestReview("d-1", "octo-reviewer")
			if tt.wantTitle == "" {
				runs := h.runner.Runs()
				if len(runs) != 1 || h.runner.Spec(runs[0].ID).Path != tt.wantPath {
					t.Fatalf("runs = %+v, want one of %s", runs, tt.wantPath)
				}
				return
			}
			report := h.onlyReport("review")
			if len(h.runner.Runs()) != 0 || report.Conclusion != ci.Failure || report.Title != tt.wantTitle {
				t.Fatalf("runs = %+v, report = %+v", h.runner.Runs(), report)
			}
			mustContain(t, report.Summary, tt.wantText)
		})
	}
}

func TestRerunOfAReviewKeepsItsRequest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setupReview(h, reviewConfig)
	h.requestReview("d-1", "octo-reviewer")
	h.finish("demo-review-1111111-1", ci.Failure)

	h.svc.Rerun(ctx, rerun("maintainer", h.onlyReport("AI Review")))
	second := h.run("demo-review-1111111-2")
	if tr := second.Trigger; tr.Event != ci.EventReviewRequest || tr.ReviewRequest == nil || tr.ReviewRequest.Reviewer != "octo-reviewer" ||
		tr.ConfigRef != "main" || tr.RerunBy != "maintainer" {
		t.Fatalf("re-run trigger = %+v", tr)
	}
	if spec := h.runner.Spec(second.ID); spec.Path != "shared:review/pipelinerun.yaml" || len(spec.Secrets) != 1 || spec.Params["reviewer"] != "octo-reviewer" {
		t.Fatalf("re-run spec = %+v", spec)
	}
}

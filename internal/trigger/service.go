// Package trigger decides what a GitHub event means for a repository: it reads
// .octomatron.yaml, matches pipelines, applies trust and path rules, creates
// held PipelineRuns with their check runs and token Secrets, releases them per
// concurrency policy, and handles re-runs, approvals, comment commands,
// schedules, token refresh and PVC retention.
package trigger

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/arikkfir-org/octomatron/internal/checkrun"
	"github.com/arikkfir-org/octomatron/internal/githubapp"
	"github.com/arikkfir-org/octomatron/internal/metrics"
	"github.com/arikkfir-org/octomatron/internal/repoconfig"
	"github.com/arikkfir-org/octomatron/internal/tekton"
	"github.com/arikkfir-org/octomatron/internal/tmpl"
	"github.com/arikkfir-org/octomatron/internal/webhook"
	"github.com/google/go-github/v92/github"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ApproveAction is the identifier of the "Approve and run" check-run action.
const ApproveAction = "approve"

// Runs is the Kubernetes side of triggering; *tekton.Client implements it.
type Runs interface {
	NamespaceExists(ctx context.Context, namespace string) (bool, error)
	Create(ctx context.Context, pr *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error)
	List(ctx context.Context, namespace, selector string) ([]unstructured.Unstructured, error)
	SetStatus(ctx context.Context, namespace, name, status string) error
	Cancel(ctx context.Context, namespace, name string, annotations map[string]string) error
	Annotate(ctx context.Context, namespace, name string, annotations map[string]*string) error
	Label(ctx context.Context, namespace, name string, labels, annotations map[string]string) error
	CreateTokenSecret(ctx context.Context, run *unstructured.Unstructured, token tekton.Token, annotations map[string]string) error
	TokenSecret(ctx context.Context, namespace, run string) (*corev1.Secret, error)
	UpdateTokenSecret(ctx context.Context, secret *corev1.Secret, token tekton.Token) error
	PVCs(ctx context.Context, namespace string) ([]corev1.PersistentVolumeClaim, error)
	DeletePVC(ctx context.Context, namespace, name string) error
}

var _ Runs = (*tekton.Client)(nil)

// NamespaceResolver maps a repository to its namespace; *config.Namespaces implements it.
type NamespaceResolver interface {
	Resolve(repo tmpl.Repository) (string, error)
}

// ScheduleNotifier is told when a repository's default branch changes, so its
// schedules are re-read; *Scheduler implements it.
type ScheduleNotifier interface {
	Notify(installationID int64, repo checkrun.Repository)
}

// Service turns webhook events into check runs and PipelineRuns.
type Service struct {
	GitHub     githubapp.Provider
	Runs       Runs
	Namespaces NamespaceResolver
	// DashboardURL is the Tekton Dashboard base URL used for check-run links (optional).
	DashboardURL string
	// OwnerAllowed filters events by repository owner; nil allows every owner.
	OwnerAllowed func(owner string) bool
	// Schedules, when set, is notified of pushes to default branches.
	Schedules ScheduleNotifier
	Logger    *slog.Logger
	Metrics   *metrics.Metrics
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logFor(c checkrun.Context) *slog.Logger {
	l := s.Logger.With("repository", c.Repository.FullName, "event", c.Event, "sha", c.Revision)
	if c.DeliveryID != "" {
		l = l.With("delivery", c.DeliveryID)
	}
	if c.Pipeline != "" {
		l = l.With("pipeline", c.Pipeline)
	}
	return l
}

func (s *Service) ownerAllowed(owner string) bool {
	return s.OwnerAllowed == nil || s.OwnerAllowed(owner)
}

// Route implements webhook.Router: it validates and filters a parsed payload and
// returns the job that processes it, or an empty job and the reason it is ignored.
func (s *Service) Route(event, delivery string, payload any) (webhook.Job, string) {
	switch ev := payload.(type) {
	case *github.PushEvent:
		c, reason := pushContext(ev, delivery)
		if reason != "" {
			return webhook.Job{}, reason
		}
		if !s.ownerAllowed(c.Repository.Owner) {
			return webhook.Job{}, "repository owner is not allowed"
		}
		if s.Schedules != nil && c.Branch != "" && c.Branch == c.Repository.DefaultBranch {
			s.Schedules.Notify(c.InstallationID, c.Repository)
		}
		return s.evaluateJob(c, EvalOptions{ReportConfigErrors: true})

	case *github.PullRequestEvent:
		c, draft, reason := pullRequestContext(ev, delivery)
		if reason != "" {
			return webhook.Job{}, reason
		}
		// Configuration errors are reported only for the actions that normally run
		// pipelines, so that labeling or editing a pull request does not add failures.
		return s.evaluateJob(c, EvalOptions{Draft: draft, ReportConfigErrors: slices.Contains(repoconfig.DefaultPullRequestTypes, c.Action)})

	case *github.MergeGroupEvent:
		c, reason := mergeGroupContext(ev, delivery)
		if reason != "" {
			return webhook.Job{}, reason
		}
		switch ev.GetAction() {
		case "checks_requested":
			return s.evaluateJob(c, EvalOptions{ReportConfigErrors: true})
		case "destroyed":
			if !s.ownerAllowed(c.Repository.Owner) {
				return webhook.Job{}, "repository owner is not allowed"
			}
			why := ev.GetReason()
			return webhook.Job{Name: "merge_group destroyed " + c.Repository.FullName, Run: func(ctx context.Context) {
				s.CancelMergeGroup(ctx, c, why)
			}}, ""
		default:
			return webhook.Job{}, "unhandled merge_group action " + ev.GetAction()
		}

	case *github.IssueCommentEvent:
		req, reason := commentRequest(ev, delivery)
		if reason != "" {
			return webhook.Job{}, reason
		}
		if !s.ownerAllowed(req.Repository.Owner) {
			return webhook.Job{}, "repository owner is not allowed"
		}
		return webhook.Job{Name: "comment " + req.Repository.FullName, Run: func(ctx context.Context) {
			s.HandleComment(ctx, req)
		}}, ""

	case *github.CheckRunEvent:
		cr := ev.GetCheckRun()
		req := RerunRequest{
			InstallationID: ev.GetInstallation().GetID(),
			Repository:     repository(ev.GetRepo()),
			CheckRuns:      []*github.CheckRun{cr},
			Requester:      ev.GetSender().GetLogin(),
			DeliveryID:     delivery,
		}
		switch ev.GetAction() {
		case "rerequested":
		case "requested_action":
			if ev.GetRequestedAction() == nil || ev.GetRequestedAction().Identifier != ApproveAction {
				return webhook.Job{}, "unknown requested action"
			}
			req.Approve = true
		default:
			return webhook.Job{}, "unhandled check_run action " + ev.GetAction()
		}
		if cr.GetApp().GetID() != s.GitHub.AppID() {
			return webhook.Job{}, "check run belongs to another app"
		}
		return s.rerunJob(req)

	case *github.CheckSuiteEvent:
		if ev.GetAction() != "rerequested" {
			return webhook.Job{}, "unhandled check_suite action " + ev.GetAction()
		}
		suite := ev.GetCheckSuite()
		if suite.GetApp().GetID() != s.GitHub.AppID() {
			return webhook.Job{}, "check suite belongs to another app"
		}
		return s.rerunJob(RerunRequest{
			InstallationID: ev.GetInstallation().GetID(),
			Repository:     repository(ev.GetRepo()),
			SuiteID:        suite.GetID(),
			Requester:      ev.GetSender().GetLogin(),
			DeliveryID:     delivery,
		})
	}
	return webhook.Job{}, "unhandled event " + event
}

func (s *Service) evaluateJob(c checkrun.Context, opts EvalOptions) (webhook.Job, string) {
	if !s.ownerAllowed(c.Repository.Owner) {
		return webhook.Job{}, "repository owner is not allowed"
	}
	return webhook.Job{
		Name: c.Event + " " + c.Repository.FullName + "@" + checkrun.ShortSHA(c.Revision),
		Run:  func(ctx context.Context) { s.Evaluate(ctx, c, opts) },
	}, ""
}

func (s *Service) rerunJob(req RerunRequest) (webhook.Job, string) {
	switch {
	case req.InstallationID == 0:
		return webhook.Job{}, "no installation in payload"
	case req.Repository.FullName == "":
		return webhook.Job{}, "no repository in payload"
	case req.Requester == "":
		return webhook.Job{}, "no sender in payload"
	case !s.ownerAllowed(req.Repository.Owner):
		return webhook.Job{}, "repository owner is not allowed"
	}
	return webhook.Job{
		Name: "re-run " + req.Repository.FullName,
		Run:  func(ctx context.Context) { s.Rerun(ctx, req) },
	}, ""
}

// firstLine returns the first line of a comment, trimmed.
func firstLine(body string) string {
	line, _, _ := strings.Cut(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	return strings.TrimSpace(line)
}

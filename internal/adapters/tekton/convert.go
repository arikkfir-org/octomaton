package tekton

import (
	"encoding/json"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"octomaton.dev/internal/services/ci"
)

// runOf reads a PipelineRun as a ci.Run.
func runOf(pr *unstructured.Unstructured) ci.Run {
	l, a := pr.GetLabels(), pr.GetAnnotations()
	run := ci.Run{
		ID:               ci.RunID{Tenant: pr.GetNamespace(), Name: pr.GetName()},
		Trigger:          triggerOf(pr),
		Attempt:          int(parseInt(a[annotationAttempt])),
		Group:            l[labelConcurrencyGroup],
		Policy:           ci.Policy(a[annotationConcurrencyPolicy]),
		Token:            tokenSettingsOf(a[annotationToken]),
		TaskReports:      a[annotationTaskChecks] == "true",
		Tasks:            taskNames(pr),
		Phase:            phaseOf(pr),
		CancelRequested:  cancelRequested(pr),
		Deleting:         pr.GetDeletionTimestamp() != nil,
		Cancellation:     cancellationOf(a),
		Created:          pr.GetCreationTimestamp().Time.UTC(),
		ReportID:         ci.ReportID(parseInt(a[annotationCheckRunID])),
		Reported:         ci.Reported(a[annotationReported]),
		Progress:         a[annotationProgress],
		TaskReportIDs:    jsonMap[ci.ReportID](a[annotationTaskCheckIDs]),
		TaskReportStates: jsonMap[ci.Status](a[annotationTaskCheckStates]),
		WaitingFor:       a[annotationWaitingFor],
		Done:             l[labelDone] != "",
	}
	st, _ := getPipelineRunStatus(pr)
	if st.StartTime != nil {
		run.Started = st.StartTime.Time.UTC()
	}
	if st.CompletionTime != nil {
		run.Finished = st.CompletionTime.Time.UTC()
	}
	if run.Phase == ci.Finished {
		if conclusion, message, done := conclusionOf(st.Conditions); done {
			run.Outcome = ci.Outcome{Conclusion: conclusion, Message: message}
		}
	}
	return run
}

func phaseOf(pr *unstructured.Unstructured) ci.Phase {
	switch {
	case isDone(pr):
		return ci.Finished
	case isPending(pr):
		return ci.Held
	case started(pr):
		return ci.Running
	default:
		return ci.Released
	}
}

// triggerOf reads the trigger a run stores. When it is missing or unreadable, the run's bookkeeping
// still names its repository, installation, commit and pipeline; such a trigger has Version 0.
func triggerOf(pr *unstructured.Unstructured) ci.Trigger {
	l, a := pr.GetLabels(), pr.GetAnnotations()
	if raw := a[annotationContext]; raw != "" {
		var t ci.Trigger
		if err := json.Unmarshal([]byte(raw), &t); err == nil && t.Version == ci.TriggerVersion {
			return t
		}
	}
	owner, name, _ := strings.Cut(a[annotationRepository], "/")
	return ci.Trigger{
		Event:          l[labelEvent],
		InstallationID: parseInt(a[annotationInstallationID]),
		Repository:     ci.Repository{ID: parseInt(l[labelRepositoryID]), Owner: owner, Name: name, FullName: a[annotationRepository]},
		Revision:       a[annotationSHA],
		Pipeline:       l[labelPipeline],
	}
}

func cancellationOf(a map[string]string) ci.Cancellation {
	c := ci.Cancellation{Reason: a[annotationCancelReason]}
	by := a[annotationSupersededBy]
	if sha, ok := strings.CutPrefix(by, supersededByHead); ok {
		c.NewerCommit = sha
	} else {
		c.SupersededBy = by
	}
	return c
}

func tokenSettingsOf(raw string) *ci.TokenSettings {
	if raw == "" {
		return nil
	}
	var t ci.TokenSettings
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil
	}
	return &t
}

func jsonMap[V any](raw string) map[string]V {
	if raw == "" {
		return nil
	}
	m := map[string]V{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

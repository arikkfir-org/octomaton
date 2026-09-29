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
		Attempt:          int(parseInt(a[AnnotationAttempt])),
		Group:            l[LabelConcurrencyGroup],
		Policy:           ci.Policy(a[AnnotationConcurrencyPolicy]),
		Token:            tokenSettingsOf(a[AnnotationToken]),
		TaskReports:      a[AnnotationTaskChecks] == "true",
		Tasks:            TaskNames(pr),
		Phase:            phaseOf(pr),
		CancelRequested:  CancelRequested(pr),
		Deleting:         pr.GetDeletionTimestamp() != nil,
		Cancellation:     cancellationOf(a),
		Created:          pr.GetCreationTimestamp().Time.UTC(),
		ReportID:         ci.ReportID(parseInt(a[AnnotationCheckRunID])),
		Reported:         ci.Reported(a[AnnotationReported]),
		Progress:         a[AnnotationProgress],
		TaskReportIDs:    jsonMap[ci.ReportID](a[AnnotationTaskCheckIDs]),
		TaskReportStates: jsonMap[ci.Status](a[AnnotationTaskCheckStates]),
		WaitingFor:       a[AnnotationWaitingFor],
		Done:             l[LabelDone] != "",
	}
	st, _ := GetPipelineRunStatus(pr)
	if st.StartTime != nil {
		run.Started = st.StartTime.Time.UTC()
	}
	if st.CompletionTime != nil {
		run.Finished = st.CompletionTime.Time.UTC()
	}
	if run.Phase == ci.Finished {
		o := OutcomeOf(st.Conditions)
		run.Outcome = ci.Outcome{Conclusion: ci.Conclusion(o.Conclusion), Message: o.Message}
	}
	return run
}

func phaseOf(pr *unstructured.Unstructured) ci.Phase {
	switch {
	case IsDone(pr):
		return ci.Finished
	case IsPending(pr):
		return ci.Held
	case Started(pr):
		return ci.Running
	default:
		return ci.Released
	}
}

// triggerOf reads the trigger a run stores. When it is missing or unreadable, the run's bookkeeping
// still names its repository, installation, commit and pipeline; such a trigger has Version 0.
func triggerOf(pr *unstructured.Unstructured) ci.Trigger {
	l, a := pr.GetLabels(), pr.GetAnnotations()
	if raw := a[AnnotationContext]; raw != "" {
		var t ci.Trigger
		if err := json.Unmarshal([]byte(raw), &t); err == nil && t.Version == ci.TriggerVersion {
			return t
		}
	}
	owner, name, _ := strings.Cut(a[AnnotationRepository], "/")
	return ci.Trigger{
		Event:          l[LabelEvent],
		InstallationID: parseInt(a[AnnotationInstallationID]),
		Repository:     ci.Repository{ID: parseInt(l[LabelRepositoryID]), Owner: owner, Name: name, FullName: a[AnnotationRepository]},
		Revision:       a[AnnotationSHA],
		Pipeline:       l[LabelPipeline],
	}
}

func cancellationOf(a map[string]string) ci.Cancellation {
	c := ci.Cancellation{Reason: a[AnnotationCancelReason]}
	by := a[AnnotationSupersededBy]
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

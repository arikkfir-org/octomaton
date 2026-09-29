// Package checkrun holds what Octomaton stores on GitHub check runs: the trigger
// context (serialized into a hidden marker so a check can be re-run after its
// PipelineRun is gone), output size limits and Tekton Dashboard links.
package checkrun

import "octomaton.dev/internal/services/ci"

// ConfigCheckName is the name of the check run that reports problems which are not
// specific to one pipeline, such as an invalid .octomaton.yaml.
const ConfigCheckName = ci.ConfigReportName

// Event names Octomaton triggers pipelines for.
const (
	EventPush        = ci.EventPush
	EventPullRequest = ci.EventPullRequest
	EventMergeGroup  = ci.EventMergeGroup
	EventComment     = ci.EventComment
	EventSchedule    = ci.EventSchedule
)

// ContextVersion is the version of the serialized Context format.
const ContextVersion = ci.TriggerVersion

// The trigger context is ci.Trigger.
type (
	Repository  = ci.Repository
	Push        = ci.Push
	PullRequest = ci.PullRequest
	MergeGroup  = ci.MergeGroup
	Comment     = ci.Comment
	Schedule    = ci.Schedule
	Context     = ci.Trigger
)

// ShortSHA abbreviates a commit SHA for display.
func ShortSHA(sha string) string { return ci.ShortSHA(sha) }

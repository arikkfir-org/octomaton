// Package tekton loads, renders, creates and inspects Tekton PipelineRuns using
// unstructured objects and the dynamic client (the Tekton Go module is not imported).
package tekton

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Tekton resources Octomatron works with.
var (
	PipelineRuns = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}
	TaskRuns     = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}
)

const (
	// APIVersion is the only PipelineRun API version Octomatron accepts.
	APIVersion = "tekton.dev/v1"
	// KindPipelineRun is the only kind Octomatron accepts in a pipelineRun file.
	KindPipelineRun = "PipelineRun"
)

// Values of spec.status Octomatron writes: a pending run is held until the
// field is cleared; a cancelled one stops, running its finally tasks.
const (
	SpecStatusPending   = "PipelineRunPending"
	SpecStatusCancelled = "CancelledRunFinally"
)

// Labels Octomatron writes on the objects it creates (bookkeeping only; they
// are never read as configuration).
const (
	LabelManagedBy    = "app.kubernetes.io/managed-by"
	ManagedByValue    = "octomatron"
	LabelPipeline     = "octomatron.kfirs.com/pipeline"
	LabelEvent        = "octomatron.kfirs.com/event"
	LabelRepositoryID = "octomatron.kfirs.com/repository-id"
	LabelSHA          = "octomatron.kfirs.com/sha"
	// LabelConcurrencyGroup holds a hash of the repository and the concurrency group.
	LabelConcurrencyGroup = "octomatron.kfirs.com/concurrency-group"
	LabelComment          = "octomatron.kfirs.com/comment"
	LabelSlot             = "octomatron.kfirs.com/slot"
	// LabelDone marks a run Octomatron has finished reporting on.
	LabelDone = "octomatron.kfirs.com/done"
	// LabelPVCsFreed marks a finished run whose PVCs were deleted.
	LabelPVCsFreed = "octomatron.kfirs.com/pvcs-freed"
)

// Annotations Octomatron writes on the objects it creates.
const (
	AnnotationRepository        = "octomatron.kfirs.com/repository"
	AnnotationSHA               = "octomatron.kfirs.com/sha"
	AnnotationCheckRunID        = "octomatron.kfirs.com/check-run-id"
	AnnotationInstallationID    = "octomatron.kfirs.com/installation-id"
	AnnotationConcurrencyGroup  = "octomatron.kfirs.com/concurrency-group"
	AnnotationConcurrencyPolicy = "octomatron.kfirs.com/concurrency-policy"
	AnnotationDeliveryID        = "octomatron.kfirs.com/delivery-id"
	// AnnotationContext holds the serialized trigger context (JSON).
	AnnotationContext = "octomatron.kfirs.com/context"
	// AnnotationHead identifies the branch (or tag) the run is for.
	AnnotationHead    = "octomatron.kfirs.com/head"
	AnnotationAttempt = "octomatron.kfirs.com/attempt"
	// AnnotationToken holds the GitHub token settings (JSON) for resuming and refreshing.
	AnnotationToken      = "octomatron.kfirs.com/token"
	AnnotationTaskChecks = "octomatron.kfirs.com/task-checks"
	// AnnotationReported records what was last reported: queued, in_progress, concluded or completed.
	AnnotationReported        = "octomatron.kfirs.com/reported"
	AnnotationProgress        = "octomatron.kfirs.com/progress"
	AnnotationTaskCheckIDs    = "octomatron.kfirs.com/task-check-ids"
	AnnotationTaskCheckStates = "octomatron.kfirs.com/task-check-states"
	// AnnotationSupersededBy names the run (or "head:<sha>") that superseded this one.
	AnnotationSupersededBy = "octomatron.kfirs.com/superseded-by"
	// AnnotationWaitingFor names a same-commit rival a held run waits for.
	AnnotationWaitingFor = "octomatron.kfirs.com/waiting-for"
	// AnnotationCancelReason explains why Octomatron cancelled a run.
	AnnotationCancelReason = "octomatron.kfirs.com/cancel-reason"
	// AnnotationExpiresAt is the token expiry on a token Secret (RFC 3339).
	AnnotationExpiresAt = "octomatron.kfirs.com/expires-at"
	// AnnotationPermissions lists the token permissions on a token Secret (JSON).
	AnnotationPermissions = "octomatron.kfirs.com/permissions"
)

// Reported states.
const (
	ReportedQueued     = "queued"
	ReportedInProgress = "in_progress"
	ReportedConcluded  = "concluded"
	ReportedCompleted  = "completed"
)

const (
	maxNameLength = 63
	// TokenSecretSuffix is appended to the PipelineRun name to name its GitHub token Secret.
	TokenSecretSuffix = "-github-token"
	// TokenSecretKey is the Secret key holding the GitHub token.
	TokenSecretKey = "token"
)

// ParsePipelineRun parses a pipelineRun file: exactly one YAML (or JSON) document
// holding a tekton.dev/v1 PipelineRun. YAML is interpreted the way kubectl does.
func ParsePipelineRun(data []byte) (*unstructured.Unstructured, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs [][]byte
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading YAML: %w", err)
		}
		js, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("parsing YAML document %d: %w", len(docs)+1, err)
		}
		if t := bytes.TrimSpace(js); len(t) == 0 || bytes.Equal(t, []byte("null")) {
			continue
		}
		docs = append(docs, js)
	}
	if len(docs) != 1 {
		return nil, fmt.Errorf("expected exactly one YAML document, found %d", len(docs))
	}
	obj := map[string]any{}
	if err := utiljson.Unmarshal(docs[0], &obj); err != nil {
		return nil, fmt.Errorf("the document is not a YAML mapping: %w", err)
	}
	pr := &unstructured.Unstructured{Object: obj}
	if v := pr.GetAPIVersion(); v != APIVersion {
		return nil, fmt.Errorf("apiVersion must be %q (got %q)", APIVersion, v)
	}
	if k := pr.GetKind(); k != KindPipelineRun {
		return nil, fmt.Errorf("kind must be %q (got %q)", KindPipelineRun, k)
	}
	if spec, found, err := unstructured.NestedFieldNoCopy(obj, "spec"); err != nil || (found && !isMap(spec)) {
		return nil, errors.New("spec must be a mapping")
	}
	return pr, nil
}

func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// ShortSHA abbreviates a commit SHA to seven characters.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// RunName is the name of a pipeline's run: <repo>-<pipeline>-<sha7>-<attempt>,
// reduced to a DNS label of at most 63 characters.
func RunName(repository, pipeline, sha string, attempt int) string {
	suffix := "-" + ShortSHA(sha) + "-" + strconv.Itoa(attempt)
	base := dnsLabel(repository + "-" + pipeline)
	if limit := maxNameLength - len(suffix); len(base) > limit {
		base = strings.TrimRight(base[:limit], "-")
	}
	if base == "" {
		base = "run"
	}
	return base + suffix
}

// dnsLabel lower-cases s and replaces every character a DNS label may not hold with "-".
func dnsLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// GroupLabel is a concurrency group as a label value: a hash of the repository
// and the group key, so groups never span repositories.
func GroupLabel(repository, key string) string {
	sum := sha256.Sum256([]byte(repository + " " + key))
	return hex.EncodeToString(sum[:8])
}

// TokenSecretName is the name of the Secret holding a PipelineRun's GitHub token.
func TokenSecretName(pipelineRunName string) string {
	return pipelineRunName + TokenSecretSuffix
}

// RenderInput describes how Octomatron customizes a PipelineRun.
type RenderInput struct {
	Namespace string
	Name      string
	// Params set or override spec.params entries by name.
	Params map[string]string
	// Timeout, when positive, sets spec.timeouts.pipeline.
	Timeout time.Duration
	// TokenWorkspace, when set, binds the GitHub token Secret to this workspace.
	TokenWorkspace string
	Labels         map[string]string
	Annotations    map[string]string
	// Held creates the run with spec.status PipelineRunPending.
	Held bool
}

// Render returns a copy of src customized for creation: namespace and name set,
// generateName and server-populated fields cleared, params, timeout, token
// workspace, labels and annotations applied, and optionally held.
func Render(src *unstructured.Unstructured, in RenderInput) (*unstructured.Unstructured, error) {
	if ns := src.GetNamespace(); ns != "" && ns != in.Namespace {
		return nil, fmt.Errorf("the PipelineRun sets namespace %q, but this repository's runs must be in namespace %q", ns, in.Namespace)
	}
	pr := src.DeepCopy()
	pr.SetNamespace(in.Namespace)
	pr.SetName(in.Name)
	pr.SetGenerateName("")
	pr.SetResourceVersion("")
	pr.SetUID("")
	pr.SetGeneration(0)
	pr.SetManagedFields(nil)
	pr.SetOwnerReferences(nil)
	pr.SetFinalizers(nil)
	unstructured.RemoveNestedField(pr.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(pr.Object, "metadata", "deletionTimestamp")
	unstructured.RemoveNestedField(pr.Object, "status")

	pr.SetLabels(merge(pr.GetLabels(), in.Labels))
	pr.SetAnnotations(merge(pr.GetAnnotations(), in.Annotations))

	if len(in.Params) > 0 {
		if err := setParams(pr, in.Params); err != nil {
			return nil, err
		}
	}
	if in.Timeout > 0 {
		if err := unstructured.SetNestedField(pr.Object, in.Timeout.String(), "spec", "timeouts", "pipeline"); err != nil {
			return nil, fmt.Errorf("setting spec.timeouts.pipeline: %w", err)
		}
	}
	if in.TokenWorkspace != "" {
		if err := bindSecretWorkspace(pr, in.TokenWorkspace, TokenSecretName(in.Name)); err != nil {
			return nil, err
		}
	}
	if in.Held {
		if err := unstructured.SetNestedField(pr.Object, SpecStatusPending, "spec", "status"); err != nil {
			return nil, fmt.Errorf("setting spec.status: %w", err)
		}
	} else {
		unstructured.RemoveNestedField(pr.Object, "spec", "status")
	}
	return pr, nil
}

func merge(base, overrides map[string]string) map[string]string {
	if len(base) == 0 && len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// setParams sets the value of each named entry in spec.params, appending
// entries (sorted by name) that do not exist yet.
func setParams(pr *unstructured.Unstructured, params map[string]string) error {
	list, err := namedList(pr, "params")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if i := indexByName(list, name); i >= 0 {
			list[i].(map[string]any)["value"] = params[name]
		} else {
			list = append(list, map[string]any{"name": name, "value": params[name]})
		}
	}
	return unstructured.SetNestedSlice(pr.Object, list, "spec", "params")
}

// bindSecretWorkspace binds workspace to a Secret, replacing an existing binding of the same name.
func bindSecretWorkspace(pr *unstructured.Unstructured, workspace, secretName string) error {
	list, err := namedList(pr, "workspaces")
	if err != nil {
		return err
	}
	binding := map[string]any{"name": workspace, "secret": map[string]any{"secretName": secretName}}
	if i := indexByName(list, workspace); i >= 0 {
		list[i] = binding
	} else {
		list = append(list, binding)
	}
	return unstructured.SetNestedSlice(pr.Object, list, "spec", "workspaces")
}

func namedList(pr *unstructured.Unstructured, field string) ([]any, error) {
	list, _, err := unstructured.NestedSlice(pr.Object, "spec", field)
	if err != nil {
		return nil, fmt.Errorf("spec.%s must be a list: %w", field, err)
	}
	for i, item := range list {
		if _, ok := item.(map[string]any); !ok {
			return nil, fmt.Errorf("spec.%s[%d] must be a mapping", field, i)
		}
	}
	return list, nil
}

func indexByName(list []any, name string) int {
	for i, item := range list {
		if n, _ := item.(map[string]any)["name"].(string); n == name {
			return i
		}
	}
	return -1
}

// Secrets lists every Secret a PipelineRun names: in a volume or workspace
// (secretName), a projected source (secret.name) or an environment variable
// (secretKeyRef.name, secretRef.name).
func Secrets(obj map[string]any) []string {
	var out []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if name, ok := child.(string); ok && (k == "secretName" || (k == "name" && (key == "secret" || key == "secretKeyRef" || key == "secretRef"))) {
					out = append(out, name)
				}
				walk(k, child)
			}
		case []any:
			for _, child := range t {
				walk(key, child)
			}
		}
	}
	walk("", obj)
	sort.Strings(out)
	return out
}

// CheckSecrets refuses a PipelineRun that references any Secret other than
// allowed (its own token Secret, or none).
func CheckSecrets(pr *unstructured.Unstructured, allowed string) error {
	for _, name := range Secrets(pr.Object) {
		if name != allowed {
			if allowed == "" {
				return fmt.Errorf("the PipelineRun references Secret %q; runs may not mount Secrets (use githubToken for a GitHub token)", name)
			}
			return fmt.Errorf("the PipelineRun references Secret %q; runs may only mount their own token Secret %q", name, allowed)
		}
	}
	return nil
}

// TaskNames lists the tasks of a run's pipeline in order (finally tasks
// excluded): from status.pipelineSpec once Tekton has resolved it, else from
// the run's own spec.pipelineSpec.
func TaskNames(pr *unstructured.Unstructured) []string {
	tasks, _, _ := unstructured.NestedSlice(pr.Object, "status", "pipelineSpec", "tasks")
	if len(tasks) == 0 {
		tasks, _, _ = unstructured.NestedSlice(pr.Object, "spec", "pipelineSpec", "tasks")
	}
	var out []string
	for _, t := range tasks {
		if m, ok := t.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// TaskCheckName is the name of the check reporting one task of a pipeline.
func TaskCheckName(pipeline, task string) string {
	return pipeline + " / " + task
}

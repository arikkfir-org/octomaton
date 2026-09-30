// Package tekton is Octomaton's runner: Runner implements ci.Runner over Tekton PipelineRuns in the
// repositories' namespaces. It renders and creates PipelineRuns, keeps Octomaton's bookkeeping in
// their labels and annotations, maps their status to ci.Run and watches them for the leader;
// Renderer renders them without a cluster, for octomaton-lint. Tekton objects are unstructured and go
// through the dynamic client (the Tekton Go module is not imported).
package tekton

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
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

// Tekton resources Octomaton works with.
var (
	PipelineRuns = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}
	TaskRuns     = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}
)

const (
	// apiVersion is the only PipelineRun API version Octomaton accepts.
	apiVersion = "tekton.dev/v1"
	// kindPipelineRun is the only kind Octomaton accepts in a pipelineRun file.
	kindPipelineRun = "PipelineRun"
)

// Values of spec.status Octomaton writes: a pending run is held until the
// field is cleared; a cancelled one stops, running its finally tasks.
const (
	specStatusPending   = "PipelineRunPending"
	specStatusCancelled = "CancelledRunFinally"
)

// Labels Octomaton writes on the objects it creates (bookkeeping only; they
// are never read as configuration).
const (
	labelManagedBy    = "app.kubernetes.io/managed-by"
	managedByValue    = "octomaton"
	labelPipeline     = "octomaton.dev/pipeline"
	labelEvent        = "octomaton.dev/event"
	labelRepositoryID = "octomaton.dev/repository-id"
	labelSHA          = "octomaton.dev/sha"
	// labelConcurrencyGroup holds a hash of the repository and the concurrency group.
	labelConcurrencyGroup = "octomaton.dev/concurrency-group"
	labelComment          = "octomaton.dev/comment"
	labelSlot             = "octomaton.dev/slot"
	// labelDone marks a run Octomaton has finished reporting on.
	labelDone = "octomaton.dev/done"
	// labelPVCsFreed marks a finished run whose PVCs were deleted.
	labelPVCsFreed = "octomaton.dev/pvcs-freed"
)

// Annotations Octomaton writes on the objects it creates.
const (
	annotationRepository        = "octomaton.dev/repository"
	annotationSHA               = "octomaton.dev/sha"
	annotationCheckRunID        = "octomaton.dev/check-run-id"
	annotationInstallationID    = "octomaton.dev/installation-id"
	annotationConcurrencyGroup  = "octomaton.dev/concurrency-group"
	annotationConcurrencyPolicy = "octomaton.dev/concurrency-policy"
	annotationDeliveryID        = "octomaton.dev/delivery-id"
	// annotationContext holds the serialized trigger context (JSON).
	annotationContext = "octomaton.dev/context"
	// annotationHead identifies the branch (or tag) the run is for.
	annotationHead    = "octomaton.dev/head"
	annotationAttempt = "octomaton.dev/attempt"
	// annotationToken holds the GitHub token settings (JSON) for resuming and refreshing.
	annotationToken      = "octomaton.dev/token"
	annotationTaskChecks = "octomaton.dev/task-checks"
	// annotationReported records what was last reported: queued, in_progress, concluded or completed.
	annotationReported        = "octomaton.dev/reported"
	annotationProgress        = "octomaton.dev/progress"
	annotationTaskCheckIDs    = "octomaton.dev/task-check-ids"
	annotationTaskCheckStates = "octomaton.dev/task-check-states"
	// annotationSupersededBy names the run (or "head:<sha>") that superseded this one.
	annotationSupersededBy = "octomaton.dev/superseded-by"
	// annotationWaitingFor names a same-commit rival a held run waits for.
	annotationWaitingFor = "octomaton.dev/waiting-for"
	// annotationCancelReason explains why Octomaton cancelled a run.
	annotationCancelReason = "octomaton.dev/cancel-reason"
	// annotationExpiresAt is the token expiry on a token Secret (RFC 3339).
	annotationExpiresAt = "octomaton.dev/expires-at"
	// annotationPermissions lists the token permissions on a token Secret (JSON).
	annotationPermissions = "octomaton.dev/permissions"
)

const (
	maxNameLength = 63
	// tokenSecretSuffix is appended to the PipelineRun name to name its GitHub token Secret.
	tokenSecretSuffix = "-github-token"
	// tokenSecretKey is the Secret key holding the GitHub token.
	tokenSecretKey = "token"
)

// parsePipelineRun parses a pipelineRun file: exactly one YAML (or JSON) document
// holding a tekton.dev/v1 PipelineRun. YAML is interpreted the way kubectl does.
func parsePipelineRun(data []byte) (*unstructured.Unstructured, error) {
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
	if v := pr.GetAPIVersion(); v != apiVersion {
		return nil, fmt.Errorf("apiVersion must be %q (got %q)", apiVersion, v)
	}
	if k := pr.GetKind(); k != kindPipelineRun {
		return nil, fmt.Errorf("kind must be %q (got %q)", kindPipelineRun, k)
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

// shortSHA abbreviates a commit SHA to seven characters.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// runName is the name of a pipeline's run: <repo>-<pipeline>-<sha7>-<attempt>,
// reduced to a DNS label of at most 63 characters.
func runName(repository, pipeline, sha string, attempt int) string {
	suffix := "-" + shortSHA(sha) + "-" + strconv.Itoa(attempt)
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

// groupLabel is a concurrency group as a label value: a hash of the repository
// and the group key, so groups never span repositories.
func groupLabel(repository, key string) string {
	sum := sha256.Sum256([]byte(repository + " " + key))
	return hex.EncodeToString(sum[:8])
}

// tokenSecretName is the name of the Secret holding a PipelineRun's GitHub token.
func tokenSecretName(pipelineRunName string) string {
	return pipelineRunName + tokenSecretSuffix
}

// renderInput describes how Octomaton customizes a PipelineRun.
type renderInput struct {
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

// render returns a copy of src customized for creation: namespace and name set,
// generateName and server-populated fields cleared, params, timeout, token
// workspace, labels and annotations applied, and optionally held.
func render(src *unstructured.Unstructured, in renderInput) (*unstructured.Unstructured, error) {
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
		if err := bindSecretWorkspace(pr, in.TokenWorkspace, tokenSecretName(in.Name)); err != nil {
			return nil, err
		}
	}
	if in.Held {
		if err := unstructured.SetNestedField(pr.Object, specStatusPending, "spec", "status"); err != nil {
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

// secrets lists every Secret a PipelineRun names: in a volume or workspace
// (secretName), a projected source (secret.name) or an environment variable
// (secretKeyRef.name, secretRef.name).
func secrets(obj map[string]any) []string {
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

// checkSecrets refuses a PipelineRun that references any Secret other than its own token Secret
// (token, when it has one) and the Secrets its pipeline declares.
func checkSecrets(pr *unstructured.Unstructured, token string, declared []string) error {
	for _, name := range secrets(pr.Object) {
		if (token != "" && name == token) || slices.Contains(declared, name) {
			continue
		}
		allowed := "no Secrets"
		if names := slices.DeleteFunc(append([]string{token}, declared...), func(n string) bool { return n == "" }); len(names) > 0 {
			allowed = "only " + quoteAll(names)
		}
		return fmt.Errorf("the PipelineRun references Secret %q; its runs may mount %s (a GitHub token comes from githubToken, other Secrets must be listed in the pipeline's secrets)", name, allowed)
	}
	return nil
}

func quoteAll(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return strings.Join(quoted, ", ")
}

// remoteKeys are the fields through which Tekton fetches definitions from elsewhere.
var remoteKeys = map[string]bool{"pipelineRef": true, "taskRef": true, "resolver": true, "bundle": true}

// checkRemoteRefs refuses a PipelineRun that fetches any definition from elsewhere: a pipelineRef, a
// taskRef, a step's ref, a resolver or a bundle. Octomaton checks definitions (their Secrets
// included) only as the file holds them, so a run must be self-contained. Params and metadata may
// hold any keys, so they are not searched.
func checkRemoteRefs(obj map[string]any) error {
	var found string
	var walk func(path string, v any, inSteps bool)
	walk = func(path string, v any, inSteps bool) {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(t)) {
				if found != "" {
					return
				}
				switch {
				case k == "params" || k == "metadata":
					continue
				case remoteKeys[k] || (inSteps && k == "ref"):
					found = path + "." + k
					return
				}
				walk(path+"."+k, t[k], false)
			}
		case []any:
			for i, child := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), child, strings.HasSuffix(path, ".steps"))
			}
		}
	}
	walk("", obj, false)
	if found != "" {
		return fmt.Errorf("the PipelineRun fetches a definition through %s; Octomaton runs only self-contained PipelineRuns (spec.pipelineSpec, a taskSpec per task, steps without ref)", strings.TrimPrefix(found, "."))
	}
	return nil
}

// taskNames lists the tasks of a run's pipeline in order (finally tasks
// excluded): from status.pipelineSpec once Tekton has resolved it, else from
// the run's own spec.pipelineSpec.
func taskNames(pr *unstructured.Unstructured) []string {
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

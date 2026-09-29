package tekton

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/yaml"
)

func object(t *testing.T, doc string) map[string]any {
	t.Helper()
	js, err := yaml.YAMLToJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]any{}
	if err := utiljson.Unmarshal(js, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestParsePipelineRun(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{name: "minimal", doc: "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec:\n  pipelineRef: {name: p}\n"},
		{name: "leading separator and comments", doc: "# a comment\n---\napiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: {}\n---\n# trailing comment only\n"},
		{name: "JSON", doc: `{"apiVersion":"tekton.dev/v1","kind":"PipelineRun","spec":{}}`},
		{name: "two documents", doc: "apiVersion: tekton.dev/v1\nkind: PipelineRun\n---\napiVersion: tekton.dev/v1\nkind: PipelineRun\n", wantErr: "exactly one YAML document, found 2"},
		{name: "empty", doc: "", wantErr: "found 0"},
		{name: "v1beta1", doc: "apiVersion: tekton.dev/v1beta1\nkind: PipelineRun\n", wantErr: `apiVersion must be "tekton.dev/v1"`},
		{name: "wrong kind", doc: "apiVersion: tekton.dev/v1\nkind: TaskRun\n", wantErr: `kind must be "PipelineRun"`},
		{name: "not a mapping", doc: "- a\n- b\n", wantErr: "not a YAML mapping"},
		{name: "spec not a mapping", doc: "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: [1]\n", wantErr: "spec must be a mapping"},
		{name: "invalid YAML", doc: "apiVersion: [tekton\n", wantErr: "parsing YAML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr, err := parsePipelineRun([]byte(tt.doc))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || pr.GetKind() != "PipelineRun" {
				t.Fatalf("ParsePipelineRun = %v, %v", pr, err)
			}
		})
	}
}

const sourceRun = `
apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  generateName: ci-
  labels: {team: platform, app.kubernetes.io/managed-by: someone-else}
  annotations: {note: keep}
  resourceVersion: "12"
  uid: abc
  creationTimestamp: "2026-01-01T00:00:00Z"
spec:
  status: Cancelled
  params:
    - {name: revision, value: placeholder}
    - {name: flags, value: [a, b]}
  timeouts: {tasks: 30m}
  workspaces:
    - {name: source, emptyDir: {}}
    - {name: github-token, emptyDir: {}}
  pipelineSpec:
    tasks:
      - {name: build, taskRef: {name: build}}
      - {name: test, taskRef: {name: test}}
    finally:
      - {name: notify, taskRef: {name: notify}}
status:
  conditions: [{type: Succeeded, status: "True"}]
`

func TestRenderGolden(t *testing.T) {
	src, err := parsePipelineRun([]byte(sourceRun))
	if err != nil {
		t.Fatal(err)
	}
	got, err := render(src, renderInput{
		Namespace:      "ci-repo",
		Name:           "repo-ci-0123456-1",
		Params:         map[string]string{"revision": "0123456789", "repo-url": "https://example/repo.git", "a-first": "x"},
		Timeout:        90 * time.Minute,
		TokenWorkspace: "github-token",
		Labels:         map[string]string{labelManagedBy: managedByValue, labelPipeline: "ci"},
		Annotations:    map[string]string{annotationSHA: "0123456789"},
		Held:           true,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := object(t, `
apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  name: repo-ci-0123456-1
  namespace: ci-repo
  labels: {team: platform, app.kubernetes.io/managed-by: octomaton, octomaton.dev/pipeline: ci}
  annotations: {note: keep, octomaton.dev/sha: "0123456789"}
spec:
  status: PipelineRunPending
  params:
    - {name: revision, value: "0123456789"}
    - {name: flags, value: [a, b]}
    - {name: a-first, value: x}
    - {name: repo-url, value: "https://example/repo.git"}
  timeouts: {tasks: 30m, pipeline: 1h30m0s}
  workspaces:
    - {name: source, emptyDir: {}}
    - {name: github-token, secret: {secretName: repo-ci-0123456-1-github-token}}
  pipelineSpec:
    tasks:
      - {name: build, taskRef: {name: build}}
      - {name: test, taskRef: {name: test}}
    finally:
      - {name: notify, taskRef: {name: notify}}
`)
	if !reflect.DeepEqual(got.Object, want) {
		gotYAML, _ := yaml.Marshal(got.Object)
		wantYAML, _ := yaml.Marshal(want)
		t.Fatalf("rendered PipelineRun differs:\n--- got\n%s\n--- want\n%s", gotYAML, wantYAML)
	}
	if src.GetName() != "" || src.GetGenerateName() != "ci-" {
		t.Fatalf("Render must not modify its source")
	}
}

func TestRenderVariants(t *testing.T) {
	base := func(t *testing.T, doc string) *unstructured.Unstructured {
		t.Helper()
		pr, err := parsePipelineRun([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		return pr
	}
	t.Run("appends params and workspaces to an empty spec", func(t *testing.T) {
		pr, err := render(base(t, "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: {pipelineRef: {name: p}}\n"), renderInput{
			Namespace: "ns", Name: "run", Params: map[string]string{"b": "2", "a": "1"}, TokenWorkspace: "tok",
		})
		if err != nil {
			t.Fatal(err)
		}
		params, _, _ := unstructured.NestedSlice(pr.Object, "spec", "params")
		if len(params) != 2 || params[0].(map[string]any)["name"] != "a" {
			t.Fatalf("params = %v, want a then b", params)
		}
		ws, _, _ := unstructured.NestedSlice(pr.Object, "spec", "workspaces")
		if len(ws) != 1 || ws[0].(map[string]any)["secret"].(map[string]any)["secretName"] != "run-github-token" {
			t.Fatalf("workspaces = %v", ws)
		}
		if _, found, _ := unstructured.NestedString(pr.Object, "spec", "status"); found {
			t.Fatalf("a run rendered without Held must not set spec.status")
		}
		if _, found, _ := unstructured.NestedFieldNoCopy(pr.Object, "spec", "timeouts"); found {
			t.Fatalf("no timeout configured, none set")
		}
	})
	t.Run("same namespace is accepted", func(t *testing.T) {
		if _, err := render(base(t, "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata: {namespace: ns}\nspec: {}\n"), renderInput{Namespace: "ns", Name: "r"}); err != nil {
			t.Fatalf("Render: %v", err)
		}
	})
	t.Run("other namespace is refused", func(t *testing.T) {
		_, err := render(base(t, "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata: {namespace: other}\nspec: {}\n"), renderInput{Namespace: "ns", Name: "r"})
		if err == nil || !strings.Contains(err.Error(), `sets namespace "other"`) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("params must be a list", func(t *testing.T) {
		_, err := render(base(t, "apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec: {params: {a: b}}\n"), renderInput{Namespace: "ns", Name: "r", Params: map[string]string{"a": "c"}})
		if err == nil || !strings.Contains(err.Error(), "spec.params must be a list") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunName(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	labelRE := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	tests := []struct {
		repo, pipeline string
		attempt        int
		want           string
	}{
		{"octomaton", "ci", 1, "octomaton-ci-0123456-1"},
		{".github", "ci", 2, "github-ci-0123456-2"},
		{"My_Repo", "release", 10, "my-repo-release-0123456-10"},
		{strings.Repeat("r", 70), "ci", 3, strings.Repeat("r", 53) + "-0123456-3"},
		{strings.Repeat("r", 51) + "-x", "ci", 1, strings.Repeat("r", 51) + "-x-0123456-1"},
	}
	for _, tt := range tests {
		got := runName(tt.repo, tt.pipeline, sha, tt.attempt)
		if got != tt.want {
			t.Errorf("RunName(%q, %q, %d) = %q, want %q", tt.repo, tt.pipeline, tt.attempt, got, tt.want)
		}
		if len(got) > 63 || !labelRE.MatchString(got) {
			t.Errorf("RunName(%q) = %q is not a DNS label", tt.repo, got)
		}
	}
}

func TestGroupLabel(t *testing.T) {
	a := groupLabel("octo/repo", "pr-1")
	if a != groupLabel("octo/repo", "pr-1") || len(a) != 16 {
		t.Fatalf("GroupLabel is not a stable 16-character hash: %q", a)
	}
	if a == groupLabel("octo/other", "pr-1") || a == groupLabel("octo/repo", "pr-2") {
		t.Fatalf("groups must differ by repository and key")
	}
}

func TestSecretsGuard(t *testing.T) {
	doc := `
apiVersion: tekton.dev/v1
kind: PipelineRun
spec:
  workspaces:
    - {name: token, secret: {secretName: run-github-token}}
    - {name: creds, secret: {secretName: registry-creds}}
  taskRunTemplate:
    podTemplate:
      volumes:
        - name: projected
          projected: {sources: [{secret: {name: projected-secret}}]}
  pipelineSpec:
    tasks:
      - name: t
        taskSpec:
          steps:
            - name: s
              env: [{name: A, valueFrom: {secretKeyRef: {name: env-secret, key: k}}}]
              envFrom: [{secretRef: {name: envfrom-secret}}]
`
	pr, err := parsePipelineRun([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	got := secrets(pr.Object)
	want := []string{"env-secret", "envfrom-secret", "projected-secret", "registry-creds", "run-github-token"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Secrets = %v, want %v", got, want)
	}
	if err := checkSecrets(pr, "run-github-token"); err == nil || !strings.Contains(err.Error(), "env-secret") {
		t.Fatalf("CheckSecrets = %v, want a refusal", err)
	}
	clean, _ := parsePipelineRun([]byte("apiVersion: tekton.dev/v1\nkind: PipelineRun\nspec:\n  workspaces: [{name: t, secret: {secretName: run-github-token}}]\n"))
	if err := checkSecrets(clean, "run-github-token"); err != nil {
		t.Fatalf("the run's own token Secret is allowed: %v", err)
	}
	if err := checkSecrets(clean, ""); err == nil || !strings.Contains(err.Error(), "may not mount Secrets") {
		t.Fatalf("without a token, no Secret is allowed: %v", err)
	}
}

func TestTaskNames(t *testing.T) {
	pr, _ := parsePipelineRun([]byte(sourceRun))
	if got := taskNames(pr); !reflect.DeepEqual(got, []string{"build", "test"}) {
		t.Fatalf("TaskNames = %v", got)
	}
	_ = unstructured.SetNestedSlice(pr.Object, []any{map[string]any{"name": "resolved"}}, "status", "pipelineSpec", "tasks")
	if got := taskNames(pr); !reflect.DeepEqual(got, []string{"resolved"}) {
		t.Fatalf("TaskNames must prefer status.pipelineSpec: %v", got)
	}
}

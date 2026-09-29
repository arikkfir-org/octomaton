package tekton

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"octomaton.dev/internal/services/ci"
)

func newClient(objects ...runtime.Object) (*kubeClient, *kubefake.Clientset) {
	kube := kubefake.NewSimpleClientset(objects...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		PipelineRuns: "PipelineRunList",
		TaskRuns:     "TaskRunList",
	})
	return &kubeClient{Dynamic: dyn, Kube: kube}, kube
}

func newRun(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kindPipelineRun,
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name,
			"labels": map[string]any{labelPipeline: "ci", labelRepositoryID: "42", labelManagedBy: managedByValue}},
		"spec": map[string]any{"status": specStatusPending, "pipelineRef": map[string]any{"name": "p"}},
	}}
	return u
}

func TestNamespaceExists(t *testing.T) {
	c, kube := newClient(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ci-repo"}})
	ctx := context.Background()
	if ok, err := c.NamespaceExists(ctx, "ci-repo"); !ok || err != nil {
		t.Fatalf("existing namespace: %v, %v", ok, err)
	}
	if ok, err := c.NamespaceExists(ctx, "ci-missing"); ok || err != nil {
		t.Fatalf("missing namespace: %v, %v", ok, err)
	}
	kube.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "x", errors.New("no"))
	})
	if ok, err := c.NamespaceExists(ctx, "ci-anything"); !ok || err != nil {
		t.Fatalf("forbidden namespace read must be treated as existing: %v, %v", ok, err)
	}
	kube.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	if _, err := c.NamespaceExists(ctx, "ci-anything"); err == nil {
		t.Fatalf("other errors are returned")
	}
}

func TestRunLifecycle(t *testing.T) {
	c, _ := newClient()
	ctx := context.Background()
	created, err := c.Create(ctx, newRun("ns", "r1"))
	if err != nil || created.GetName() != "r1" {
		t.Fatalf("Create = %v, %v", created, err)
	}
	if _, err := c.Create(ctx, newRun("ns", "r1")); !errors.Is(err, errAlreadyExists) {
		t.Fatalf("second Create err = %v, want ErrAlreadyExists", err)
	}
	if err := c.SetStatus(ctx, "ns", "r1", ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, _ := c.Get(ctx, "ns", "r1")
	if isPending(got) {
		t.Fatalf("clearing spec.status must release the run: %v", got.Object["spec"])
	}
	value := "v"
	if err := c.Annotate(ctx, "ns", "r1", map[string]*string{"a": &value, "b": nil}); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if err := c.Cancel(ctx, "ns", "r1", map[string]string{annotationSupersededBy: "r2"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := c.Label(ctx, "ns", "r1", map[string]string{labelDone: "true"}, map[string]string{annotationReported: string(ci.ReportedCompleted)}); err != nil {
		t.Fatalf("Label: %v", err)
	}
	got, _ = c.Get(ctx, "ns", "r1")
	if !cancelRequested(got) || got.GetAnnotations()["a"] != "v" || got.GetAnnotations()[annotationSupersededBy] != "r2" ||
		got.GetLabels()[labelDone] != "true" || got.GetAnnotations()[annotationReported] != string(ci.ReportedCompleted) {
		t.Fatalf("run after patches = %v", got.Object)
	}
	if missing, err := c.Get(ctx, "ns", "nope"); missing != nil || err != nil {
		t.Fatalf("Get(missing) = %v, %v", missing, err)
	}
	if _, err := c.Create(ctx, newRun("ns", "r2")); err != nil {
		t.Fatal(err)
	}
	list, err := c.List(ctx, "ns", "!"+labelDone)
	if err != nil || len(list) != 1 || list[0].GetName() != "r2" {
		t.Fatalf("List(!done) = %v, %v", list, err)
	}
	all, err := c.List(ctx, "", labelManagedBy+"="+managedByValue)
	if err != nil || len(all) != 2 {
		t.Fatalf("cluster-wide List = %d, %v", len(all), err)
	}
}

func TestTokenSecret(t *testing.T) {
	c, kube := newClient()
	ctx := context.Background()
	run := newRun("ns", "r1")
	expires := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := c.CreateTokenSecret(ctx, run, token{Value: "t1", ExpiresAt: expires, Permissions: map[string]string{"contents": "read"}}, map[string]string{annotationRepository: "o/r"}); err != nil {
		t.Fatalf("CreateTokenSecret: %v", err)
	}
	s, err := c.TokenSecret(ctx, "ns", "r1")
	if err != nil || s == nil {
		t.Fatalf("TokenSecret = %v, %v", s, err)
	}
	if s.Name != "r1-github-token" || string(s.Data[tokenSecretKey]) != "t1" || s.Annotations[annotationExpiresAt] != "2026-01-01T01:00:00Z" ||
		s.Annotations[annotationPermissions] != `{"contents":"read"}` || s.Annotations[annotationRepository] != "o/r" {
		t.Fatalf("secret = %+v", s)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != kindPipelineRun || s.OwnerReferences[0].Name != "r1" || s.OwnerReferences[0].UID != "uid-r1" {
		t.Fatalf("owner references = %+v", s.OwnerReferences)
	}
	if err := c.UpdateTokenSecret(ctx, s, token{Value: "t2", ExpiresAt: expires.Add(time.Hour)}); err != nil {
		t.Fatalf("UpdateTokenSecret: %v", err)
	}
	s, _ = kube.CoreV1().Secrets("ns").Get(ctx, "r1-github-token", metav1.GetOptions{})
	if string(s.Data[tokenSecretKey]) != "t2" || s.Annotations[annotationExpiresAt] != "2026-01-01T02:00:00Z" {
		t.Fatalf("updated secret = %+v", s)
	}
	if missing, err := c.TokenSecret(ctx, "ns", "other"); missing != nil || err != nil {
		t.Fatalf("TokenSecret(missing) = %v, %v", missing, err)
	}
}

func TestPVCs(t *testing.T) {
	c, _ := newClient(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Namespace: "ns"}})
	ctx := context.Background()
	pvcs, err := c.PVCs(ctx, "ns")
	if err != nil || len(pvcs) != 1 {
		t.Fatalf("PVCs = %v, %v", pvcs, err)
	}
	if err := c.DeletePVC(ctx, "ns", "pvc-1"); err != nil {
		t.Fatalf("DeletePVC: %v", err)
	}
	if err := c.DeletePVC(ctx, "ns", "pvc-1"); err != nil {
		t.Fatalf("deleting a missing PVC is not an error: %v", err)
	}
}

func TestTaskRunsAndLogs(t *testing.T) {
	c, _ := newClient()
	ctx := context.Background()
	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": "TaskRun",
		"metadata": map[string]any{"name": "r1-build", "namespace": "ns", "labels": map[string]any{"tekton.dev/pipelineRun": "r1", "tekton.dev/pipelineTask": "build"}},
	}}
	if _, err := c.Dynamic.Resource(TaskRuns).Namespace("ns").Create(ctx, tr, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	list, err := c.TaskRuns(ctx, "ns", "r1")
	if err != nil || len(list) != 1 {
		t.Fatalf("TaskRuns = %v, %v", list, err)
	}
	logs, err := c.PodLogs(ctx, "ns", "pod", "step-build", 50, 1024)
	if err != nil || logs == "" {
		t.Fatalf("PodLogs = %q, %v", logs, err)
	}
}

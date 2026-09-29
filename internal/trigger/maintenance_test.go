package trigger

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"octomaton.dev/internal/tekton"
)

func TestRefreshTokens(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.files(sha2, ciConfig, ciRun)
	h.svc.Evaluate(ctx, pushCtx(sha1, "main"), EvalOptions{})
	h.svc.Evaluate(ctx, pushCtx(sha2, "main"), EvalOptions{})
	expiring, fresh := "demo-ci-1111111-1", "demo-ci-2222222-1"

	setExpiry := func(run string, at time.Time) {
		s, err := h.runs.TokenSecret(ctx, namespace, run)
		if err != nil || s == nil {
			t.Fatalf("token Secret of %s: %v", run, err)
		}
		s.Annotations[tekton.AnnotationExpiresAt] = at.UTC().Format(time.RFC3339)
		s.Data[tekton.TokenSecretKey] = []byte("old")
		if _, err := h.kube.CoreV1().Secrets(namespace).Update(ctx, s, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	setExpiry(expiring, h.clockNow().Add(5*time.Minute))
	setExpiry(fresh, h.clockNow().Add(50*time.Minute))

	n, err := h.svc.RefreshTokensOnce(ctx, 20*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("RefreshTokensOnce = %d, %v; want 1 refreshed", n, err)
	}
	s, _ := h.runs.TokenSecret(ctx, namespace, expiring)
	if string(s.Data[tekton.TokenSecretKey]) == "old" {
		t.Fatalf("the expiring token must be re-minted")
	}
	s, _ = h.runs.TokenSecret(ctx, namespace, fresh)
	if string(s.Data[tekton.TokenSecretKey]) != "old" {
		t.Fatalf("a token far from expiry must be left alone")
	}
	h.finish(expiring, "True", "Succeeded")
	_ = h.runs.Label(ctx, namespace, expiring, map[string]string{tekton.LabelDone: "true"}, nil)
	setExpiry(expiring, h.clockNow().Add(time.Minute))
	if n, _ := h.svc.RefreshTokensOnce(ctx, 20*time.Minute); n != 0 {
		t.Fatalf("finished runs' tokens are not refreshed")
	}
}

func TestFreePVCs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.files(sha1, ciConfig, ciRun)
	h.files(sha2, ciConfig, ciRun)
	h.svc.Evaluate(ctx, pushCtx(sha1, "main"), EvalOptions{})
	h.svc.Evaluate(ctx, pushCtx(sha2, "main"), EvalOptions{})
	old, recent := h.run("demo-ci-1111111-1"), h.run("demo-ci-2222222-1")

	finishAt := func(pr *unstructured.Unstructured, at time.Time) {
		_ = unstructured.SetNestedField(pr.Object, map[string]any{
			"conditions":     []any{map[string]any{"type": "Succeeded", "status": "True"}},
			"completionTime": at.UTC().Format(time.RFC3339),
		}, "status")
		labels := pr.GetLabels()
		labels[tekton.LabelDone] = "true"
		pr.SetLabels(labels)
		if _, err := h.dyn.Resource(tekton.PipelineRuns).Namespace(namespace).Update(ctx, pr, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	finishAt(old, h.clockNow().Add(-2*time.Hour))
	finishAt(recent, h.clockNow().Add(-10*time.Minute))
	pvc := func(name string, owner *unstructured.Unstructured) {
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		if owner != nil {
			claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: tekton.APIVersion, Kind: tekton.KindPipelineRun, Name: owner.GetName(), UID: owner.GetUID()}}
		}
		if _, err := h.kube.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pvc("pvc-old", old)
	pvc("pvc-recent", recent)
	pvc("pvc-unrelated", nil)

	n, err := h.svc.FreePVCsOnce(ctx, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("FreePVCsOnce = %d, %v; want 1", n, err)
	}
	claims, _ := h.kube.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	var names []string
	for _, c := range claims.Items {
		names = append(names, c.Name)
	}
	if len(names) != 2 || names[0] != "pvc-recent" || names[1] != "pvc-unrelated" {
		t.Fatalf("remaining PVCs = %v", names)
	}
	if h.run(old.GetName()).GetLabels()[tekton.LabelPVCsFreed] != "true" {
		t.Fatalf("a freed run must be marked so it is not examined again")
	}
	if n, _ := h.svc.FreePVCsOnce(ctx, time.Hour); n != 0 {
		t.Fatalf("nothing left to free")
	}
}

package tekton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// errAlreadyExists is returned by Create when the PipelineRun name is taken.
var errAlreadyExists = errors.New("already exists")

// token is a GitHub token exposed to a PipelineRun through a Secret.
type token struct {
	Value       string
	ExpiresAt   time.Time
	Permissions map[string]string
}

// kubeClient creates and manages PipelineRuns and their token Secrets.
type kubeClient struct {
	Dynamic dynamic.Interface
	Kube    kubernetes.Interface
}

func (c *kubeClient) runs(namespace string) dynamic.ResourceInterface {
	return c.Dynamic.Resource(PipelineRuns).Namespace(namespace)
}

// NamespaceExists reports whether a namespace exists. Reading namespaces is a
// cluster-scoped permission Octomaton may lack; then the namespace is assumed
// to exist and creating the run reports the problem instead.
func (c *kubeClient) NamespaceExists(ctx context.Context, namespace string) (bool, error) {
	_, err := c.Kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	switch {
	case err == nil, apierrors.IsForbidden(err):
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// ServiceAccount returns a ServiceAccount, or nil when it doesn't exist.
func (c *kubeClient) ServiceAccount(ctx context.Context, namespace, name string) (*corev1.ServiceAccount, error) {
	sa, err := c.Kube.CoreV1().ServiceAccounts(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sa, nil
}

// Create creates a PipelineRun; errAlreadyExists when the name is taken.
func (c *kubeClient) Create(ctx context.Context, pr *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	created, err := c.runs(pr.GetNamespace()).Create(ctx, pr, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, errAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("creating PipelineRun %s/%s: %w", pr.GetNamespace(), pr.GetName(), err)
	}
	return created, nil
}

// Get returns a PipelineRun, or nil when it does not exist.
func (c *kubeClient) Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	pr, err := c.runs(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading PipelineRun %s/%s: %w", namespace, name, err)
	}
	return pr, nil
}

// List returns the PipelineRuns in namespace matching a label selector.
func (c *kubeClient) List(ctx context.Context, namespace, selector string) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	opts := metav1.ListOptions{LabelSelector: selector, Limit: 250}
	for {
		list, err := c.runs(namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing PipelineRuns in %q (%s): %w", namespace, selector, err)
		}
		out = append(out, list.Items...)
		if list.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = list.GetContinue()
	}
}

// SetStatus sets spec.status ("" clears it, which releases a held run).
func (c *kubeClient) SetStatus(ctx context.Context, namespace, name, status string) error {
	var value any
	if status != "" {
		value = status
	}
	return c.patch(ctx, namespace, name, map[string]any{"spec": map[string]any{"status": value}})
}

// Cancel stops a run (spec.status CancelledRunFinally), recording why.
func (c *kubeClient) Cancel(ctx context.Context, namespace, name string, annotations map[string]string) error {
	patch := map[string]any{"spec": map[string]any{"status": specStatusCancelled}}
	if len(annotations) > 0 {
		patch["metadata"] = map[string]any{"annotations": annotations}
	}
	return c.patch(ctx, namespace, name, patch)
}

// Annotate merges annotations into a PipelineRun (a nil value removes one).
func (c *kubeClient) Annotate(ctx context.Context, namespace, name string, annotations map[string]*string) error {
	return c.patch(ctx, namespace, name, map[string]any{"metadata": map[string]any{"annotations": annotations}})
}

// Label merges labels and annotations into a PipelineRun.
func (c *kubeClient) Label(ctx context.Context, namespace, name string, labels map[string]string, annotations map[string]string) error {
	meta := map[string]any{"labels": labels}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	return c.patch(ctx, namespace, name, map[string]any{"metadata": meta})
}

func (c *kubeClient) patch(ctx context.Context, namespace, name string, patch map[string]any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if _, err := c.runs(namespace).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patching PipelineRun %s/%s: %w", namespace, name, err)
	}
	return nil
}

// TaskRuns lists the TaskRuns of a PipelineRun.
func (c *kubeClient) TaskRuns(ctx context.Context, namespace, pipelineRun string) ([]unstructured.Unstructured, error) {
	list, err := c.Dynamic.Resource(TaskRuns).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: "tekton.dev/pipelineRun=" + pipelineRun})
	if err != nil {
		return nil, fmt.Errorf("listing TaskRuns of %s/%s: %w", namespace, pipelineRun, err)
	}
	return list.Items, nil
}

// CreateTokenSecret creates the token Secret of a run, owned by the run so it
// is deleted with it.
func (c *kubeClient) CreateTokenSecret(ctx context.Context, run *unstructured.Unstructured, token token, annotations map[string]string) error {
	perms, _ := json.Marshal(token.Permissions)
	ann := map[string]string{
		annotationExpiresAt:   token.ExpiresAt.UTC().Format(time.RFC3339),
		annotationPermissions: string(perms),
	}
	for k, v := range annotations {
		ann[k] = v
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tokenSecretName(run.GetName()),
			Namespace:   run.GetNamespace(),
			Labels:      map[string]string{labelManagedBy: managedByValue, labelPipeline: run.GetLabels()[labelPipeline], labelRepositoryID: run.GetLabels()[labelRepositoryID]},
			Annotations: ann,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: apiVersion,
				Kind:       kindPipelineRun,
				Name:       run.GetName(),
				UID:        run.GetUID(),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{tokenSecretKey: []byte(token.Value)},
	}
	if _, err := c.Kube.CoreV1().Secrets(run.GetNamespace()).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating Secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return nil
}

// TokenSecret returns a run's token Secret, or nil when it does not exist.
func (c *kubeClient) TokenSecret(ctx context.Context, namespace, run string) (*corev1.Secret, error) {
	s, err := c.Kube.CoreV1().Secrets(namespace).Get(ctx, tokenSecretName(run), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Secret %s/%s: %w", namespace, tokenSecretName(run), err)
	}
	return s, nil
}

// UpdateTokenSecret replaces the token held by a token Secret.
func (c *kubeClient) UpdateTokenSecret(ctx context.Context, secret *corev1.Secret, token token) error {
	s := secret.DeepCopy()
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[annotationExpiresAt] = token.ExpiresAt.UTC().Format(time.RFC3339)
	s.Data = map[string][]byte{tokenSecretKey: []byte(token.Value)}
	if _, err := c.Kube.CoreV1().Secrets(s.Namespace).Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}

// PVCs lists the PersistentVolumeClaims in a namespace.
func (c *kubeClient) PVCs(ctx context.Context, namespace string) ([]corev1.PersistentVolumeClaim, error) {
	list, err := c.Kube.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing PVCs in %s: %w", namespace, err)
	}
	return list.Items, nil
}

// DeletePVC deletes a PersistentVolumeClaim (a missing one is not an error).
func (c *kubeClient) DeletePVC(ctx context.Context, namespace, name string) error {
	err := c.Kube.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting PVC %s/%s: %w", namespace, name, err)
	}
	return nil
}

// PodLogs returns the last lines of a container's log.
func (c *kubeClient) PodLogs(ctx context.Context, namespace, pod, container string, tailLines, limitBytes int64) (string, error) {
	raw, err := c.Kube.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  container,
		TailLines:  &tailLines,
		LimitBytes: &limitBytes,
	}).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

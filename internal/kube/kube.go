// Package kube connects to the Kubernetes API server.
package kube

import (
	"context"
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Clients are the typed and dynamic clients of one API server.
type Clients struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
}

// NewClients connects with the in-cluster configuration or, outside a cluster, with KUBECONFIG
// (else ~/.kube/config). Requests carry userAgent and are limited to 20 per second, in bursts of 50.
func NewClients(userAgent string) (*Clients, error) {
	cfg, err := restConfig()
	if err != nil {
		return nil, fmt.Errorf("configuring the Kubernetes client: %w", err)
	}
	cfg.UserAgent = userAgent
	cfg.QPS, cfg.Burst = 20, 50
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating the Kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating the dynamic Kubernetes client: %w", err)
	}
	return &Clients{Kube: kube, Dynamic: dyn}, nil
}

// Ping checks that the API server answers.
func (c *Clients) Ping(ctx context.Context) error {
	return c.Kube.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Error()
}

func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
}

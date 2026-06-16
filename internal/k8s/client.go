// Package k8s wraps client-go for FileRecoverySession and Secret access.
package k8s

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ClientOptions configures NewClient.
type ClientOptions struct {
	InCluster  bool
	Kubeconfig string
	Fake       bool
}

// Client bundles the corev1 and dynamic clientsets.
type Client struct {
	core   kubernetes.Interface
	dyn    dynamic.Interface
	isFake bool
}

// NewClient builds a Kubernetes clientset.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Fake {
		return &Client{
			core:   fake.NewSimpleClientset(),
			dyn:    dynfake.NewSimpleDynamicClient(NewScheme()),
			isFake: true,
		}, nil
	}

	var (
		cfg *rest.Config
		err error
	)
	if opts.InCluster {
		cfg, err = rest.InClusterConfig()
	} else {
		kubeconfig := opts.Kubeconfig
		if kubeconfig == "" {
			if home, _ := os.UserHomeDir(); home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("build kube config: %w", err)
	}

	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("core client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	return &Client{core: core, dyn: dyn}, nil
}

// Core returns the corev1 clientset.
func (c *Client) Core() kubernetes.Interface { return c.core }

// Dynamic returns the dynamic clientset.
func (c *Client) Dynamic() dynamic.Interface { return c.dyn }

// IsFake reports whether this client is backed by a fake clientset.
func (c *Client) IsFake() bool { return c.isFake }

// buildRESTFor returns a rest.Interface scoped to the apps.kio.kasten.io
// group. Used for subresources the dynamic client doesn't expose
// (like RestorePoint /details). Without the GroupVersion set, the
// generated REST client would 502 with "GroupVersion is required when
// initializing a RESTClient" on the first request.
func buildRESTFor(c *Client) (rest.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, err
		}
	}
	cfg.GroupVersion = &schema.GroupVersion{Group: "apps.kio.kasten.io", Version: "v1alpha1"}
	cfg.APIPath = "/apis"
	return rest.RESTClientFor(cfg)
}

// Package k8s wraps client-go for FileRecoverySession and Secret access.
package k8s

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

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

// Client bundles the corev1 and dynamic clientsets, plus a raw
// http.Client + rest.Config used by GetRestorePointDetails for
// the RP /details subresource. The dynamic client doesn't expose
// subresources, and the typed REST client requires a
// GroupVersion + NegotiatedSerializer that apps.kio.kasten.io
// doesn't define in our scheme. An http.Client with a manual
// bearer-token dance avoids both pitfalls.
type Client struct {
	core     kubernetes.Interface
	dyn      dynamic.Interface
	cfg      *rest.Config
	http     *http.Client
	isFake   bool
	hostBase string
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
	hostBase := cfg.Host
	if hostBase == "" {
		hostBase = "https://kubernetes.default.svc"
	}
	// http.DefaultClient does not honour cfg.TLSClientConfig, so
	// the apiserver's custom CA bundle would be ignored and
	// requests would fail with "x509: certificate signed by
	// unknown authority". Clone DefaultTransport and inject the
	// kube TLS config.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tlsCfg, terr := rest.TLSConfigFor(cfg); terr == nil {
		tr.TLSClientConfig = tlsCfg
	}
	httpClient := &http.Client{Transport: tr}
	return &Client{
		core: core, dyn: dyn, cfg: cfg,
		http:     httpClient,
		hostBase: hostBase,
	}, nil
}

// Core returns the corev1 clientset.
func (c *Client) Core() kubernetes.Interface { return c.core }

// Dynamic returns the dynamic clientset.
func (c *Client) Dynamic() dynamic.Interface { return c.dyn }

// IsFake reports whether this client is backed by a fake clientset.
func (c *Client) IsFake() bool { return c.isFake }

// doK8sRequest issues a raw HTTP request to the kube-apiserver using
// the in-cluster bearer token. Used for subresources the dynamic
// client doesn't expose (RestorePoint /details) and for any path
// where the typed REST client's GroupVersion/NegotiatedSerializer
// plumbing is heavier than the raw bytes we actually need.
//
// path must be the absolute path on the apiserver, e.g.
//   /apis/apps.kio.kasten.io/v1alpha1/namespaces/default/restorepoints/foo/details
func (c *Client) doK8sRequest(ctx context.Context, method, path string) ([]byte, error) {
	if c.isFake {
		return nil, fmt.Errorf("doK8sRequest: not supported in fake mode")
	}
	if c.cfg == nil || c.cfg.BearerToken == "" {
		return nil, fmt.Errorf("doK8sRequest: no in-cluster bearer token")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.hostBase+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("apiserver returned %d: %s", resp.StatusCode, snippet(body))
	}
	return body, nil
}

func snippet(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

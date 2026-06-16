package handlers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type fakeFRS struct {
	ns      string
	name    string
	host    string
	port    int
	hostKey string
}

func FakeFRSProvider(t *testing.T, hostPort, hostKey string) *fakeFRS {
	t.Helper()
	return &fakeFRS{
		ns: "default", name: "my-frs",
		host: hostPort, port: 2222, hostKey: hostKey,
	}
}

func (f *fakeFRS) ListActiveFRS(_ context.Context, _ []string) ([]k8s.FRSView, error) {
	return []k8s.FRSView{{
		Ref:         k8s.FRSRef{Namespace: f.ns, Name: f.name},
		ServiceName: f.ns, ServiceNS: f.ns, Port: int64(f.port),
		HostKeySig: f.hostKey, State: "Running", ExpiryTime: time.Now().Add(time.Hour),
		CreatedAt: time.Now(),
	}}, nil
}

func (f *fakeFRS) GetFRS(_ context.Context, ref k8s.FRSRef) (k8s.FRSView, error) {
	if ref.Namespace != f.ns || ref.Name != f.name {
		return k8s.FRSView{}, fmt.Errorf("not found")
	}
	return k8s.FRSView{
		Ref:         ref,
		ServiceName: ref.Namespace, ServiceNS: f.ns, Port: int64(f.port),
		HostKeySig: f.hostKey, State: "Running", ExpiryTime: time.Now().Add(time.Hour),
		CreatedAt: time.Now(),
	}, nil
}

// ListVMs / ListRestorePoints / GetRestorePointDetails /
// CreateFRS / DeleteFRS / WaitForReady are stubbed here so
// fakeFRS continues to satisfy the FRSProvider interface after
// the wizard extension (Task 8). The sessions/browse tests
// never exercise them.

func (f *fakeFRS) ListVMs(_ context.Context, _ []string) ([]k8s.VM, error) {
	return nil, nil
}
func (f *fakeFRS) ListVMNamespaces(_ context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeFRS) ListRestorePoints(_ context.Context, _, _ string) ([]k8s.RestorePoint, error) {
	return nil, nil
}
func (f *fakeFRS) GetRestorePointDetails(_ context.Context, _, _ string) ([]k8s.VolumeArtifact, error) {
	return nil, nil
}
func (f *fakeFRS) CreateFRS(_ context.Context, ns string, _ k8s.FRSpec) (*k8s.FRSView, error) {
	return &k8s.FRSView{Ref: k8s.FRSRef{Namespace: ns, Name: f.name}}, nil
}
func (f *fakeFRS) DeleteFRS(_ context.Context, _, _ string) error { return nil }
func (f *fakeFRS) CloneDataVolume(_ context.Context, _ string, _ k8s.DataVolumeSource) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("not stubbed")
}
func (f *fakeFRS) WaitDataVolumeSucceeded(_ context.Context, _, _ string, _ time.Duration) error { return nil }
func (f *fakeFRS) DeleteDataVolume(_ context.Context, _, _ string) error { return nil }
func (f *fakeFRS) WaitForReady(_ context.Context, ns, name string, _ time.Duration) (k8s.FRSView, error) {
	return k8s.FRSView{Ref: k8s.FRSRef{Namespace: ns, Name: name}, State: "Ready"}, nil
}

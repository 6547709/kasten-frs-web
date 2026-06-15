package sftpclient

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
)

// FakeFRSProvider returns a FRSProvider (used by handler tests).
// It exposes one active FRS pointing at the testserver.
//
// The returned provider implements only the methods used by handlers.
type fakeFRS struct {
	ns      string
	name    string
	host    string
	port    int
	hostKey string
}

// FakeFRSProvider builds a fake provider for handler tests. Not used in production.
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

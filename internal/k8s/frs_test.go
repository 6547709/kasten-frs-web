package k8s

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(ClientOptions{Fake: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func makeFRS(name, ns string, active bool, expired bool) *unstructured.Unstructured {
	state := "Running"
	if !active {
		state = "Terminated"
	}
	expiry := time.Now().Add(time.Hour).Format(time.RFC3339)
	if expired {
		expiry = time.Now().Add(-time.Hour).Format(time.RFC3339)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "datamover.kio.kasten.io/v1alpha1",
		"kind":       "FileRecoverySession",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         ns,
			"creationTimestamp": time.Now().Format(time.RFC3339),
		},
		"status": map[string]any{
			"state":      state,
			"expiryTime": expiry,
			"transports": map[string]any{
				"sftp": map[string]any{
					"serviceName":      "frs-xxx",
					"serviceNamespace": "kasten-io",
					"portNumber":       int64(2222),
					"endpoints":        []any{"frs-xxx.kasten-io.svc.cluster.local"},
					"hostKeySignature": "[frs-xxx.kasten-io.svc.cluster.local.]:2222 ssh-ed25519 AAAA",
				},
			},
		},
	}}
}

func TestListActiveFRS_FiltersExpiredAndInactive(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "datamover.kio.kasten.io", Version: "v1alpha1", Resource: "filerecoverysessions"}

	// seed
	for _, u := range []*unstructured.Unstructured{
		makeFRS("active-1", "ns-a", true, false),
		makeFRS("active-2", "ns-b", true, false),
		makeFRS("inactive", "ns-a", false, false),
		makeFRS("expired", "ns-b", true, true),
	} {
		_, err := c.Dynamic().Resource(gvr).Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := c.ListActiveFRS(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 active FRS, got %d: %+v", len(got), got)
	}
}

func TestListActiveFRS_NamespaceWhitelist(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "datamover.kio.kasten.io", Version: "v1alpha1", Resource: "filerecoverysessions"}
	for _, u := range []*unstructured.Unstructured{
		makeFRS("a", "ns-a", true, false),
		makeFRS("b", "ns-b", true, false),
	} {
		_, _ = c.Dynamic().Resource(gvr).Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{})
	}
	got, err := c.ListActiveFRS(ctx, []string{"ns-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ref.Namespace != "ns-a" {
		t.Fatalf("got = %+v", got)
	}
}
package k8s

import (
	"context"
	"strings"
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

// makeFRSForCreate builds a connectable FRS object with explicit transport fields.
func makeFRSForCreate(ns, name, state, svcName, svcNS string, port int64, hostKeySig string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "datamover.kio.kasten.io", Version: "v1alpha1", Kind: "FileRecoverySession",
	})
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, state, "status", "state")
	_ = unstructured.SetNestedField(u.Object, svcName, "status", "transports", "sftp", "serviceName")
	_ = unstructured.SetNestedField(u.Object, svcNS, "status", "transports", "sftp", "serviceNamespace")
	_ = unstructured.SetNestedField(u.Object, port, "status", "transports", "sftp", "portNumber")
	_ = unstructured.SetNestedField(u.Object, hostKeySig, "status", "transports", "sftp", "hostKeySignature")
	return u
}

func newTestFRSClient(t *testing.T, seed ...*unstructured.Unstructured) *Client {
	t.Helper()
	c, err := NewClient(ClientOptions{Fake: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, u := range seed {
		if _, err := c.Dynamic().Resource(FRSGroupVersionResource).Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestCreateFRS_SpecShape(t *testing.T) {
	c := newTestFRSClient(t)
	spec := FRSpec{
		RestorePointName: "rp1",
		PVCNames:         []string{"data-pvc", "logs-pvc"},
		SSHUserPublicKey: "ssh-ed25519 AAAA... user@host",
	}
	view, err := c.CreateFRS(context.Background(), "default", spec)
	if err != nil {
		t.Fatal(err)
	}
	if view == nil {
		t.Fatal("view nil")
	}
	if view.Ref.Namespace != "default" {
		t.Errorf("ns = %s", view.Ref.Namespace)
	}
	// Resolve the created object by listing — the fake dynamic client does not
	// assign names from generateName, so we look it up via List instead of Get.
	list, err := c.Dynamic().Resource(FRSGroupVersionResource).Namespace("default").List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 FRS in namespace, got %d", len(list.Items))
	}
	got := list.Items[0]
	if !strings.HasPrefix(got.GetGenerateName(), "frs-wizard-") {
		t.Errorf("generateName = %q, want frs-wizard- prefix", got.GetGenerateName())
	}
	vols, _, _ := unstructured.NestedSlice(got.Object, "spec", "volumes")
	if len(vols) != 2 {
		t.Errorf("got %d volumes, want 2", len(vols))
	}
	key, _, _ := unstructured.NestedString(got.Object, "spec", "transports", "sftp", "userPublicKey")
	if key != "ssh-ed25519 AAAA... user@host" {
		t.Errorf("userPublicKey = %q", key)
	}
}

func TestCreateFRS_GenerateName(t *testing.T) {
	c := newTestFRSClient(t)
	spec := FRSpec{RestorePointName: "rp1", PVCNames: []string{"p"}, SSHUserPublicKey: "k"}
	_, err := c.CreateFRS(context.Background(), "default", spec)
	if err != nil {
		t.Fatal(err)
	}
	// Verify the generateName field is set on the created object. The real
	// apiserver resolves generateName to a unique name; the fake client does
	// not, but the generateName field is preserved on the unstructured object.
	list, err := c.Dynamic().Resource(FRSGroupVersionResource).Namespace("default").List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 FRS, got %d", len(list.Items))
	}
	gn := list.Items[0].GetGenerateName()
	if !strings.HasPrefix(gn, "frs-wizard-") {
		t.Errorf("generateName = %q, want frs-wizard- prefix", gn)
	}
}

func TestDeleteFRS_Idempotent(t *testing.T) {
	c := newTestFRSClient(t)
	if err := c.DeleteFRS(context.Background(), "default", "nope"); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestWaitForReady_StateMachine(t *testing.T) {
	f := makeFRSForCreate("default", "frs1", "Ready", "svc1", "kasten-io", 2222, "sig")
	c := newTestFRSClient(t, f)
	view, err := c.WaitForReady(context.Background(), "default", "frs1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "Ready" {
		t.Errorf("state = %q", view.State)
	}
}

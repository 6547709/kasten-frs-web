package k8s

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func makeRP(ns, name, appName, appType, state string, created time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apps.kio.kasten.io", Version: "v1alpha1", Kind: "RestorePoint",
	})
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{
		"k10.kasten.io/appName":      appName,
		"k10.kasten.io/appNamespace": ns,
		"k10.kasten.io/appType":      appType,
	})
	if state != "" {
		_ = unstructured.SetNestedField(u.Object, state, "status", "state")
	}
	u.SetCreationTimestamp(metav1.Time{Time: created})
	return u
}

func newTestDynClient(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "apps.kio.kasten.io", Version: "v1alpha1", Kind: "RestorePointList",
	}, &unstructured.UnstructuredList{})
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(
		s, map[schema.GroupVersionResource]string{
			RestorePointGVR: "RestorePointList",
		}, objs...,
	)
}

func TestListVMs_DedupAndSort(t *testing.T) {
	now := time.Now()
	rps := []runtime.Object{
		makeRP("default", "rp1", "web-01", "virtualMachine", "Bound", now.Add(-1*time.Hour)),
		makeRP("default", "rp2", "web-01", "virtualMachine", "Bound", now.Add(-30*time.Minute)),
		makeRP("default", "rp3", "db-01", "virtualMachine", "Bound", now.Add(-2*time.Hour)),
		// non-VM RPs are ignored regardless of state
		makeRP("default", "rp4", "web-01", "namespace", "Bound", now),
		// Failed state no longer filters: user chooses namespace first
		makeRP("default", "rp5", "web-01", "virtualMachine", "Failed", now),
	}
	c := &Client{dyn: newTestDynClient(rps...)}
	vms, err := c.ListVMs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("got %d vms, want 2", len(vms))
	}
	// both VMs are in namespace "default"; web-01 has the latest RP (now)
	if vms[0].AppName != "web-01" || vms[1].AppName != "db-01" {
		t.Errorf("sort wrong: %+v", vms)
	}
	if vms[0].RPCount != 3 {
		t.Errorf("web-01 RPCount = %d, want 3", vms[0].RPCount)
	}
	if vms[1].RPCount != 1 {
		t.Errorf("db-01 RPCount = %d, want 1", vms[1].RPCount)
	}
}

func TestListVMs_NamespaceFilterAndCrossNS(t *testing.T) {
	// Same app name in two namespaces must surface as two separate VMs
	// when no filter is applied, and be reduced to one when filtered.
	now := time.Now()
	rps := []runtime.Object{
		makeRP("ns-a", "rp-a", "web01", "virtualMachine", "Bound", now),
		makeRP("ns-b", "rp-b", "web01", "virtualMachine", "Bound", now.Add(-1*time.Hour)),
		makeRP("ns-b", "rp-c", "db01", "virtualMachine", "Bound", now),
	}
	c := &Client{dyn: newTestDynClient(rps...)}
	all, err := c.ListVMs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("cluster-wide: got %d, want 3", len(all))
	}
	bOnly, err := c.ListVMs(context.Background(), []string{"ns-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bOnly) != 2 {
		t.Fatalf("ns-b only: got %d, want 2", len(bOnly))
	}
	for _, v := range bOnly {
		if v.AppNamespace != "ns-b" {
			t.Errorf("filter leaked: %+v", v)
		}
	}
}

func TestListVMNamespaces(t *testing.T) {
	now := time.Now()
	rps := []runtime.Object{
		makeRP("ns-a", "rp1", "web01", "virtualMachine", "Bound", now),
		makeRP("ns-b", "rp2", "db01", "virtualMachine", "Bound", now),
		makeRP("ns-a", "rp3", "cfg01", "namespace", "Bound", now), // ignored: not a VM
	}
	c := &Client{dyn: newTestDynClient(rps...)}
	got, err := c.ListVMNamespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ns-a", "ns-b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] got %s want %s", i, got[i], w)
		}
	}
}

func TestListRestorePoints_OrderByCreatedDesc(t *testing.T) {
	now := time.Now()
	rps := []runtime.Object{
		makeRP("default", "rp-old", "web-01", "virtualMachine", "Bound", now.Add(-3*time.Hour)),
		makeRP("default", "rp-new", "web-01", "virtualMachine", "Bound", now.Add(-1*time.Hour)),
		makeRP("default", "rp-mid", "web-01", "virtualMachine", "Bound", now.Add(-2*time.Hour)),
		makeRP("default", "rp-oth", "other", "virtualMachine", "Bound", now),
	}
	c := &Client{dyn: newTestDynClient(rps...)}
	got, err := c.ListRestorePoints(context.Background(), "default", "web-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	wantOrder := []string{"rp-new", "rp-mid", "rp-old"}
	for i, w := range wantOrder {
		if got[i].Name != w {
			t.Errorf("[%d] got %s want %s", i, got[i].Name, w)
		}
	}
}

func TestListVMs_AcceptsMissingStateField(t *testing.T) {
	// K10 in some deployments does not populate status.state at all
	// (only scheduledTime + sizes). A RP with VM labels + a size must
	// still be counted as an active VM; previously isActiveState("")
	// dropped it and the UI showed no VMs at all.
	now := time.Now()
	rps := []runtime.Object{
		makeRP("default", "rp-no-state", "rocky-9-nginx", "virtualMachine", "", now),
	}
	c := &Client{dyn: newTestDynClient(rps...)}
	vms, err := c.ListVMs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 {
		t.Fatalf("got %d vms, want 1 (state-less RP must count)", len(vms))
	}
	if vms[0].AppName != "rocky-9-nginx" || vms[0].RPCount != 1 {
		t.Errorf("vm = %+v", vms[0])
	}
}

func TestListRestorePoints_AcceptsMissingStateField(t *testing.T) {
	now := time.Now()
	rps := []runtime.Object{
		makeRP("default", "rp1", "rocky-9-nginx", "virtualMachine", "", now.Add(-1*time.Hour)),
		makeRP("default", "rp2", "rocky-9-nginx", "virtualMachine", "", now),
	}
	c := &Client{dyn: newTestDynClient(rps...)}
	got, err := c.ListRestorePoints(context.Background(), "default", "rocky-9-nginx")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d RPs, want 2 (state-less RPs must be listed)", len(got))
	}
}

func TestGetRestorePointDetails_ParsePVCs(t *testing.T) {
	body := []byte(`{
		"artifacts": [
			{"kind": "PersistentVolumeClaim", "name": "data-pvc", "occupiedSize": "10Gi"},
			{"kind": "ConfigMap", "name": "cfg"}
		]
	}`)
	arts, err := parseDetailsPVCs(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].PVCName != "data-pvc" || arts[0].Size != "10Gi" {
		t.Fatalf("got %+v", arts)
	}
}

// Real K10 returns the artifact list under status.restorePointDetails
// (not at the top level) and identifies each artifact by the GVR's
// resource (e.g. "persistentvolumeclaims") rather than a flat kind.
// This test guards against the schema being missed again — when the
// old top-level-only code was running, this same payload would parse
// to zero artifacts and the wizard would show "这个还原点没有 PVC artifacts".
func TestGetRestorePointDetails_RealSchema(t *testing.T) {
	body := []byte(`{
		"status": {
			"restorePointDetails": {
				"artifacts": [
					{"meta": {"spec": {"resource": "virtualmachines.virtualmachine.kubevirt.io", "name": "rocky-9-nginx"}}, "source": {"kind": "virtualmachine"}},
					{"meta": {"spec": {"resource": "persistentvolumeclaims", "name": "rocky-9-nginx-volume", "namespace": "default"}}, "occupiedSize": "100Gi", "source": {"kind": "virtualmachine"}}
				]
			}
		}
	}`)
	arts, err := parseDetailsPVCs(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d, want 1 (only the PVC)", len(arts))
	}
	if arts[0].PVCName != "rocky-9-nginx-volume" {
		t.Errorf("pvc name = %q, want rocky-9-nginx-volume", arts[0].PVCName)
	}
	if arts[0].Size != "100Gi" {
		t.Errorf("size = %q, want 100Gi", arts[0].Size)
	}
}

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
		makeRP("default", "rp4", "web-01", "namespace", "Bound", now),
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
	if vms[0].AppName != "web-01" || vms[1].AppName != "db-01" {
		t.Errorf("sort wrong: %+v", vms)
	}
	if vms[0].RPCount != 2 {
		t.Errorf("web-01 RPCount = %d, want 2", vms[0].RPCount)
	}
	if vms[1].RPCount != 1 {
		t.Errorf("db-01 RPCount = %d, want 1", vms[1].RPCount)
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

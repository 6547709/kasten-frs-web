package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// RestorePointGVR is the GVR for RestorePoint (and its /details subresource).
var RestorePointGVR = schema.GroupVersionResource{
	Group: "apps.kio.kasten.io", Version: "v1alpha1", Resource: "restorepoints",
}

// VM represents a deduplicated (appName, appNamespace) discovered via
// virtualMachine-labelled RestorePoints.
type VM struct {
	AppName      string
	AppNamespace string
	LastRPName   string
	LastRPTime   time.Time
	RPCount      int
}

// RestorePoint is a UI-friendly view of an apps.kio.kasten.io RestorePoint.
type RestorePoint struct {
	Name      string
	Namespace string
	State     string
	CreatedAt time.Time
}

// VolumeArtifact is a PVC exposed via the RestorePoint /details subresource.
type VolumeArtifact struct {
	PVCName string
	Size    string
	// IsClone is true for K10-managed DataVolume clones that
	// appear alongside the source PVC in the artifact list. K10
	// datamover accepts only the clone name as spec.volumes[].pvcName;
	// the source PVC alone 502s with "snapshot not found".
	IsClone bool
}

// ListVMs returns all VMs discovered via appType=virtualMachine RPs,
// grouped by (appNamespace, appName).
// namespaces is an optional allow-list (nil = all namespaces).
func (c *Client) ListVMs(ctx context.Context, namespaces []string) ([]VM, error) {
	u, err := c.dyn.Resource(RestorePointGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list restorepoints: %w", err)
	}
	allow := make(map[string]bool, len(namespaces))
	for _, n := range namespaces {
		allow[n] = true
	}
	type key struct{ name, ns string }
	seen := map[key]*VM{}
	for i := range u.Items {
		it := &u.Items[i]
		if it.GetLabels()["k10.kasten.io/appType"] != "virtualMachine" {
			continue
		}
		ns := it.GetLabels()["k10.kasten.io/appNamespace"]
		if len(allow) > 0 && !allow[ns] {
			continue
		}
		appName := it.GetLabels()["k10.kasten.io/appName"]
		k := key{appName, ns}
		v, ok := seen[k]
		if !ok {
			v = &VM{AppName: appName, AppNamespace: ns}
			seen[k] = v
		}
		v.RPCount++
		created := it.GetCreationTimestamp().Time
		if created.After(v.LastRPTime) {
			v.LastRPTime = created
			v.LastRPName = it.GetName()
		}
	}
	out := make([]VM, 0, len(seen))
	for _, v := range seen {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AppNamespace != out[j].AppNamespace {
			return out[i].AppNamespace < out[j].AppNamespace
		}
		if out[i].LastRPTime.Equal(out[j].LastRPTime) {
			return out[i].AppName < out[j].AppName
		}
		return out[i].LastRPTime.After(out[j].LastRPTime)
	})
	return out, nil
}

// ListVMNamespaces returns the distinct set of namespaces that have at
// least one appType=virtualMachine RestorePoint. Used to populate the
// namespace selector on the wizard's first step.
func (c *Client) ListVMNamespaces(ctx context.Context) ([]string, error) {
	u, err := c.dyn.Resource(RestorePointGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list restorepoints: %w", err)
	}
	set := map[string]struct{}{}
	for i := range u.Items {
		it := &u.Items[i]
		if it.GetLabels()["k10.kasten.io/appType"] != "virtualMachine" {
			continue
		}
		ns := it.GetLabels()["k10.kasten.io/appNamespace"]
		if ns == "" {
			continue
		}
		set[ns] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// ListRestorePoints returns RPs for (namespace, appName) ordered by createdAt desc.
func (c *Client) ListRestorePoints(ctx context.Context, ns, appName string) ([]RestorePoint, error) {
	sel := fmt.Sprintf("k10.kasten.io/appName=%s,k10.kasten.io/appType=virtualMachine", appName)
	u, err := c.dyn.Resource(RestorePointGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: sel,
	})
	if err != nil {
		return nil, fmt.Errorf("list restorepoints: %w", err)
	}
	out := make([]RestorePoint, 0, len(u.Items))
	for i := range u.Items {
		it := &u.Items[i]
		state, _, _ := unstructured.NestedString(it.Object, "status", "state")
		out = append(out, RestorePoint{
			Name: it.GetName(), Namespace: it.GetNamespace(),
			State: state, CreatedAt: it.GetCreationTimestamp().Time,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// GetRestorePointDetails fetches the /details subresource and returns
// PVC artifacts. Uses the client's raw http.Client + bearer-token
// because the dynamic client doesn't expose subresources and the
// typed REST client requires a GroupVersion + NegotiatedSerializer
// that apps.kio.kasten.io doesn't define in our scheme.
func (c *Client) GetRestorePointDetails(ctx context.Context, ns, name string) ([]VolumeArtifact, error) {
	body, err := c.doK8sRequest(ctx, "GET",
		fmt.Sprintf("/apis/apps.kio.kasten.io/v1alpha1/namespaces/%s/restorepoints/%s/details", ns, name),
	)
	if err != nil {
		return nil, fmt.Errorf("get rp details: %w", err)
	}
	return parseDetailsPVCs(body)
}

// parseDetailsPVCs extracts PVC artifacts from the RestorePoint /details JSON body.
//
// K10 has historically returned two different shapes depending on
// version + the deployment profile that produced the snapshot:
//
//   flat:   {"artifacts":[{"kind":"PersistentVolumeClaim","name":"data-pvc",...}]}
//   nested: {"status":{"restorePointDetails":{"artifacts":[{"meta":{"spec":{"resource":"persistentvolumeclaims","name":"data-pvc",...}}}]}}}
//
// Older deployments and unit tests use the flat shape; newer K10
// (the version our wizard is running against) uses the nested
// shape. Try the nested one first because that's what we see on
// the live cluster; fall back to flat so we don't regress older
// or mirrored deployments.
func parseDetailsPVCs(body []byte) ([]VolumeArtifact, error) {
	if arts, ok := extractNestedPVCDetails(body); ok {
		return arts, nil
	}
	return extractFlatPVCDetails(body)
}

func extractNestedPVCDetails(body []byte) ([]VolumeArtifact, bool) {
	var doc struct {
		Status struct {
			RestorePointDetails struct {
				Artifacts []map[string]any `json:"artifacts"`
			} `json:"restorePointDetails"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	artifacts := doc.Status.RestorePointDetails.Artifacts
	if len(artifacts) == 0 {
		return nil, false
	}
	var out []VolumeArtifact
	for _, m := range artifacts {
		meta, ok := m["meta"].(map[string]any)
		if !ok {
			continue
		}
		spec, ok := meta["spec"].(map[string]any)
		if !ok {
			continue
		}
		if res, _ := spec["resource"].(string); res != "persistentvolumeclaims" {
			continue
		}
		name, _ := spec["name"].(string)
		if name == "" {
			continue
		}
		size, _ := m["occupiedSize"].(string)
		out = append(out, VolumeArtifact{PVCName: name, Size: size})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func extractFlatPVCDetails(body []byte) ([]VolumeArtifact, error) {
	var raw struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal details: %w", err)
	}
	var out []VolumeArtifact
	for _, m := range raw.Artifacts {
		kind, _ := m["kind"].(string)
		if kind != "PersistentVolumeClaim" {
			continue
		}
		pvc, _ := m["name"].(string)
		size, _ := m["occupiedSize"].(string)
		out = append(out, VolumeArtifact{PVCName: pvc, Size: size})
	}
	return out, nil
}

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
// PVCNamespace and StorageClass are populated when the nested-meta
// schema carries them; they're used by the wizard to issue a
// DataVolume clone that K10's datamover can find a snapshot for.
type VolumeArtifact struct {
	PVCName      string
	PVCNamespace string
	Size         string
	StorageClass string
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
// PVC artifacts. Uses an http.Client with the in-cluster bearer token
// rather than a typed REST client: the dynamic client doesn't expose
// subresources and the typed REST client wants a GroupVersion +
// NegotiatedSerializer that the apps.kio.kasten.io API doesn't define
// in our scheme. DoRaw is sufficient because we only need the raw
// JSON body for parseDetailsPVCs.
func (c *Client) GetRestorePointDetails(ctx context.Context, ns, name string) ([]VolumeArtifact, error) {
	body, err := c.doK8sRequest(ctx, "GET",
		fmt.Sprintf("/apis/apps.kio.kasten.io/v1alpha1/namespaces/%s/restorepoints/%s/details", ns, name),
	)
	if err != nil {
		return nil, fmt.Errorf("get rp details: %w", err)
	}
	return parseDetailsPVCs(body)
}

// parseDetailsPVCs extracts PVC artifacts from the RestorePoint /details
// JSON body. K10's actual schema nests the artifact list at
// status.restorePointDetails.artifacts, and identifies each artifact by
// resource group (e.g. "persistentvolumeclaims") rather than by a flat
// "kind" field. Earlier code looked at the top-level `artifacts` and
// found nothing on real clusters.
func parseDetailsPVCs(body []byte) ([]VolumeArtifact, error) {
	var doc struct {
		Status struct {
			RestorePointDetails struct {
				Artifacts []map[string]any `json:"artifacts"`
			} `json:"restorePointDetails"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal details: %w", err)
	}
	artifacts := doc.Status.RestorePointDetails.Artifacts
	if len(artifacts) == 0 {
		// Fall back to a top-level artifacts array in case the deployment
		// uses a slightly different shape (e.g. older K10 or a custom
		// mirror).
		var alt struct {
			Artifacts []map[string]any `json:"artifacts"`
		}
		if err := json.Unmarshal(body, &alt); err == nil {
			artifacts = alt.Artifacts
		}
	}
	var out []VolumeArtifact
	for _, m := range artifacts {
		// Two ways an artifact can identify itself as a PVC:
		//  1. flat:   {"kind":"PersistentVolumeClaim",...}
		//  2. nested: {"meta":{"spec":{"resource":"persistentvolumeclaims",...}},...}
		if kind, _ := m["kind"].(string); kind == "PersistentVolumeClaim" {
			pvc, _ := m["name"].(string)
			size, _ := m["occupiedSize"].(string)
			out = append(out, VolumeArtifact{PVCName: pvc, Size: size})
			continue
		}
		if meta, ok := m["meta"].(map[string]any); ok {
			if spec, ok := meta["spec"].(map[string]any); ok {
				if res, _ := spec["resource"].(string); res == "persistentvolumeclaims" {
					name, _ := spec["name"].(string)
					pvcNs, _ := spec["namespace"].(string)
					size, _ := m["occupiedSize"].(string)
					// meta.spec.config is a JSON-stringified copy of
					// the live PVC spec; pull storageClassName +
					// resources.requests.storage from it so the
					// wizard can issue a matching DataVolume clone.
					var storageClass, pvcSize string
					if cfgStr, _ := spec["config"].(string); cfgStr != "" {
						var cfg map[string]any
						if json.Unmarshal([]byte(cfgStr), &cfg) == nil {
							storageClass, _ = cfg["spec"].(map[string]any)["storageClassName"].(string)
							if res, ok := cfg["spec"].(map[string]any)["resources"].(map[string]any); ok {
								if req, ok := res["requests"].(map[string]any); ok {
									if v, ok := req["storage"].(string); ok {
										pvcSize = v
									}
								}
							}
						}
					}
					if size == "" {
						size = humanStorageQuantity(pvcSize)
					} else {
						size = humanStorageQuantity(size)
					}
					out = append(out, VolumeArtifact{
						PVCName:      name,
						PVCNamespace: pvcNs,
						Size:         size,
						StorageClass: storageClass,
					})
				}
			}
		}
	}
	return out, nil
}

// humanStorageQuantity normalises a K8s storage quantity to a
// human-friendly string the DataVolume spec will accept. K10's RP
// artifact often returns raw bytes ("107374182400"); the DV
// resources.requests.storage field requires a suffix like "Gi".
// If the input already has a suffix, return it unchanged.
func humanStorageQuantity(s string) string {
	if s == "" {
		return s
	}
	if !isAllDigits(s) {
		return s
	}
	const (
		Ki = 1 << 10
		Mi = 1 << 20
		Gi = 1 << 30
		Ti = 1 << 40
	)
	n := parseUint(s)
	switch {
	case n >= Ti && n%Ti == 0:
		return fmt.Sprintf("%dTi", n/Ti)
	case n >= Gi && n%Gi == 0:
		return fmt.Sprintf("%dGi", n/Gi)
	case n >= Mi && n%Mi == 0:
		return fmt.Sprintf("%dMi", n/Mi)
	case n >= Ki && n%Ki == 0:
		return fmt.Sprintf("%dKi", n/Ki)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseUint(s string) uint64 {
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + uint64(c-'0')
	}
	return n
}

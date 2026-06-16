package k8s

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// FRSRef identifies a FileRecoverySession.
type FRSRef struct {
	Namespace string
	Name      string
}

// FRSView is a denormalized, UI-ready view of a FileRecoverySession.
type FRSView struct {
	Ref         FRSRef
	ServiceName string
	ServiceNS   string
	Port        int64
	HostKeySig  string
	ExpiryTime  time.Time
	State       string
	CreatedAt   time.Time
}

// FRSGroupVersionResource is the GVR for FileRecoverySession.
var FRSGroupVersionResource = schema.GroupVersionResource{
	Group: "datamover.kio.kasten.io", Version: "v1alpha1", Resource: "filerecoverysessions",
}

// ListActiveFRS returns FRSViews filtered to active+non-expired entries.
func (c *Client) ListActiveFRS(ctx context.Context, namespaces []string) ([]FRSView, error) {
	u, err := c.dyn.Resource(FRSGroupVersionResource).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list FRS: %w", err)
	}

	allow := make(map[string]bool, len(namespaces))
	for _, n := range namespaces {
		allow[n] = true
	}

	var out []FRSView
	now := time.Now()
	for i := range u.Items {
		item := &u.Items[i]
		if len(allow) > 0 && !allow[item.GetNamespace()] {
			continue
		}
		view, ok := buildFRSView(item)
		if !ok {
			continue
		}
		if !isActiveState(view.State) {
			continue
		}
		if !view.ExpiryTime.IsZero() && now.After(view.ExpiryTime) {
			continue
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// isActiveState reports whether s is a non-terminal FRS state.
// Empty state is treated as in-progress (K10 in some deployments does
// not populate status.state early; the FRS is still observable and
// expirable via the watch loop).
func isActiveState(s string) bool {
	switch s {
	case "Failed", "Succeeded", "Terminated":
		return false
	}
	return true
}

func buildFRSView(item *unstructured.Unstructured) (FRSView, bool) {
	view := FRSRef{Namespace: item.GetNamespace(), Name: item.GetName()}
	v := FRSView{Ref: view, CreatedAt: item.GetCreationTimestamp().Time}

	state, _, _ := unstructured.NestedString(item.Object, "status", "state")
	expiryStr, _, _ := unstructured.NestedString(item.Object, "status", "expiryTime")
	if expiryStr != "" {
		if t, err := time.Parse(time.RFC3339, expiryStr); err == nil {
			v.ExpiryTime = t
		}
	}
	v.State = state

	svc, _, _ := unstructured.NestedString(item.Object, "status", "transports", "sftp", "serviceName")
	svcNS, _, _ := unstructured.NestedString(item.Object, "status", "transports", "sftp", "serviceNamespace")
	port, _, _ := unstructured.NestedInt64(item.Object, "status", "transports", "sftp", "portNumber")
	hostKey, _, _ := unstructured.NestedString(item.Object, "status", "transports", "sftp", "hostKeySignature")
	if svc == "" || svcNS == "" || port == 0 {
		return v, false
	}
	v.ServiceName = svc
	v.ServiceNS = svcNS
	v.Port = port
	v.HostKeySig = hostKey
	return v, true
}

// GetFRS returns a single FRSView.
func (c *Client) GetFRS(ctx context.Context, ref FRSRef) (FRSView, error) {
	u, err := c.dyn.Resource(FRSGroupVersionResource).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return FRSView{}, fmt.Errorf("get FRS %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	v, ok := buildFRSView(u)
	if !ok {
		return FRSView{}, fmt.Errorf("FRS %s/%s not in connectable state", ref.Namespace, ref.Name)
	}
	return v, nil
}

// FRSpec is the spec for creating a FileRecoverySession.
type FRSpec struct {
	Name             string   // empty → use generateName: "frs-wizard-"
	RestorePointName string   // required
	PVCNames         []string // required, 1+
	SSHUserPublicKey string   // required, authorized_keys format
}

// CreateFRS creates a FileRecoverySession. Returns the FRSView on success.
// A freshly created FRS is typically not yet connectable (no service/port yet);
// callers should follow with WaitForReady to wait for state=Ready.
func (c *Client) CreateFRS(ctx context.Context, ns string, spec FRSpec) (*FRSView, error) {
	if spec.RestorePointName == "" || len(spec.PVCNames) == 0 || spec.SSHUserPublicKey == "" {
		return nil, fmt.Errorf("FRSpec: all of RestorePointName, PVCNames, SSHUserPublicKey required")
	}
	volumes := make([]any, 0, len(spec.PVCNames))
	for _, p := range spec.PVCNames {
		volumes = append(volumes, map[string]any{
			"restorePointName": spec.RestorePointName,
			"pvcName":          p,
		})
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "datamover.kio.kasten.io", Version: "v1alpha1", Kind: "FileRecoverySession",
	})
	obj.SetNamespace(ns)
	if spec.Name != "" {
		obj.SetName(spec.Name)
	} else {
		obj.SetGenerateName("frs-wizard-")
	}
	_ = unstructured.SetNestedSlice(obj.Object, volumes, "spec", "volumes")
	_ = unstructured.SetNestedField(obj.Object, map[string]any{
		"sftp": map[string]any{"userPublicKey": spec.SSHUserPublicKey},
	}, "spec", "transports")

	out, err := c.dyn.Resource(FRSGroupVersionResource).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create FRS: %w", err)
	}
	v, _ := buildFRSView(out)
	return &v, nil
}

// DeleteFRS deletes a FileRecoverySession. NotFound is not an error.
func (c *Client) DeleteFRS(ctx context.Context, ns, name string) error {
	err := c.dyn.Resource(FRSGroupVersionResource).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err == nil {
		return nil
	}
	var se *apierrors.StatusError
	if errors.As(err, &se) && se.ErrStatus.Reason == metav1.StatusReasonNotFound {
		return nil
	}
	return fmt.Errorf("delete FRS: %w", err)
}

// DataVolumeSource describes a single source PVC for a clone DataVolume.
// K10's FileRecoverySession datamover doesn't accept the source PVC
// name directly — it expects the name of a cdi.kubevirt.io
// DataVolume that has been cloned from the source. k10tools does this
// step before creating the FRS; the wizard must do the same so the
// datamover can find a snapshot in the RestorePoint artifact list.
type DataVolumeSource struct {
	SourcePVC      string
	SourcePVCNS    string
	Size           string // e.g. "100Gi"; empty → use source PVC's capacity
	StorageClass   string
	AccessModes    []string
}

// CloneDataVolume creates a cdi.kubevirt.io/v1beta1 DataVolume that
// clones src.SourcePVC into ns with a generated name. Returns the
// generated DV name and the resulting DataVolume object (unstructured).
// The caller is responsible for waiting on Succeeded phase.
func (c *Client) CloneDataVolume(ctx context.Context, ns string, src DataVolumeSource) (*unstructured.Unstructured, error) {
	if src.SourcePVC == "" {
		return nil, fmt.Errorf("CloneDataVolume: SourcePVC required")
	}
	if src.Size == "" {
		src.Size = "10Gi" // safe default; K10 doesn't strictly need an exact match
	}
	if len(src.AccessModes) == 0 {
		src.AccessModes = []string{"ReadWriteOnce"}
	}
	dvGVR := schema.GroupVersionResource{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes",
	}
	// 5-char base36 suffix, matching the k10tools naming convention.
	// We use crypto/rand via std lib.
	suffix := randSuffix(5)
	dvName := fmt.Sprintf("dv-wizard-%s-%s", src.SourcePVC, suffix)

	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume",
	})
	dv.SetNamespace(ns)
	dv.SetName(dvName)
	// Build the DV spec via the unstructured helpers (SetNestedMap /
	// SetNestedSlice) rather than a single SetNestedMap of nested
	// map[string]any values. The runtime deep-copy that helper uses
	// panics on []string accessModes when the source is a literal
	// []any{} in a map[string]any; flattening it through the typed
	// setters avoids the panic.
	_ = unstructured.SetNestedField(dv.Object, map[string]any{
		"name":      src.SourcePVC,
		"namespace": src.SourcePVCNS,
	}, "spec", "source", "pvc")
	pvcSpec := map[string]any{
		"resources": map[string]any{
			"requests": map[string]any{"storage": src.Size},
		},
	}
	if src.StorageClass != "" {
		pvcSpec["storageClassName"] = src.StorageClass
	}
	_ = unstructured.SetNestedMap(dv.Object, pvcSpec, "spec", "pvc")
	accessModes := make([]any, len(src.AccessModes))
	for i, m := range src.AccessModes {
		accessModes[i] = m
	}
	_ = unstructured.SetNestedSlice(dv.Object, accessModes, "spec", "pvc", "accessModes")

	out, err := c.dyn.Resource(dvGVR).Namespace(ns).Create(ctx, dv, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create DataVolume: %w", err)
	}
	return out, nil
}

// WaitDataVolumeSucceeded blocks until the named DataVolume reaches
// phase=Succeeded, or returns an error if it Failed or the timeout
// elapses.
func (c *Client) WaitDataVolumeSucceeded(ctx context.Context, ns, name string, timeout time.Duration) error {
	dvGVR := schema.GroupVersionResource{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes",
	}
	deadline := time.Now().Add(timeout)
	for {
		u, err := c.dyn.Resource(dvGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get dv %s/%s: %w", ns, name, err)
		}
		phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
		switch phase {
		case "Succeeded":
			return nil
		case "Failed":
			return fmt.Errorf("DataVolume %s/%s Failed", ns, name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("DataVolume %s/%s did not reach Succeeded within %s (last phase=%q)", ns, name, timeout, phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// randSuffix returns n random lowercase base36 characters. Used for
// generating wizard-created DataVolume names without collisions.
func randSuffix(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is exceptional; fall back to time-based
		// suffix so the wizard still produces something usable.
		return fmt.Sprintf("%x", time.Now().UnixNano())[:n]
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// DeleteDataVolume removes a cdi.kubevirt.io DataVolume. NotFound is
// not an error so callers can use it as a best-effort cleanup.
func (c *Client) DeleteDataVolume(ctx context.Context, ns, name string) error {
	dvGVR := schema.GroupVersionResource{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes",
	}
	err := c.dyn.Resource(dvGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err == nil {
		return nil
	}
	var se *apierrors.StatusError
	if errors.As(err, &se) && se.ErrStatus.Reason == metav1.StatusReasonNotFound {
		return nil
	}
	return fmt.Errorf("delete dv: %w", err)
}

// WaitForReady polls status.state until Ready/Failed/timeout.
// Returns the latest FRSView.
func (c *Client) WaitForReady(ctx context.Context, ns, name string, timeout time.Duration) (FRSView, error) {
	deadline := time.Now().Add(timeout)
	for {
		v, err := c.GetFRS(ctx, FRSRef{Namespace: ns, Name: name})
		if err == nil {
			switch v.State {
			case "Ready":
				return v, nil
			case "Failed":
				return v, fmt.Errorf("FRS %s/%s reached state=Failed", ns, name)
			}
		}
		if time.Now().After(deadline) {
			return v, fmt.Errorf("FRS %s/%s did not reach Ready within %s (last state=%q)", ns, name, timeout, v.State)
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

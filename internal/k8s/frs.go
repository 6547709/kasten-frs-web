package k8s

import (
	"context"
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

func isActiveState(s string) bool {
	switch s {
	case "Failed", "Succeeded", "Terminated", "":
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

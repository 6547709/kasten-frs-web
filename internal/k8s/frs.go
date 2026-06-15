package k8s

import (
	"context"
	"fmt"
	"sort"
	"time"

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
	Ref             FRSRef
	ServiceName     string
	ServiceNS       string
	Port            int64
	HostKeySig      string
	ExpiryTime      time.Time
	State           string
	CreatedAt       time.Time
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
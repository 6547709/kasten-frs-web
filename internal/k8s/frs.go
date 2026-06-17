package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/liguoqiang/kasten-frs-web/internal/metrics"
)

// recordK8sError logs a K8s API error with its GVR/op and HTTP status
// (when available) and bumps the K8sAPIErrorsTotal counter so
// operators can alert on apiserver problems instead of only seeing
// them rendered as a user-facing 502.
func recordK8sError(op, namespace string, err error) {
	code := "unknown"
	var se *apierrors.StatusError
	if errors.As(err, &se) {
		code = strconv.Itoa(int(se.ErrStatus.Code))
	}
	metrics.K8sAPIErrorsTotal.WithLabelValues(op, code).Inc()
	slog.Warn("k8s.api.error", "op", op, "namespace", namespace, "code", code, "err", err)
}

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
	// SourceApp is the VM/app name this FRS was created from
	// (parsed from the FRS spec.volumes[].restorePointName →
	// RestorePoint metadata.labels.k10.kasten.io/appName).
	// Empty when the FRS was created out-of-band.
	SourceApp string
	// SourceAppNS is the namespace of that app.
	SourceAppNS string
	// RestorePointCreatedAt is the creation timestamp of the
	// RestorePoint this FRS is bound to. Set alongside
	// SourceApp so the table can show "VM @ time" instead of
	// just "FRS name".
	RestorePointCreatedAt time.Time
	// Connectable is true once the FRS has published its SFTP
	// transport (serviceName + serviceNamespace + portNumber).
	// A freshly created / Pending FRS is observable and worth
	// showing in the sessions list, but its Browse action must
	// be disabled until Connectable flips true.
	Connectable bool
	// Terminal is true for FRSes in a non-recoverable end state
	// (Failed / Succeeded / Terminated). These can no longer become
	// connectable — e.g. a timed-out FRS goes Failed and K10 tears
	// down its frs-xxx pod — so the UI only offers a Delete action
	// for them, letting operators clean up the leftover CR objects.
	Terminal bool
}

// FRSGroupVersionResource is the GVR for FileRecoverySession.
var FRSGroupVersionResource = schema.GroupVersionResource{
	Group: "datamover.kio.kasten.io", Version: "v1alpha1", Resource: "filerecoverysessions",
}

// ListActiveFRS returns FRSViews filtered to active+non-expired entries.
// Kept for callers that only care about live sessions; the UI session
// list uses ListAllFRS so operators can also see (and clean up)
// terminal/expired FRSes.
func (c *Client) ListActiveFRS(ctx context.Context, namespaces []string) ([]FRSView, error) {
	return c.listFRS(ctx, namespaces, false)
}

// ListAllFRS returns every FRSView in the (optionally whitelisted)
// namespaces, INCLUDING terminal (Failed/Succeeded/Terminated) and
// expired sessions. This is what the sessions page renders: a timed-out
// FRS flips to Failed and K10 deletes its frs-xxx pod, but the FRS CR
// lingers in the cluster. Hiding those left the operator with no way to
// see — let alone delete — the accumulating garbage. Now they all show
// up, with non-Ready rows offering only a Delete action.
func (c *Client) ListAllFRS(ctx context.Context, namespaces []string) ([]FRSView, error) {
	return c.listFRS(ctx, namespaces, true)
}

// listFRS is the shared listing core. When includeTerminal is false it
// drops terminal-state and expired FRSes (legacy ListActiveFRS
// behaviour); when true it returns everything.
func (c *Client) listFRS(ctx context.Context, namespaces []string, includeTerminal bool) ([]FRSView, error) {
	u, err := c.dyn.Resource(FRSGroupVersionResource).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		recordK8sError("list_frs", "", err)
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
		// buildFRSView always returns a usable view; the bool now
		// only reports connectability. We intentionally do NOT skip
		// non-connectable (Pending) FRSes here — they belong in the
		// list with Browse disabled so users can see in-flight
		// sessions instead of them silently vanishing.
		view, _ := buildFRSView(item)
		if !includeTerminal {
			if !isActiveState(view.State) {
				continue
			}
			if !view.ExpiryTime.IsZero() && now.After(view.ExpiryTime) {
				continue
			}
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
	return !isTerminalState(s)
}

// isTerminalState reports whether s is a non-recoverable end state.
func isTerminalState(s string) bool {
	switch s {
	case "Failed", "Succeeded", "Terminated":
		return true
	}
	return false
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
	v.Terminal = isTerminalState(state)

	svc, _, _ := unstructured.NestedString(item.Object, "status", "transports", "sftp", "serviceName")
	svcNS, _, _ := unstructured.NestedString(item.Object, "status", "transports", "sftp", "serviceNamespace")
	port, _, _ := unstructured.NestedInt64(item.Object, "status", "transports", "sftp", "portNumber")
	hostKey, _, _ := unstructured.NestedString(item.Object, "status", "transports", "sftp", "hostKeySignature")
	v.ServiceName = svc
	v.ServiceNS = svcNS
	v.Port = port
	v.HostKeySig = hostKey
	// An FRS is connectable once K10 has published its SFTP service
	// coordinates. Pending FRSes lack these; we still return a
	// populated view (Connectable=false) so callers can list it.
	if svc == "" || svcNS == "" || port == 0 {
		v.Connectable = false
		return v, false
	}
	v.Connectable = true
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

// LookupFRSSource fills in SourceApp / SourceAppNS /
// RestorePointCreatedAt on the given FRSView by reading the FRS
// spec.volumes[].restorePointName and pulling the matching
// RestorePoint's labels + creation timestamp. Best-effort: any
// error leaves the FRSView unchanged so the table can still
// render the row.
func (c *Client) LookupFRSSource(ctx context.Context, v *FRSView) {
	if c.isFake {
		return
	}
	// Fetch the single underlying FRS object directly by name so we
	// can read spec.volumes[0].restorePointName. Previously this did
	// a full namespace List + linear scan, which meant enriching N
	// sessions issued N full List calls against the apiserver. A
	// Get(name) is O(1) on the server side and scales cleanly with
	// the number of sessions on the page.
	item, err := c.dyn.Resource(FRSGroupVersionResource).Namespace(v.Ref.Namespace).Get(ctx, v.Ref.Name, metav1.GetOptions{})
	if err != nil {
		return
	}
	volumes, _, _ := unstructured.NestedSlice(item.Object, "spec", "volumes")
	for _, vol := range volumes {
		m, ok := vol.(map[string]any)
		if !ok {
			continue
		}
		rpName, _, _ := unstructured.NestedString(m, "restorePointName")
		if rpName == "" {
			continue
		}
		// FRS spec.volumes is always against the same namespace
		// as the FRS itself.
		rp, err := c.dyn.Resource(RestorePointGVR).Namespace(v.Ref.Namespace).Get(ctx, rpName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		v.SourceApp = rp.GetLabels()["k10.kasten.io/appName"]
		v.SourceAppNS = rp.GetLabels()["k10.kasten.io/appNamespace"]
		v.RestorePointCreatedAt = rp.GetCreationTimestamp().Time
		return
	}
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

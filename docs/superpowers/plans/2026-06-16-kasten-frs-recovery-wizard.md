# FRS Recovery Wizard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a single-page web wizard to `kasten-frs-web` that lets an operator select VM → restore point → volumes, with the helper creating the FileRecoverySession directly via the K8s dynamic client. Includes auto-managed SSH keys, FRS cleanup UI, expiration countdown, and a deploy-experience doc.

**Architecture:** Veeam-style master-detail page at `/wizard` using htmx partial refresh. New `internal/keymgr` package owns SSH key lifecycle (read or generate-or-Derive on startup). `internal/k8s` extends with RestorePoint listing and FRS Create/Delete/Wait. FRS ready polling is async via in-memory watch map; the HTTP handler always 303-redirects immediately to avoid HAProxy 30s edge timeouts.

**Tech Stack:** Go 1.22, `client-go` dynamic client, htmx 1.x, html/template, `golang.org/x/crypto/ssh` (ed25519). Existing test infrastructure: `k8s.io/client-go/dynamic/fake` and `k8s.io/client-go/kubernetes/fake`.

**Spec:** `docs/superpowers/specs/2026-06-16-kasten-frs-recovery-wizard-design.md` (commit `45882a0`). Read alongside this plan.

---

## File Structure

| File | Responsibility |
|---|---|
| `deploy/06-rbac.yaml` | (modify) Add `create,delete` to FRS, add `restorepoints` read, add `secret` create/update/patch |
| `internal/config/config.go` | (modify) Add `FRSWaitTimeout` env var (default 30s) |
| `internal/k8s/restorepoints.go` | (new) `ListVMs`, `ListRestorePoints`, `GetRestorePointDetails` via dynamic client |
| `internal/k8s/restorepoints_test.go` | (new) TDD tests using fake dynamic client |
| `internal/k8s/frs.go` | (modify) Add `FRSpec`, `CreateFRS`, `DeleteFRS`, `WaitForReady` |
| `internal/k8s/frs_test.go` | (modify) Tests for new FRS functions |
| `internal/k8s/scheme.go` | (modify) Register `RestorePoint` and `RestorePointDetails` types in fake scheme |
| `internal/keymgr/keymgr.go` | (new) `LoadOrGenerate` reads Secret; generates/derives SSH keys |
| `internal/keymgr/keymgr_test.go` | (new) TDD coverage of 4 secret states |
| `cmd/helper/main.go` | (modify) Wire `keymgr.Manager` into sftpclient + handlers |
| `internal/handlers/handlers.go` | (modify) Register wizard routes; expose `FRSProvider.CreateFRS` etc; add watch map |
| `internal/handlers/wizard.go` | (new) 5 wizard handlers (`Page`, `VMs`, `RPs`, `Volumes`, `Create`, `Cancel`) |
| `internal/handlers/wizard_test.go` | (new) htmx fragment rendering tests |
| `internal/handlers/sessions.go` | (new) `handleSessionDelete` |
| `web/templates/wizard.html` | (new) 3-panel master-detail with htmx wiring |
| `web/templates/sessions.html` | (modify) Countdown cell + delete column |
| `web/templates/browse.html` | (modify) Delete button + `browse_preparing_body` |
| `web/templates/layout.html` | (modify) Add "Wizard" nav item; include countdown JS on /sessions |
| `web/static/veeam-theme.css` | (modify) Add `.badge-warn`, `.badge-crit` rules |
| `scripts/deploy-test.sh` | (modify) Add `oc auth can-i` RBAC verification step |
| `docs/superpowers/experience/2026-06-16-deploy-experience.md` | (new) Deploy/test experience doc |
| `README.md` | (modify) Add "Wizard" section |
| `DEPLOY.md` | (modify) Drop "generate SSH key" step; add wizard e2e smoke |
| `CHANGELOG.md` | (modify) v0.3.0 entry |
| `scripts/wizard-test.sh` | (new, **gitignored**) Fake-client e2e for wizard |

---

## Task 1: RBAC expansion + e2e auth can-i check

**Files:**
- Modify: `deploy/06-rbac.yaml`
- Modify: `scripts/deploy-test.sh`

The current ClusterRole grants `get,list,watch` on `filerecoverysessions` and `get` on the private-key Secret. We add `create,delete` on FRS, `get,list` on `restorepoints`, `get` on the `restorepoints/details` subresource, and `create,update,patch` on the private-key Secret (keymgr needs to write back derived/generated keys).

- [ ] **Step 1: Update the ClusterRole in `deploy/06-rbac.yaml`**

Replace the existing `ClusterRole` block (lines 1-9) with:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kasten-frs-web-helper-frs-list
rules:
- apiGroups: ["datamover.kio.kasten.io"]
  resources: ["filerecoverysessions"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: ["apps.kio.kasten.io"]
  resources: ["restorepoints"]
  verbs: ["get", "list"]
- apiGroups: ["apps.kio.kasten.io"]
  resources: ["restorepoints/details"]
  verbs: ["get"]
```

Replace the existing `Role` block (lines 23-32) with:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kasten-frs-web-helper-secret-read
  namespace: kasten-io
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["kasten-frs-helper-private-key"]
  verbs: ["get", "create", "update", "patch"]
```

- [ ] **Step 2: Add a `kubectl auth can-i` step to `scripts/deploy-test.sh`**

Find the `secrets` step in `scripts/deploy-test.sh` (the one that creates `kasten-frs-helper-private-key`). Immediately after that step, before the `overlay` step, insert:

```bash
# Verify the helper's ServiceAccount can create FRS, read RPs, and
# write the private-key secret. This is the M1 milestone gate.
SA="-n kasten-io sa/kasten-frs-web-helper"
for v in \
    "create filerecoverysessions.datamover.kio.kasten.io" \
    "delete filerecoverysessions.datamover.kio.kasten.io" \
    "list  restorepoints.apps.kio.kasten.io" \
    "get   restorepoints.apps.kio.kasten.io" \
    "get   restorepoints.details.apps.kio.kasten.io" \
    "create secret" \
    "update secret" \
    "patch  secret"; do
    if ! kubectl auth can-i $v $SA; then
        die "RBAC missing: SA cannot $v ($v $SA)"
    fi
done
log "RBAC can-i checks pass"
```

Also add `die` if it's not already defined (it is — `die()` is used elsewhere in the script; if absent, define it as `die() { echo "FATAL: $*" >&2; exit 1; }`).

- [ ] **Step 3: Run the deploy-test script in dry-mode to verify the new step is syntactically valid**

Run: `bash -n scripts/deploy-test.sh && echo "syntax ok"`
Expected: `syntax ok`

- [ ] **Step 4: Commit**

```bash
git add deploy/06-rbac.yaml scripts/deploy-test.sh
git commit -m "rbac: add FRS create/delete, restorepoints read, secret write for wizard"
```

---

## Task 2: `internal/k8s/scheme.go` — register new types for fake client

**Files:**
- Modify: `internal/k8s/scheme.go`

The fake dynamic client needs to know about the `RestorePoint` and `RestorePointDetails` GVRs so the TDD tests in Task 4 can build Unstructured objects.

- [ ] **Step 1: Read `internal/k8s/scheme.go` to see current registration**

Run: `cat internal/k8s/scheme.go`

The file currently builds a runtime.Scheme and registers FRS GVR via AddKnownTypeWithName. We need to add the same for RestorePoint and RestorePointDetails.

- [ ] **Step 2: Add the new types to `internal/k8s/scheme.go`**

In the same file, inside the `init()` (or wherever GVRs are registered), add after the existing FRS registration:

```go
// RestorePoint — apps.kio.kasten.io/v1alpha1
RestorePointGVR = schema.GroupVersionResource{
    Group: "apps.kio.kasten.io", Version: "v1alpha1", Resource: "restorepoints",
}
// Register an Unstructured-compatible type so the fake dynamic client
// accepts restorepoints objects in tests.
scheme.AddKnownTypeWithName(
    schema.GroupVersionKind{Group: "apps.kio.kasten.io", Version: "v1alpha1", Kind: "RestorePoint"},
    &unstructured.UnstructuredList{},
)
```

(If the file uses a different registration pattern, follow the existing pattern. The goal is: the fake dynamic client can `Get/List` restorepoints objects without panicking on unknown GVK.)

- [ ] **Step 3: Build to verify the type compiles**

Run: `go build ./internal/k8s/...`
Expected: no errors

- [ ] **Step 4: Commit**

```bash
git add internal/k8s/scheme.go
git commit -m "k8s(scheme): register RestorePoint GVR for fake dynamic client"
```

---

## Task 3: `internal/k8s/restorepoints.go` — TDD

**Files:**
- Create: `internal/k8s/restorepoints.go`
- Create: `internal/k8s/restorepoints_test.go`

This file owns: `VM`, `RestorePoint`, `VolumeArtifact` types, and `ListVMs`, `ListRestorePoints`, `GetRestorePointDetails` methods on `*Client`. The VM list deduplicates RestorePoint entries by (appName, appNamespace) where label `k10.kasten.io/appType=virtualMachine`.

- [ ] **Step 1: Write the failing test `internal/k8s/restorepoints_test.go`**

```go
package k8s

import (
    "context"
    "testing"
    "time"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime"
    dynfake "k8s.io/client-go/dynamic/fake"
    "k8s.io/client-go/kubernetes/scheme"
)

func makeRP(ns, name, appName, appType, state string, created time.Time) *unstructured.Unstructured {
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(restorePointGVK)
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
    s.AddKnownTypeWithName(restorePointGVK, &unstructured.UnstructuredList{})
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
        makeRP("default", "rp3", "db-01",  "virtualMachine", "Bound", now.Add(-2*time.Hour)),
        makeRP("default", "rp4", "web-01", "namespace",      "Bound", now), // ignored
        makeRP("default", "rp5", "web-01", "virtualMachine", "Failed", now), // ignored
    }
    c := &Client{dyn: newTestDynClient(rps...)}
    vms, err := c.ListVMs(context.Background(), nil)
    if err != nil { t.Fatal(err) }
    if len(vms) != 2 { t.Fatalf("got %d vms, want 2", len(vms)) }
    // most-recent-rp first
    if vms[0].AppName != "web-01" || vms[1].AppName != "db-01" {
        t.Errorf("sort wrong: %+v", vms)
    }
    if vms[0].RPCount != 2 { t.Errorf("web-01 RPCount = %d, want 2", vms[0].RPCount) }
    if vms[1].RPCount != 1 { t.Errorf("db-01 RPCount = %d, want 1", vms[1].RPCount) }
}

func TestListRestorePoints_OrderByCreatedDesc(t *testing.T) {
    now := time.Now()
    rps := []runtime.Object{
        makeRP("default", "rp-old", "web-01", "virtualMachine", "Bound", now.Add(-3*time.Hour)),
        makeRP("default", "rp-new", "web-01", "virtualMachine", "Bound", now.Add(-1*time.Hour)),
        makeRP("default", "rp-mid", "web-01", "virtualMachine", "Bound", now.Add(-2*time.Hour)),
        makeRP("default", "rp-oth", "other",  "virtualMachine", "Bound", now), // wrong app
    }
    c := &Client{dyn: newTestDynClient(rps...)}
    got, err := c.ListRestorePoints(context.Background(), "default", "web-01")
    if err != nil { t.Fatal(err) }
    if len(got) != 3 { t.Fatalf("got %d, want 3", len(got)) }
    wantOrder := []string{"rp-new", "rp-mid", "rp-old"}
    for i, w := range wantOrder {
        if got[i].Name != w { t.Errorf("[%d] got %s want %s", i, got[i].Name, w) }
    }
}

func TestGetRestorePointDetails_FilterPVCs(t *testing.T) {
    details := &unstructured.Unstructured{}
    details.SetGroupVersionKind(schema.GroupVersionKind{
        Group: "apps.kio.kasten.io", Version: "v1alpha1", Kind: "RestorePointDetails",
    })
    _ = unstructured.SetNestedSlice(details.Object, []any{
        map[string]any{
            "kind": "PersistentVolumeClaim",
            "name": "data-pvc", "namespace": "default",
            "occupiedSize": "10Gi",
        },
        map[string]any{
            "kind": "ConfigMap",
            "name": "cfg", "namespace": "default",
        },
    }, "artifacts")

    // This test requires the fake client to support the /details
    // subresource. If the fake client doesn't, fall back to a stubbed
    // method that reads from a pre-seeded object store. See step 3
    // for the workaround.
    // For now, mark as expected-fail:
    t.Skip("implementation pending; see restorepoints.go step 3")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/k8s/ -run "TestListVMs|TestListRestorePoints" -v`
Expected: compile error (functions not defined) or FAIL

- [ ] **Step 3: Implement `internal/k8s/restorepoints.go`**

```go
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

var restorePointGVK = schema.GroupVersionKind{
    Group: "apps.kio.kasten.io", Version: "v1alpha1", Kind: "RestorePoint",
}

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
    Size    string // keep as string; K10 emits "10Gi" etc.
}

// ListVMs returns all VMs discovered via appType=virtualMachine RPs.
// namespaces is an optional cluster-wide filter (nil = all).
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
        return out[i].LastRPTime.After(out[j].LastRPTime)
    })
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
// PVC artifacts.
func (c *Client) GetRestorePointDetails(ctx context.Context, ns, name string) ([]VolumeArtifact, error) {
    // The dynamic client doesn't expose subresources by default; use
    // the REST client to GET the raw subresource URL.
    rc, err := buildRESTFor(c)
    if err != nil {
        return nil, err
    }
    body, err := rc.Get().AbsPath(
        fmt.Sprintf("/apis/apps.kio.kasten.io/v1alpha1/namespaces/%s/restorepoints/%s/details", ns, name),
    ).DoRaw(ctx)
    if err != nil {
        return nil, fmt.Errorf("get rp details: %w", err)
    }
    u := &unstructured.Unstructured{}
    if err := u.UnmarshalJSON(body); err != nil {
        return nil, fmt.Errorf("unmarshal details: %w", err)
    }
    arts, _, err := unstructured.NestedSlice(u.Object, "artifacts")
    if err != nil {
        return nil, fmt.Errorf("parse artifacts: %w", err)
    }
    var out []VolumeArtifact
    for _, a := range arts {
        m, ok := a.(map[string]any)
        if !ok { continue }
        kind, _, _ := unstructured.NestedString(m, "kind")
        if kind != "PersistentVolumeClaim" { continue }
        pvc, _, _ := unstructured.NestedString(m, "name")
        size, _, _ := unstructured.NestedString(m, "occupiedSize")
        out = append(out, VolumeArtifact{PVCName: pvc, Size: size})
    }
    return out, nil
}
```

`buildRESTFor(c)` is a small helper that returns a `restclient.Interface` from the same rest.Config used by `c.dyn`. Add this helper at the top of the file (or in `client.go`):

```go
func buildRESTFor(c *Client) (restclient.Interface, error) {
    cfg, err := rest.InClusterConfig()
    if err != nil { cfg, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile) }
    if err != nil { return nil, err }
    return restclient.RESTClientFor(cfg)
}
```

(If the `Client` struct already stores a rest.Config, use that instead. The fallback to kubeconfig is only for the out-of-cluster dev path.)

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/k8s/ -run "TestListVMs|TestListRestorePoints" -v`
Expected: PASS for both. The third test (`TestGetRestorePointDetails_FilterPVCs`) is `t.Skip`'d — see step 5 for the subresource test approach.

- [ ] **Step 5: Add a subresource test using a fake REST client**

Append to `internal/k8s/restorepoints_test.go`:

```go
func TestGetRestorePointDetails_ParsePVCs(t *testing.T) {
    // Bypass the dynamic client; directly call a refactored pure parser.
    body := []byte(`{
        "artifacts": [
            {"kind": "PersistentVolumeClaim", "name": "data-pvc", "occupiedSize": "10Gi"},
            {"kind": "ConfigMap", "name": "cfg"}
        ]
    }`)
    arts, err := parseDetailsPVCs(body)
    if err != nil { t.Fatal(err) }
    if len(arts) != 1 || arts[0].PVCName != "data-pvc" || arts[0].Size != "10Gi" {
        t.Fatalf("got %+v", arts)
    }
}
```

Extract the parser to a private helper so it's testable without a fake REST client. Replace the body of `GetRestorePointDetails` with:

```go
func (c *Client) GetRestorePointDetails(ctx context.Context, ns, name string) ([]VolumeArtifact, error) {
    rc, err := buildRESTFor(c)
    if err != nil { return nil, err }
    body, err := rc.Get().AbsPath(
        fmt.Sprintf("/apis/apps.kio.kasten.io/v1alpha1/namespaces/%s/restorepoints/%s/details", ns, name),
    ).DoRaw(ctx)
    if err != nil { return nil, fmt.Errorf("get rp details: %w", err) }
    return parseDetailsPVCs(body)
}

func parseDetailsPVCs(body []byte) ([]VolumeArtifact, error) {
    u := &unstructured.Unstructured{}
    if err := u.UnmarshalJSON(body); err != nil { return nil, err }
    arts, _, err := unstructured.NestedSlice(u.Object, "artifacts")
    if err != nil { return nil, err }
    var out []VolumeArtifact
    for _, a := range arts {
        m, ok := a.(map[string]any)
        if !ok { continue }
        kind, _, _ := unstructured.NestedString(m, "kind")
        if kind != "PersistentVolumeClaim" { continue }
        pvc, _, _ := unstructured.NestedString(m, "name")
        size, _, _ := unstructured.NestedString(m, "occupiedSize")
        out = append(out, VolumeArtifact{PVCName: pvc, Size: size})
    }
    return out, nil
}
```

Run: `go test ./internal/k8s/ -v -run "TestListVMs|TestListRestorePoints|TestGetRestorePointDetails"`
Expected: all 3 PASS

- [ ] **Step 6: Commit**

```bash
git add internal/k8s/restorepoints.go internal/k8s/restorepoints_test.go
git commit -m "k8s: add ListVMs, ListRestorePoints, GetRestorePointDetails"
```

---

## Task 4: `internal/k8s/frs.go` — extend with Create / Delete / Wait

**Files:**
- Modify: `internal/k8s/frs.go`
- Modify: `internal/k8s/frs_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/k8s/frs_test.go`:

```go
func TestCreateFRS_SpecShape(t *testing.T) {
    c := newTestClientWithDyn(fakeDyn())
    spec := FRSpec{
        RestorePointName: "rp1",
        PVCNames:         []string{"data-pvc", "logs-pvc"},
        SSHUserPublicKey: "ssh-ed25519 AAAA... user@host",
    }
    frs, err := c.CreateFRS(context.Background(), "default", spec)
    if err != nil { t.Fatal(err) }
    if frs.Ref.Namespace != "default" { t.Errorf("ns = %s", frs.Ref.Namespace) }
    // Round-trip: GET the created FRS and check spec shape.
    got, err := c.GetFRS(context.Background(), frs.Ref)
    if err != nil { t.Fatal(err) }
    un, ok := got.(*unstructured.Unstructured) // or whatever the existing GetFRS returns
    _ = un
    _ = ok
    // Check volumes + userPublicKey on the raw object. Use a fresh GET
    // through the dynamic client.
    raw, err := c.dyn.Resource(FRSGroupVersionResource).Namespace("default").Get(
        context.Background(), frs.Ref.Name, metav1.GetOptions{},
    )
    if err != nil { t.Fatal(err) }
    vols, _, _ := unstructured.NestedSlice(raw.Object, "spec", "volumes")
    if len(vols) != 2 { t.Errorf("got %d volumes, want 2", len(vols)) }
    key, _, _ := unstructured.NestedString(raw.Object, "spec", "transports", "sftp", "userPublicKey")
    if key != "ssh-ed25519 AAAA... user@host" {
        t.Errorf("userPublicKey = %q", key)
    }
}

func TestCreateFRS_GenerateName(t *testing.T) {
    c := newTestClientWithDyn(fakeDyn())
    spec := FRSpec{ /* Name empty */ RestorePointName: "rp1", PVCNames: []string{"p"}, SSHUserPublicKey: "k" }
    frs, _ := c.CreateFRS(context.Background(), "default", spec)
    if !strings.HasPrefix(frs.Ref.Name, "frs-wizard-") {
        t.Errorf("name = %q, want frs-wizard- prefix", frs.Ref.Name)
    }
}

func TestDeleteFRS_Idempotent(t *testing.T) {
    c := newTestClientWithDyn(fakeDyn())
    // delete without create — should not error
    if err := c.DeleteFRS(context.Background(), "default", "nope"); err != nil {
        t.Errorf("expected nil, got %v", err)
    }
}

func TestWaitForReady_StateMachine(t *testing.T) {
    // Build a dynamic client that returns state=Starting, then state=Ready
    // on subsequent Gets. Use a counter and a fake REST client, or
    // manipulate the object store between calls.
    // For simplicity, pre-seed a FRS object and have the fake client
    // serve it; the poll loop should exit early when state=Ready.
    c := newTestClientWithDyn(fakeDynWithFRS("default", "frs1", "Ready"))
    view, err := c.WaitForReady(context.Background(), "default", "frs1", 5*time.Second)
    if err != nil { t.Fatal(err) }
    if view.State != "Ready" { t.Errorf("state = %q", view.State) }
}
```

If the existing `Client` constructor pattern doesn't lend itself to test injection, refactor as needed. The key is: the tests must be self-contained (use the existing fake dynamic client pattern from `client_test.go`).

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/k8s/ -run "TestCreateFRS|TestDeleteFRS|TestWaitForReady" -v`
Expected: compile errors (functions not defined)

- [ ] **Step 3: Implement Create / Delete / Wait in `internal/k8s/frs.go`**

Append to the file:

```go
// FRSpec is the spec for creating a FileRecoverySession.
type FRSpec struct {
    Name             string   // empty → use generateName: "frs-wizard-"
    RestorePointName string   // required
    PVCNames         []string // required, 1+
    SSHUserPublicKey string   // required, authorized_keys format
}

// CreateFRS creates a FileRecoverySession. Returns the FRSView on success.
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
    v, ok := buildFRSView(out)
    if !ok {
        return nil, fmt.Errorf("created FRS %s/%s not in connectable state", ns, out.GetName())
    }
    return &v, nil
}

// DeleteFRS deletes a FileRecoverySession. NotFound is not an error.
func (c *Client) DeleteFRS(ctx context.Context, ns, name string) error {
    err := c.dyn.Resource(FRSGroupVersionResource).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
    if err != nil && !errors.Is(err, &apierrors.StatusError{}) {
        // Check specifically for NotFound; the dynamic client returns
        // an apierrors.StatusError with Reason=NotFound.
        var se *apierrors.StatusError
        if errors.As(err, &se) && se.ErrStatus.Reason == metav1.StatusReasonNotFound {
            return nil
        }
        return fmt.Errorf("delete FRS: %w", err)
    }
    return nil
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
```

Add `"k8s.io/apimachinery/pkg/api/errors"` to imports as `apierrors`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/k8s/ -v`
Expected: all PASS, including pre-existing tests

- [ ] **Step 5: Commit**

```bash
git add internal/k8s/frs.go internal/k8s/frs_test.go
git commit -m "k8s(frs): add CreateFRS, DeleteFRS, WaitForReady + spec type"
```

---

## Task 5: `internal/keymgr/keymgr.go` — TDD

**Files:**
- Create: `internal/keymgr/keymgr.go`
- Create: `internal/keymgr/keymgr_test.go`

`LoadOrGenerate(ctx, k8sClient, ns, name)` is the only exported function. It implements the 4-state decision tree from spec §6.

- [ ] **Step 1: Write the failing tests**

```go
package keymgr

import (
    "context"
    "testing"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes/fake"
)

// k8sClientIface is the subset of kubernetes.Interface we need; the
// real helper passes c.core, the test passes fake.NewSimpleClientset.
type k8sClientIface interface {
    CoreV1() corev1client.CoreV1Interface
}

// We import corev1client through the real k8s pkg; if the import
// causes circular issues, refactor to take a function:
//
//   type SecretGetter func(ctx, ns, name) (*corev1.Secret, error)
//
// and inject the real / fake getter at call sites.

func newSecret(priv, pub string) *corev1.Secret {
    s := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "n"},
        Type:       corev1.SecretTypeSSHAuth,
    }
    s.Data = map[string][]byte{}
    if priv != "" { s.Data["ssh-privatekey"] = []byte(priv) }
    if pub != ""  { s.Data["ssh-publickey"]  = []byte(pub)  }
    return s
}

func TestLoadOrGenerate_NoSecret_GeneratesAndPersists(t *testing.T) {
    // fake clientset with no objects
    cli := fake.NewSimpleClientset()
    m, err := LoadOrGenerate(context.Background(), cli, "n", "k")
    if err != nil { t.Fatal(err) }
    if m.Signer == nil { t.Fatal("signer nil") }
    if len(m.PubKeyPEM) == 0 { t.Fatal("pubkey pem empty") }
    // Verify it was persisted
    got, err := cli.CoreV1().Secrets("n").Get(context.Background(), "k", metav1.GetOptions{})
    if err != nil { t.Fatal(err) }
    if len(got.Data["ssh-privatekey"]) == 0 { t.Error("private key not persisted") }
    if len(got.Data["ssh-publickey"]) == 0 { t.Error("public key not persisted") }
    if got.Type != corev1.SecretTypeSSHAuth { t.Errorf("type = %s", got.Type) }
}

func TestLoadOrGenerate_BothPresent_UseExisting(t *testing.T) {
    // Pre-seed with both. The private key needs to be a valid ed25519 PEM.
    cli := fake.NewSimpleClientset(/* pre-generated valid pair */)
    // Generate a pair to use as the seed:
    seedMgr, _ := LoadOrGenerate(context.Background(), fake.NewSimpleClientset(), "tmp", "tmp")
    cli = fake.NewSimpleClientset(newSecret(string(seedMgr.signerRaw), string(seedMgr.PubKeyPEM)))

    m, err := LoadOrGenerate(context.Background(), cli, "n", "k")
    if err != nil { t.Fatal(err) }
    if m.Signer == nil { t.Fatal("signer nil") }
    if string(m.PubKeyPEM) != string(seedMgr.PubKeyPEM) {
        t.Errorf("pubkey changed; want %q, got %q", seedMgr.PubKeyPEM, m.PubKeyPEM)
    }
    // Should NOT re-write the secret (idempotent — same content).
}

func TestLoadOrGenerate_OnlyPrivate_DerivesPublic(t *testing.T) {
    seedMgr, _ := LoadOrGenerate(context.Background(), fake.NewSimpleClientset(), "tmp", "tmp")
    cli := fake.NewSimpleClientset(newSecret(string(seedMgr.signerRaw), ""))

    m, err := LoadOrGenerate(context.Background(), cli, "n", "k")
    if err != nil { t.Fatal(err) }
    if m.Signer == nil { t.Fatal("signer nil") }
    if len(m.PubKeyPEM) == 0 { t.Fatal("pubkey not derived") }
    // Verify secret was patched with public key
    got, _ := cli.CoreV1().Secrets("n").Get(context.Background(), "k", metav1.GetOptions{})
    if len(got.Data["ssh-publickey"]) == 0 { t.Error("public key not written back") }
}

func TestLoadOrGenerate_OnlyPublic_Fails(t *testing.T) {
    cli := fake.NewSimpleClientset(newSecret("", "ssh-ed25519 AAAA... user@host"))
    _, err := LoadOrGenerate(context.Background(), cli, "n", "k")
    if err == nil { t.Error("expected error when only public key present") }
}
```

Note: `Manager.signerRaw` is an internal field we add for test injection. It holds the raw PEM bytes for re-seeding.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/keymgr/ -v`
Expected: compile error (LoadOrGenerate / Manager undefined)

- [ ] **Step 3: Implement `internal/keymgr/keymgr.go`**

```go
// Package keymgr manages the SSH keypair used by the helper for FRS SFTP auth.
// On startup, LoadOrGenerate reads the configured Secret and either uses
// the existing keypair, derives a missing public key, or generates a fresh
// ed25519 keypair and writes it back.
package keymgr

import (
    "context"
    "crypto/ed25519"
    "crypto/rand"
    "errors"
    "fmt"

    "golang.org/x/crypto/ssh"
    corev1 "k8s.io/api/core/v1"
    apierrors "k8s.io/apimachinery/pkg/api/errors"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
    fieldPrivate = "ssh-privatekey"
    fieldPublic  = "ssh-publickey"
)

// Manager holds the loaded SSH signer and a public-key PEM suitable for
// embedding in FileRecoverySession.spec.transports.sftp.userPublicKey.
type Manager struct {
    Signer    ssh.Signer
    PublicKey ssh.PublicKey
    PubKeyPEM []byte

    // signerRaw is the raw PEM of the private key. Exposed for tests
    // that need to re-seed the fake clientset. Not exported.
    signerRaw []byte
}

// CoreV1Secrets is the subset of corev1client we depend on.
type CoreV1Secrets interface {
    Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error)
    Create(ctx context.Context, s *corev1.Secret, opts metav1.CreateOptions) (*corev1.Secret, error)
    Update(ctx context.Context, s *corev1.Secret, opts metav1.UpdateOptions) (*corev1.Secret, error)
}

// Client is the subset of kubernetes.Interface we depend on.
type Client interface {
    CoreV1() corev1client.CoreV1Interface
}

// LoadOrGenerate reads Secret ns/name; if missing or partial, generates or
// derives to make it complete, then returns the Manager.
func LoadOrGenerate(ctx context.Context, cli Client, ns, name string) (*Manager, error) {
    secrets := cli.CoreV1().Secrets(ns)
    sec, err := secrets.Get(ctx, name, metav1.GetOptions{})

    switch {
    case apierrors.IsNotFound(err):
        // State 1: no secret — generate fresh.
        return generateAndPersist(ctx, secrets, name)
    case err != nil:
        return nil, fmt.Errorf("get secret %s/%s: %w", ns, name, err)
    }

    priv := sec.Data[fieldPrivate]
    pub := sec.Data[fieldPublic]

    switch {
    case len(priv) == 0 && len(pub) == 0:
        return generateAndPersist(ctx, secrets, name)
    case len(priv) == 0:
        return nil, fmt.Errorf("secret %s/%s has public key but no private key; refusing to operate", ns, name)
    case len(pub) == 0:
        // State 3: derive public from private, write back.
        return deriveAndPatch(ctx, secrets, sec, priv)
    default:
        // State 2: both present, use existing.
        return loadFromBytes(priv, pub)
    }
}

func generateAndPersist(ctx context.Context, secrets CoreV1Secrets, name string) (*Manager, error) {
    _, priv, err := ed25519.GenerateKey(rand.Reader)
    if err != nil { return nil, err }
    privPEM, pubPEM, err := marshalEd25519(priv)
    if err != nil { return nil, err }
    sec := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: secretsToNamespace(secrets)},
        Type:       corev1.SecretTypeSSHAuth,
        Data: map[string][]byte{
            fieldPrivate: privPEM,
            fieldPublic:  pubPEM,
        },
    }
    if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil {
        return nil, fmt.Errorf("create secret: %w", err)
    }
    return parseInto(privPEM, pubPEM)
}

func deriveAndPatch(ctx context.Context, secrets CoreV1Secrets, sec *corev1.Secret, priv []byte) (*Manager, error) {
    signer, err := ssh.ParsePrivateKey(priv)
    if err != nil { return nil, fmt.Errorf("parse private key: %w", err) }
    pubPEM := ssh.MarshalAuthorizedKey(signer.PublicKey())
    sec.Data[fieldPublic] = pubPEM
    if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
        return nil, fmt.Errorf("update secret: %w", err)
    }
    return &Manager{Signer: signer, PublicKey: signer.PublicKey(), PubKeyPEM: pubPEM, signerRaw: priv}
}

func loadFromBytes(priv, pub []byte) (*Manager, error) {
    return parseInto(priv, pub)
}

func parseInto(priv, pub []byte) (*Manager, error) {
    signer, err := ssh.ParsePrivateKey(priv)
    if err != nil { return nil, fmt.Errorf("parse private key: %w", err) }
    var pubKey ssh.PublicKey
    if len(pub) > 0 {
        // pub is in authorized_keys format
        k, _, _, _, err := ssh.ParseAuthorizedKey(string(pub))
        if err != nil { return nil, fmt.Errorf("parse public key: %w", err) }
        pubKey = k
    } else {
        pubKey = signer.PublicKey()
    }
    return &Manager{Signer: signer, PublicKey: pubKey, PubKeyPEM: ssh.MarshalAuthorizedKey(pubKey), signerRaw: priv}, nil
}

func marshalEd25519(priv ed25519.PrivateKey) (privPEM, pubPEM []byte, err error) {
    // Use MarshalOpenSSH for OpenSSH-format ed25519 private key
    pemBlock, err := ssh.MarshalPrivateKey(priv, "kasten-frs-web-helper")
    if err != nil { return nil, nil, err }
    privPEM = pem.EncodeToMemory(pemBlock)
    pubKey, ok := priv.Public().(ed25519.PublicKey)
    if !ok { return nil, nil, errors.New("ed25519 public key type assertion failed") }
    sshPub, err := ssh.NewPublicKey(pubKey)
    if err != nil { return nil, nil, err }
    pubPEM = ssh.MarshalAuthorizedKey(sshPub)
    return privPEM, pubPEM, nil
}

func secretsToNamespace(s CoreV1Secrets) string {
    // Trick: we don't have direct access to ns; the caller knows it.
    // In practice generateAndPersist is called with secrets obtained
    // from cli.CoreV1().Secrets(ns) which doesn't round-trip the ns.
    // Add a wrapper or change signature to take ns. Recommended fix:
    // change generateAndPersist to take ns explicitly.
    return ""
}
```

Refactor `generateAndPersist` to take `ns` explicitly to avoid the namespace-from-secrets hack. The signature becomes `generateAndPersist(ctx, secrets, ns, name)`.

Add `"encoding/pem"` to imports.

- [ ] **Step 4: Run tests, iterate until all pass**

Run: `go test ./internal/keymgr/ -v`
Expected: 4 tests PASS (after fixing the namespace-trick refactor)

- [ ] **Step 5: Commit**

```bash
git add internal/keymgr/keymgr.go internal/keymgr/keymgr_test.go
git commit -m "keymgr: LoadOrGenerate handles 4 secret states with TDD coverage"
```

---

## Task 6: Wire `keymgr` into `cmd/helper/main.go`

**Files:**
- Modify: `cmd/helper/main.go`

Replace the existing private-key loading with `keymgr.LoadOrGenerate`. The signer goes to the sftp client; the public key goes to the handlers (for embedding in FRS spec).

- [ ] **Step 1: Read `cmd/helper/main.go`**

The current flow: `LoadPrivateKey` → `parseSigner` → `sftpclient.NewClient{...}`.

New flow: `keymgr.LoadOrGenerate` → `sftpclient.NewClient{Signer: km.Signer}` + pass `km.PubKeyPEM` to handlers.

- [ ] **Step 2: Replace the private-key block**

Replace the block (lines 47-58 in current `main.go`):

```go
// Load private key once at startup
creds, err := kc.LoadPrivateKey(context.Background(), k8s.CredentialsConfig{
    Namespace: cfg.PrivateKeySecretNamespace,
    Name:      cfg.PrivateKeySecretName,
    Field:     cfg.PrivateKeyField,
})
if err != nil {
    return fmt.Errorf("load private key: %w", err)
}
signer, err := parseSigner(creds.PrivateKey)
if err != nil {
    return fmt.Errorf("parse private key: %w", err)
}
```

with:

```go
km, err := keymgr.LoadOrGenerate(context.Background(), kc, cfg.PrivateKeySecretNamespace, cfg.PrivateKeySecretName)
if err != nil {
    return fmt.Errorf("load/generate SSH key: %w", err)
}
```

- [ ] **Step 3: Update the sftpclient + handlers construction**

```go
sftpClient, err := sftpclient.NewClient(sftpclient.ClientConfig{
    Username:       cfg.FRSDefaultUsername, // was creds.Username
    Signer:         km.Signer,
    ConnectTimeout: cfg.SFTPConnectTimeout,
})
```

Then change `handlers.New(...)` to also accept `km.PubKeyPEM` (or expose it via a method on a new Server option):

```go
hs := handlers.New(sessions, pool, kc, km, cfg.FRSDefaultUsername, cfg.FRSPort, cfg.FRSNamespaceWhitelist)
```

- [ ] **Step 4: Update `internal/handlers/handlers.go` `New` signature**

Change the `New` function signature to accept `*keymgr.Manager` instead of (or in addition to) the username string. The Manager exposes `Signer` (already used) and `PubKeyPEM` (new). Update internal references in `Server` struct: store the manager, expose its `PubKeyPEM` to wizard handlers.

The simplest refactor: change the `Server` struct field `username string` to `keymgr *keymgr.Manager` (or keep both). Wizard handlers then read `s.keymgr.PubKeyPEM`.

**Also** add the following fields to the `Server` struct (in `internal/handlers/handlers.go`):

```go
type Server struct {
    // ... existing fields ...
    keymgr          *keymgr.Manager
    watches         *watchMap
    fRSWaitTimeout  time.Duration
    frsProvider     FRSProvider // re-exposed, was `frs FRSProvider` (keep both names for back-compat or rename)
}
```

Initialize them in `New`:

```go
s := &Server{
    // ... existing initializers ...
    keymgr:         km,
    watches:        &watchMap{m: make(map[k8s.FRSRef]*watchState)},
    fRSWaitTimeout: cfg.FRSWaitTimeout, // from config
}
```

`cfg` is the `*config.Config` value passed to `New` (or read from a `WaitTimeout` field added in Task 7). If `New` doesn't currently take `*config.Config`, add a `*config.Config` parameter.

- [ ] **Step 5: Build and test**

Run: `go build ./... && go test ./...`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/helper/main.go internal/handlers/handlers.go
git commit -m "main+handlers: use keymgr for SSH key, expose PubKeyPEM to wizard"
```

---

## Task 7: `internal/config/config.go` — add `FRSWaitTimeout`

**Files:**
- Modify: `internal/config/config.go`

- [ ] **Step 1: Add the field to the `Config` struct**

```go
type Config struct {
    // ... existing fields ...
    FRSWaitTimeout time.Duration
}
```

- [ ] **Step 2: Add the env var load**

In `Load()`, after the other duration loads:

```go
wait, err := time.ParseDuration(getenv("HELPER_FRS_WAIT_TIMEOUT", "30s"))
if err != nil {
    return Config{}, fmt.Errorf("HELPER_FRS_WAIT_TIMEOUT: %w", err)
}
c.FRSWaitTimeout = wait
```

- [ ] **Step 3: Build and test**

Run: `go build ./... && go test ./internal/config/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/config/config.go
git commit -m "config: add HELPER_FRS_WAIT_TIMEOUT (default 30s)"
```

---

## Task 8: `internal/handlers/wizard.go` — 6 handlers + watch map

**Files:**
- Create: `internal/handlers/wizard.go`
- Create: `internal/handlers/wizard_test.go`

Six handlers per spec §4.4. The watch map is a sync.Mutex-protected `map[FRSRef]*watchState`. Each `watchState` is `{state, view, err}` written by the goroutine started in `handleWizardCreate`.

- [ ] **Step 1: Write the failing tests**

In `internal/handlers/wizard_test.go`:

```go
package handlers

import (
    "context"
    "encoding/json"
    "html/template"
    "net/http/httptest"
    "net/url"
    "strings"
    "testing"
    "time"

    "github.com/liguoqiang/kasten-frs-web/internal/auth"
    "github.com/liguoqiang/kasten-frs-web/internal/k8s"
    "github.com/liguoqiang/kasten-frs-web/internal/sftpclient"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newWizardTestServer(t *testing.T, dyn dynInterface) *Server {
    cli := &k8s.Client{...}
    a := auth.NewAuthenticator("u", "p", auth.NewSessionStore([]byte("0123456789abcdef0123456789abcdef"), time.Hour), "k")
    pool := sftpclient.NewPool(nil, time.Hour)
    return New(a, pool, cli, nil /* keymgr */, "root", 2222, nil)
}

func TestHandleWizardPage_Renders(t *testing.T) {
    s := newWizardTestServer(t, nil)
    r := httptest.NewRequest("GET", "/wizard", nil)
    w := httptest.NewRecorder()
    s.handleWizardPage(w, r)
    if w.Code != 200 { t.Errorf("code = %d", w.Code) }
    body := w.Body.String()
    if !strings.Contains(body, "wizard") { t.Errorf("body missing 'wizard'") }
}

// Add tests for handleWizardVMs, handleWizardRPs, handleWizardVolumes,
// handleWizardCreate, handleWizardCancel in the same file.
```

(The full test file is large; write at least one test per handler and commit. Build the rest in steps 2-3.)

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/handlers/ -run "Wizard" -v`
Expected: compile errors (handlers undefined)

- [ ] **Step 3: Implement `internal/handlers/wizard.go`**

```go
package handlers

import (
    "context"
    "fmt"
    "html/template"
    "log/slog"
    "net/http"
    "strings"
    "time"

    "github.com/liguoqiang/kasten-frs-web/internal/k8s"
    "k8s.io/apimachinery/pkg/types"
)

type watchState struct {
    State string        // "Pending" | "Ready" | "Failed" | "Timeout"
    View  k8s.FRSView
    Err   error
    Done  bool
}

// server-with-watches extends Server with an in-memory watch map.
func (s *Server) watchMap() *watchMap { /* see below */ }

type watchMap struct {
    mu sync.Mutex
    m  map[k8s.FRSRef]*watchState
}

// In handlers.go, add a field to Server:
//   watches *watchMap
// Initialize in New: s.watches = &watchMap{m: make(map[k8s.FRSRef]*watchState)}

func (s *Server) handleWizardPage(w http.ResponseWriter, r *http.Request) {
    vms, err := s.frsListVMs(r.Context())
    if err != nil {
        s.renderError(w, http.StatusBadGateway, "VM 列表拉取失败", err.Error())
        return
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
        "Title":        "恢复向导",
        "BodyTemplate": "wizard_body",
        "VMs":          vms,
        "User":         s.auth.Username,
    }); err != nil {
        slog.Error("render wizard", "err", err)
    }
}

func (s *Server) handleWizardVMs(w http.ResponseWriter, r *http.Request) {
    // Re-render just the VM panel fragment for htmx
    vms, err := s.frsListVMs(r.Context())
    if err != nil { s.renderError(w, http.StatusBadGateway, "VM 列表拉取失败", err.Error()); return }
    if err := pageTemplates.ExecuteTemplate(w, "wizard_vms_fragment", map[string]any{"VMs": vms}); err != nil {
        slog.Error("render wizard vms", "err", err)
    }
}

func (s *Server) handleWizardRPs(w http.ResponseWriter, r *http.Request) {
    ns := r.PathValue("ns")
    name := r.PathValue("name")
    rps, err := s.frsListRPs(r.Context(), ns, name)
    if err != nil { s.renderError(w, http.StatusBadGateway, "RP 列表拉取失败", err.Error()); return }
    if err := pageTemplates.ExecuteTemplate(w, "wizard_rps_fragment", map[string]any{"RPs": rps, "VM": name}); err != nil {
        slog.Error("render wizard rps", "err", err)
    }
}

func (s *Server) handleWizardVolumes(w http.ResponseWriter, r *http.Request) {
    ns := r.PathValue("ns")
    name := r.PathValue("name")
    arts, err := s.frsListVolumes(r.Context(), ns, name)
    if err != nil { s.renderError(w, http.StatusBadGateway, "Volume 列表拉取失败", err.Error()); return }
    if err := pageTemplates.ExecuteTemplate(w, "wizard_vols_fragment", map[string]any{"Vols": arts, "RP": name}); err != nil {
        slog.Error("render wizard vols", "err", err)
    }
}

func (s *Server) handleWizardCreate(w http.ResponseWriter, r *http.Request) {
    if err := r.ParseForm(); err != nil {
        s.renderError(w, http.StatusBadRequest, "表单错误", err.Error()); return
    }
    vmNs := r.FormValue("vmNs")
    vmName := r.FormValue("vmName")
    rpName := r.FormValue("rpName")
    pvcNames := r.Form["pvcNames"]
    if vmNs == "" || vmName == "" || rpName == "" || len(pvcNames) == 0 {
        s.renderError(w, http.StatusBadRequest, "参数不完整", "vmNs, vmName, rpName, pvcNames 必填"); return
    }
    pubKey := ""
    if s.keymgr != nil { pubKey = string(s.keymgr.PubKeyPEM) }
    if pubKey == "" {
        s.renderError(w, http.StatusInternalServerError, "无 SSH 公钥", "helper 启动时未加载 SSH 公钥"); return
    }
    view, err := s.frsCreate(r.Context(), vmNs, k8s.FRSpec{
        RestorePointName: rpName, PVCNames: pvcNames, SSHUserPublicKey: pubKey,
    })
    if err != nil { s.renderError(w, http.StatusBadGateway, "创建 FRS 失败", err.Error()); return }
    ref := view.Ref
    s.watches.set(ref, &watchState{State: "Pending", View: *view})
    go s.watchFRSCreated(ref, view)
    http.Redirect(w, r, "/browse?frs="+ref.Namespace+"/"+ref.Name+"&path=/", http.StatusSeeOther)
}

func (s *Server) watchFRSCreated(ref k8s.FRSRef, initial k8s.FRSView) {
    timeout := 30 * time.Second
    if s.fRSWaitTimeout != nil { timeout = *s.fRSWaitTimeout }
    v, err := s.frsWaitReady(context.Background(), ref, timeout)
    state := &watchState{View: v}
    if err != nil {
        if v.State == "Failed" { state.State = "Failed" } else { state.State = "Timeout" }
        state.Err = err
    } else {
        state.State = "Ready"
    }
    state.Done = true
    s.watches.set(ref, state)
}

func (s *Server) handleWizardCancel(w http.ResponseWriter, r *http.Request) {
    if err := r.ParseForm(); err != nil { s.renderError(w, http.StatusBadRequest, "表单错误", err.Error()); return }
    frs := r.FormValue("frs")
    parts := strings.SplitN(frs, "/", 2)
    if len(parts) != 2 { s.renderError(w, http.StatusBadRequest, "frs 参数错误", frs); return }
    if err := s.frsDelete(r.Context(), parts[0], parts[1]); err != nil {
        s.renderError(w, http.StatusBadGateway, "取消失败", err.Error()); return
    }
    s.watches.del(k8s.FRSRef{Namespace: parts[0], Name: parts[1]})
    http.Redirect(w, r, "/wizard", http.StatusSeeOther)
}
```

The helpers `s.frsListVMs`, `s.frsListRPs`, `s.frsListVolumes`, `s.frsCreate`, `s.frsDelete`, `s.frsWaitReady` are thin method-forwarders to the `*k8s.Client` (kept on `Server` to centralize the FRSProvider interface). Define them in `handlers.go`.

- [ ] **Step 4: Update `FRSProvider` interface in `handlers.go`**

Add the new methods to the existing `FRSProvider` interface so tests can mock them:

```go
type FRSProvider interface {
    ListActiveFRS(ctx, namespaces) ([]k8s.FRSView, error)
    GetFRS(ctx, ref) (k8s.FRSView, error)
    ListVMs(ctx, namespaces) ([]k8s.VM, error)
    ListRestorePoints(ctx, ns, appName) ([]k8s.RestorePoint, error)
    GetRestorePointDetails(ctx, ns, name) ([]k8s.VolumeArtifact, error)
    CreateFRS(ctx, ns string, spec k8s.FRSpec) (*k8s.FRSView, error)
    DeleteFRS(ctx, ns, name string) error
    WaitForReady(ctx, ns, name string, timeout time.Duration) (k8s.FRSView, error)
}
```

`*k8s.Client` already satisfies all of these after Task 3 + Task 4.

- [ ] **Step 4: Add `watchMap` methods + method-forwarders to `*k8s.Client`**

`watchMap` (defined in step 3) needs `get`/`set`/`del` methods. Add to `wizard.go`:

```go
func (wm *watchMap) get(ref k8s.FRSRef) (*watchState, bool) {
    wm.mu.Lock(); defer wm.mu.Unlock()
    s, ok := wm.m[ref]
    return s, ok
}
func (wm *watchMap) set(ref k8s.FRSRef, s *watchState) {
    wm.mu.Lock(); defer wm.mu.Unlock()
    wm.m[ref] = s
}
func (wm *watchMap) del(ref k8s.FRSRef) {
    wm.mu.Lock(); defer wm.mu.Unlock()
    delete(wm.m, ref)
}
```

The wizard handlers in step 3 call `s.frsListVMs`, `s.frsListRPs`,
`s.frsListVolumes`, `s.frsCreate`, `s.frsDelete`, `s.frsWaitReady`,
`s.frsGet` — these are thin pass-throughs to the `*k8s.Client`.
Add to `handlers.go` as private methods on `*Server`:

```go
func (s *Server) frsListVMs(ctx context.Context) ([]k8s.VM, error) {
    return s.frs.ListVMs(ctx, s.nsWhitelist)
}
func (s *Server) frsListRPs(ctx context.Context, ns, appName string) ([]k8s.RestorePoint, error) {
    return s.frs.ListRestorePoints(ctx, ns, appName)
}
func (s *Server) frsListVolumes(ctx context.Context, ns, name string) ([]k8s.VolumeArtifact, error) {
    return s.frs.GetRestorePointDetails(ctx, ns, name)
}
func (s *Server) frsGet(ctx context.Context, ref k8s.FRSRef) (k8s.FRSView, error) {
    return s.frs.GetFRS(ctx, ref)
}
func (s *Server) frsCreate(ctx context.Context, ns string, spec k8s.FRSpec) (*k8s.FRSView, error) {
    return s.frs.CreateFRS(ctx, ns, spec)
}
func (s *Server) frsDelete(ctx context.Context, ns, name string) error {
    return s.frs.DeleteFRS(ctx, ns, name)
}
func (s *Server) frsWaitReady(ctx context.Context, ref k8s.FRSRef, timeout time.Duration) (k8s.FRSView, error) {
    return s.frs.WaitForReady(ctx, ref.Namespace, ref.Name, timeout)
}
```

This keeps the `FRSProvider` interface in `handlers.go` decoupled from
the concrete `*k8s.Client` and makes mocking easier in tests.

- [ ] **Step 6: Register routes in `handlers.go routes()`**

```go
authed.HandleFunc("GET /wizard", s.handleWizardPage)
authed.HandleFunc("GET /wizard/vms", s.handleWizardVMs)
authed.HandleFunc("GET /wizard/vms/{ns}/{name}/restorepoints", s.handleWizardRPs)
authed.HandleFunc("GET /wizard/rps/{ns}/{name}/details", s.handleWizardVolumes)
authed.HandleFunc("POST /wizard/create", s.handleWizardCreate)
authed.HandleFunc("POST /wizard/cancel", s.handleWizardCancel)
```

- [ ] **Step 7: Build and test**

Run: `go build ./... && go test ./internal/handlers/...`
Expected: PASS (template errors are OK; we'll fix in Task 9)

- [ ] **Step 8: Commit**

```bash
git add internal/handlers/wizard.go internal/handlers/wizard_test.go internal/handlers/handlers.go
git commit -m "wizard: 6 handlers + in-memory watch map + FRSProvider extensions"
```

---

## Task 9: `web/templates/wizard.html` — master-detail layout

**Files:**
- Create: `web/templates/wizard.html`

The template defines 4 named sub-templates: `wizard_body` (full page), `wizard_vms_fragment`, `wizard_rps_fragment`, `wizard_vols_fragment`. htmx targets the 3 fragment templates.

- [ ] **Step 1: Create the template file**

`web/templates/wizard.html`:

```html
{{define "wizard_body"}}<div class="wizard-stage">
  <div class="workarea-title">恢复向导</div>
  <div class="workarea-subtitle">选 VM → 选还原点 → 选 volume → 创建 FRS</div>

  <div class="wizard-grid">
    <section class="wizard-panel" id="panel-vms">
      <div class="panel-title">第 1 步：选虚拟机</div>
      <input type="search" id="vm-filter" placeholder="搜索 VM 名…" autocomplete="off">
      <ul class="vm-list" id="vm-list">
        {{template "wizard_vms_fragment" .}}
      </ul>
    </section>

    <section class="wizard-panel" id="panel-rps">
      <div class="panel-title">第 2 步：选还原点</div>
      <div id="rp-list">
        <p class="empty">从左侧选一个 VM</p>
      </div>
    </section>

    <section class="wizard-panel" id="panel-vols">
      <div class="panel-title">第 3 步：选 volume</div>
      <div id="vol-list">
        <p class="empty">从中间选一个还原点</p>
      </div>
    </section>
  </div>

  <form method="post" action="/wizard/create" class="wizard-create">
    <input type="hidden" name="vmNs"   id="vm-ns">
    <input type="hidden" name="vmName" id="vm-name">
    <input type="hidden" name="rpName" id="rp-name">
    <div id="pvc-fields"></div>
    <button type="submit" id="wizard-submit" disabled>创建 FRS</button>
  </form>
</div>

<script>
(function(){
  const $ = (s, r=document) => r.querySelector(s);
  const $$ = (s, r=document) => Array.from(r.querySelectorAll(s));

  // Client-side VM filter
  const filter = $('#vm-filter');
  if (filter) {
    filter.addEventListener('input', () => {
      const q = filter.value.toLowerCase();
      $$('#vm-list li').forEach(li => {
        const name = li.dataset.vmName || '';
        li.style.display = name.toLowerCase().includes(q) ? '' : 'none';
      });
    });
  }

  // VM click → load RPs
  document.body.addEventListener('click', e => {
    const li = e.target.closest('#vm-list li');
    if (!li) return;
    $$('#vm-list li').forEach(x => x.classList.remove('selected'));
    li.classList.add('selected');
    const ns = li.dataset.vmNs, name = li.dataset.vmName;
    $('#vm-ns').value = ns; $('#vm-name').value = name;
    $('#rp-name').value = ''; $('#pvc-fields').innerHTML = '';
    $('#wizard-submit').disabled = true;
    htmx.ajax('GET', '/wizard/vms/'+encodeURIComponent(ns)+'/'+encodeURIComponent(name)+'/restorepoints', {target:'#rp-list', swap:'innerHTML'});
  });

  // RP click → load Volumes
  document.body.addEventListener('htmx:afterRequest', e => {
    if (e.target.id === 'rp-list') {
      // delegate next click on rp-list items
      $('#rp-list').addEventListener('click', e2 => {
        const li = e2.target.closest('li');
        if (!li) return;
        $$('#rp-list li').forEach(x => x.classList.remove('selected'));
        li.classList.add('selected');
        const name = li.dataset.rpName;
        $('#rp-name').value = name;
        htmx.ajax('GET', '/wizard/rps/'+encodeURIComponent(li.dataset.rpNs)+'/'+encodeURIComponent(name)+'/details', {target:'#vol-list', swap:'innerHTML'});
      }, {once: true});
    }
    if (e.target.id === 'vol-list') {
      // Re-enable submit when at least one PVC checkbox is selected
      const any = $$('#vol-list input[name=pvcNames]').length > 0;
      $('#wizard-submit').disabled = !any;
      // Mirror checkbox state into hidden form fields
      $$('#vol-list input[name=pvcNames]').forEach(cb => {
        cb.addEventListener('change', () => {
          const checked = $$('#vol-list input[name=pvcNames]:checked').map(x => x.value);
          $('#pvc-fields').innerHTML = checked.map(v => `<input type="hidden" name="pvcNames" value="${v}">`).join('');
        });
      });
    }
  });
})();
</script>{{end}}

{{define "wizard_vms_fragment"}}
{{range .VMs}}
<li class="vm-item" data-vm-name="{{.AppName}}" data-vm-ns="{{.AppNamespace}}">
  <div class="vm-name"><strong>{{.AppName}}</strong></div>
  <div class="vm-meta">
    <span class="ns">{{.AppNamespace}}</span>
    <span class="rp-count">{{.RPCount}} 个 RP</span>
    {{if not .LastRPTime.IsZero}}<span class="last">最近 {{.LastRPTime.Format "2006-01-02 15:04"}}</span>{{end}}
  </div>
</li>
{{else}}
<li class="empty">暂无可恢复的 VM。确认 K10 有 <code>appType=virtualMachine</code> 的 RestorePoint。</li>
{{end}}{{end}}

{{define "wizard_rps_fragment"}}
{{$vm := .VM}}
{{range .RPs}}
<li class="rp-item" data-rp-name="{{.Name}}" data-rp-ns="{{.Namespace}}">
  <div class="rp-name"><strong>{{.Name}}</strong></div>
  <div class="rp-meta">
    <span class="badge {{.State | lower}}">{{.State}}</span>
    <span class="time">{{.CreatedAt.Format "2006-01-02 15:04:05"}}</span>
  </div>
</li>
{{else}}
<li class="empty">这个 VM 没有 Bound 的还原点。</li>
{{end}}{{end}}

{{define "wizard_vols_fragment"}}
{{$rp := .RP}}
<form id="vol-form">
{{range .Vols}}
<label class="vol-item">
  <input type="checkbox" name="pvcNames" value="{{.PVCName}}" checked>
  <span class="pvc-name">{{.PVCName}}</span>
  {{if .Size}}<span class="size">{{.Size}}</span>{{end}}
</label>
{{else}}
<p class="empty">这个还原点没有 PVC artifacts。</p>
{{end}}
</form>{{end}}
```

- [ ] **Step 2: Add CSS for the wizard in `web/static/veeam-theme.css`**

Append to the file:

```css
.wizard-grid {
  display: grid;
  grid-template-columns: 1fr 1fr 1fr;
  gap: 12px;
  margin: 16px 0;
}
.wizard-panel {
  background: #fff;
  border: 1px solid #D5DBE0;
  border-radius: 4px;
  padding: 12px;
  min-height: 320px;
}
.panel-title {
  font-weight: 600;
  color: #4A5560;
  font-size: 13px;
  margin-bottom: 8px;
}
.vm-list, #rp-list, #vol-list {
  list-style: none;
  margin: 0; padding: 0;
  max-height: 360px; overflow-y: auto;
}
.vm-item, .rp-item {
  padding: 6px 8px;
  border-radius: 3px;
  cursor: pointer;
  border: 1px solid transparent;
  margin-bottom: 4px;
}
.vm-item:hover, .rp-item:hover { background: #F0F3F6; }
.vm-item.selected, .rp-item.selected {
  background: #E1F0FF; border-color: #6FA8DC;
}
.vm-meta, .rp-meta { font-size: 11px; color: #6B7480; }
.wizard-create {
  background: #fff;
  padding: 12px;
  border-top: 1px solid #D5DBE0;
  border-radius: 4px;
  margin-top: 12px;
}
.wizard-create button[disabled] { opacity: 0.5; cursor: not-allowed; }
.vol-item { display: block; padding: 4px 0; }
.empty { color: #8A95A2; font-style: italic; padding: 12px; }
```

- [ ] **Step 3: Build and run**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add web/templates/wizard.html web/static/veeam-theme.css
git commit -m "wizard: master-detail template + htmx wiring + CSS"
```

---

## Task 10: `web/templates/sessions.html` — countdown + delete column

**Files:**
- Modify: `web/templates/sessions.html`

- [ ] **Step 1: Replace the table to add countdown + delete column**

Replace the existing `<table class="simple-table">…</table>` body in `sessions.html`:

```html
{{define "sessions_body"}}<table class="simple-table">
  <thead>
    <tr>
      <th style="width:80px">State</th>
      <th style="width:22%">FRS Name</th>
      <th style="width:14%">Namespace</th>
      <th>剩余</th>
      <th style="width:18%; text-align:right; padding-right:18px">Action</th>
    </tr>
  </thead>
  <tbody>
    {{range .FRS}}
    <tr>
      <td><span class="badge {{.State | lower}}">{{.State}}</span></td>
      <td><strong>{{.Ref.Name}}</strong></td>
      <td>{{.Ref.Namespace}}</td>
      <td>
        <span class="badge" data-expiry="{{.ExpiryTime.Format "2006-01-02T15:04:05Z07:00"}}">
          <span class="exp-text">…</span>
        </span>
      </td>
      <td class="action" style="text-align:right">
        <form method="post" action="/sessions/{{.Ref.Namespace}}/{{.Ref.Name}}/connect" style="display:inline">
          <button type="submit">浏览 ›</button>
        </form>
        <form method="post" action="/sessions/{{.Ref.Namespace}}/{{.Ref.Name}}/delete" style="display:inline"
              onsubmit="return confirm('结束并删除 FRS {{.Ref.Namespace}}/{{.Ref.Name}}?')">
          <button type="submit" class="danger">结束</button>
        </form>
      </td>
    </tr>
    {{else}}
    <tr><td colspan="5" style="text-align:center; padding:30px; color:#6B7480; font-style:italic">暂无可用 FRS 会话</td></tr>
    {{end}}
  </tbody>
</table>

<script>
(function(){
  if (window.__kfrsCountdown) return;
  window.__kfrsCountdown = true;
  function fmt(ms){
    if (ms < 0) return '已过期';
    const s = Math.floor(ms/1000);
    const d = Math.floor(s/86400), h = Math.floor((s%86400)/3600), m = Math.floor((s%3600)/60);
    if (d > 0) return '剩 '+d+'d '+(h<10?'0':'')+h+'h';
    return '剩 '+(h<10?'0':'')+h+'h '+(m<10?'0':'')+m+'m';
  }
  function tick(){
    const now = Date.now();
    document.querySelectorAll('[data-expiry]').forEach(el => {
      const t = Date.parse(el.dataset.expiry);
      const ms = t - now;
      const span = el.querySelector('.exp-text');
      if (ms < 0) { el.className = 'badge crit'; span.textContent = '已过期'; return; }
      if (ms < 15*60*1000) el.className = 'badge crit';
      else if (ms < 60*60*1000) el.className = 'badge warn';
      else el.className = 'badge';
      span.textContent = fmt(ms);
    });
  }
  tick();
  let id = setInterval(tick, 1000);
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) { clearInterval(id); id = null; }
    else if (!id) { tick(); id = setInterval(tick, 1000); }
  });
})();
</script>{{end}}
```

- [ ] **Step 2: Add CSS for `.badge.crit`, `.badge.warn` if missing**

In `web/static/veeam-theme.css`, ensure these exist (append if not):

```css
.badge.warn { background: #FFF4D6; color: #8A6A0A; border-color: #E6C97A; }
.badge.crit { background: #FFE0E0; color: #B02020; border-color: #D97070; }
button.danger { background: #B02020; color: #fff; border-color: #B02020; }
button.danger:hover { background: #D03030; }
```

- [ ] **Step 3: Build and test**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add web/templates/sessions.html web/static/veeam-theme.css
git commit -m "sessions: countdown + delete button + warn/crit badge colors"
```

---

## Task 11: `internal/handlers/sessions.go` — delete handler + `web/templates/browse.html` delete + preparing

**Files:**
- Create: `internal/handlers/sessions.go`
- Modify: `web/templates/browse.html`
- Modify: `web/templates/layout.html` (to render the preparing branch)

- [ ] **Step 1: Add `handleSessionDelete`**

Create `internal/handlers/sessions.go`:

```go
package handlers

import (
    "net/http"
)

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
    ns := r.PathValue("ns")
    name := r.PathValue("name")
    if err := s.frsDelete(r.Context(), ns, name); err != nil {
        s.renderError(w, http.StatusBadGateway, "删除 FRS 失败", err.Error())
        return
    }
    // Close any pooled SFTP session for this FRS
    s.pool.CloseAllForFRS(ns, name) // add this method to sftpclient.Pool if absent
    http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}
```

If `s.pool.CloseAllForFRS` doesn't exist, add a minimal variant to `internal/sftpclient/pool.go`:

```go
func (p *Pool) CloseAllForFRS(ns, name string) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for k, e := range p.entries {
        if k.FRS.Namespace == ns && k.FRS.Name == name {
            delete(p.entries, k)
            go e.sess.Close()
        }
    }
}
```

- [ ] **Step 2: Register the route in `handlers.go`**

```go
authed.HandleFunc("POST /sessions/{ns}/{name}/delete", s.handleSessionDelete)
```

- [ ] **Step 3: Modify `web/templates/browse.html` — add delete button + preparing body**

At the top of `browse_body` add the delete button, and append a new `browse_preparing_body` definition:

```html
{{define "browse_body"}}<div>
  <div class="browse-actions">
    <form method="post" action="/sessions/{{.FRS.Namespace}}/{{.FRS.Name}}/delete"
          onsubmit="return confirm('结束并删除 FRS {{.FRS.Namespace}}/{{.FRS.Name}}?')">
      <button type="submit" class="danger">结束并删除 FRS</button>
    </form>
  </div>
  <table class="filelist">
  ... existing table ...
  </table>
</div>{{end}}

{{define "browse_preparing_body"}}<div class="browse-preparing">
  <h1>⏳ FRS 正在准备</h1>
  <p>{{.FRS.Namespace}}/{{.FRS.Name}} 状态：<span class="badge {{.State | lower}}">{{.State}}</span></p>
  <p>等待时间：<span id="elapsed">0</span> 秒</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <form method="post" action="/wizard/cancel">
    <input type="hidden" name="frs" value="{{.FRS.Namespace}}/{{.FRS.Name}}">
    <button type="submit" class="danger">取消并删除 FRS</button>
  </form>
  <div hx-get="/browse?frs={{.FRS.Namespace}}/{{.FRS.Name}}&path=/&partial=ready"
       hx-trigger="every 2s" hx-swap="outerHTML" style="display:none"></div>
</div>
<script>
(function(){
  const start = Date.now();
  const el = document.getElementById('elapsed');
  if (el) setInterval(() => { el.textContent = Math.floor((Date.now()-start)/1000); }, 1000);
})();
</script>{{end}}
```

- [ ] **Step 4: Update `handleBrowse` in `handlers.go` to dispatch on state and `partial` param**

Replace the `handleBrowse` body (around lines 234-264 in current `handlers.go`):

```go
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
    ref, path, err := parseFRSQuery(r)
    if err != nil { s.renderError(w, http.StatusBadRequest, "无效的 frs 查询", err.Error()); return }

    // Check watch map first
    if ws, ok := s.watches.get(ref); ok && ws.Done {
        if ws.State == "Ready" {
            // fall through to normal browse
        } else {
            // Failed / Timeout
            s.renderPreparing(w, ref, ws)
            return
        }
    }

    key := sftpclient.SessionKey{UserSessionID: userIDFromCookie(r, s.auth.CookieName),
        FRS: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}
    sess, ok := s.pool.Get(key)
    if !ok {
        // Need to (re)connect; check FRS state
        view, err := s.frsGet(r.Context(), ref)
        if err != nil {
            // Maybe still starting — render preparing if map says so
            if ws, ok := s.watches.get(ref); ok {
                s.renderPreparing(w, ref, ws); return
            }
            s.renderError(w, http.StatusBadGateway, "FRS 查询失败", err.Error()); return
        }
        if view.State != "Ready" {
            s.renderPreparing(w, ref, &watchState{State: view.State, View: view})
            return
        }
        http.Redirect(w, r, fmt.Sprintf("/sessions/%s/%s/connect", ref.Namespace, ref.Name), http.StatusSeeOther)
        return
    }

    entries, err := sess.ListDir(path)
    if err != nil { s.renderError(w, http.StatusNotFound, "目录列表失败", err.Error()); return }

    if r.URL.Query().Get("partial") == "ready" {
        // htmx polling: return just the filelist HTML fragment
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        if err := pageTemplates.ExecuteTemplate(w, "browse_filelist_fragment", map[string]any{
            "FRS": ref, "Path": path, "Entries": entries, "User": s.auth.Username,
        }); err != nil { slog.Error("render browse fragment", "err", err) }
        return
    }

    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
        "Title": "浏览 " + ref.Namespace + "/" + ref.Name, "BodyTemplate": "browse_body",
        "FRS": ref, "Path": path, "Entries": entries, "User": s.auth.Username,
    }); err != nil { slog.Error("render browse", "err", err) }
}

func (s *Server) renderPreparing(w http.ResponseWriter, ref k8s.FRSRef, ws *watchState) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
        "Title": "FRS 准备中", "BodyTemplate": "browse_preparing_body",
        "FRS": ref, "State": ws.State, "Error": errString(ws.Err), "User": s.auth.Username,
    }); err != nil { slog.Error("render preparing", "err", err) }
}

func errString(e error) string {
    if e == nil { return "" }
    return e.Error()
}
```

Also add to `browse.html` a `browse_filelist_fragment` template that
just renders the `<table class="filelist">` body — extract the
existing table from `browse_body` into a new `browse_filelist_fragment`
template and have `browse_body` `{{template "browse_filelist_fragment" .}}` it.
That way the same HTML is used for both full page render and the
`partial=ready` htmx swap.

- [ ] **Step 5: Build and test**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/handlers/sessions.go internal/handlers/handlers.go internal/sftpclient/pool.go web/templates/browse.html
git commit -m "browse+sessions: delete button, preparing body, partial=ready polling"
```

---

## Task 12: `docs/superpowers/experience/2026-06-16-deploy-experience.md` — experience doc

**Files:**
- Create: `docs/superpowers/experience/2026-06-16-deploy-experience.md`

- [ ] **Step 1: Create the experience doc**

```markdown
# kasten-frs-web 部署 / 测试经验（2026-06-16）

## 1. OCP `restricted-v2` SCC + 命名空间 UID range 冲突

- OCP `restricted-v2` SCC 有 `runAsUser.type=MustRunAsRange`，要求
  显式 runAsUser 落在命名空间分配的 UID range 内。
- 仓库默认命名空间 `kasten-io` 在我们这个集群里 range 是
  `1001100000/10000`。
- deploy/20-deployment.yaml 早期 pin 了 `runAsUser: 1001`，落在
  range 之外，被准入拒绝，pod 起不来。修法：去掉 runAsUser pin（让
  SCC 从 range 里挑），加 `fsGroup: 1001100000` 让 emptyDir 可写。
  详见 commit `2aed67f`。

## 2. K10 default-deny NetworkPolicy（selector={}）

- K10 控制平面装好后，会自动注入一个 selector={} 的 NetworkPolicy，
  policyTypes=[Ingress]，**默认**拒绝所有进入 kasten-io pod 的流量
  （包括我们自己部署的 helper）。
- 即使我们的 deploy/50-networkpolicy.yaml 显式写了
  port-8080 ingress from openshift-ingress 也不行 —— 因为 OVN-K
  处理 K10 注入的 default-deny 时把所有后续规则都吞了。
- 解法：用 Kustomize 在 overlay 里 `op: replace` 把整个 `spec/egress`
  替换成 `[{to: [{}]}]`（放行所有出站），`spec/ingress` 替换为允许
  openshift-ingress 命名空间到 8080 的规则。详见
  `scripts/deploy-test.sh` 注释 + commit `dcee3e1`。
- 旁路：K10 pod 自己也跑在 kasten-io，它们的 netpol 状态是
  "Ingress-only"（无 egress 规则）—— 我们也照此办理。

## 3. FRS SFTP pod 的自动 NetworkPolicy

- K10 datamover 每次创建 FRS 时会同步生成一个 NetworkPolicy
  `frs-XXXXX`，只允许**源 namespace=default** 的流量到 FRS pod 的
  2222。helper pod 在 kasten-io，被这个 netpol 拒绝。
- 解法：在 overlay 加一个 ingress 规则：from helper podSelector 到
  FRS podSelector 2222。K10 的 netpol 不会限制 ingress 源集合的合并
  —— 多个 netpol 的规则是并集。
- 详见 `scripts/deploy-test.sh` 的 frs-allow.yaml 段。

## 4. GHCR 匿名拉镜像

- `kubectl get pods` 等到 ImagePullBackOff，根因往往是 ghcr.io
  匿名访问要走 OCI Accept 头：
  ```
  Accept: application/vnd.oci.image.index.v1+json
  ```
  + 先取 bearer token（`/v2/.../manifests/...` 401 响应里
  `WWW-Authenticate: Bearer realm=...`），再用 token 调 manifest。
- 详见 `scripts/deploy-test.sh` preflight 段。

## 5. FRS 状态机

```
Starting → Processing → Ready
                    ↘  Failed
```

- `Ready` 之前 `status.transports.sftp.{serviceName, portNumber,
  hostKeySignature}` 都不可读（空字符串或缺失）。
- helper 必须在 `Ready` 之后才能 SFTP dial；早期 dial 会拿到
  `connection refused` 或 `ssh: handshake failed`。
- K10 helm chart 的 `frs.sessionExpiryTimeInMinutes`（默认 60min）
  是 FRS 存活上限；超时会自动清理 pod + service + netpol。
- helper 端在 `WaitForReady` 之上加**内存 watch map**（spec §4.4）
  是为了避免在 HTTP handler 里同步等 30s+ 触发 HAProxy 默认 30s
  request timeout。

## 6. e2e 网络验证三件套

helper pod 起来后用 `oc exec` 跑这三个就能定位大多数网络问题：

```
# DNS
oc -n kasten-io exec $POD -- nslookup kubernetes.default
# K8s API
oc -n kasten-io exec $POD -- curl -sk https://kubernetes.default.svc/api
# FRS SFTP（替换 frs-xxx）
oc -n kasten-io exec $POD -- bash -c "timeout 3 bash -c '</dev/tcp/frs-xxx.kasten-io.svc.cluster.local/2222'"
```

不用 `nslookup` + `curl` 完整包名：直接 `getent hosts
kubernetes.default` 在某些精简镜像里也行。

## 7. Route edge TLS 默认 30s/300s 响应超时

- OCP Route edge termination 默认 `haproxy.router.openshift.io/timeout=300s`
  （响应）+ 30s（请求）。
- 10G+ 文件下载通常超过 300s，需要在 Route 上加 annotation：
  ```
  haproxy.router.openshift.io/timeout: 86400s
  ```
  body 缓冲 `buffer-size` 默认 0=无限制，不用调。
- 另：SFTP 池 idle TTL `HELPER_SFTP_TTL` 默认 30min；超过 30min
  的下载 helper 端 SFTP 会断，需要重连。10G 量级建议设
  `HELPER_SFTP_TTL=2h`。
```

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/experience/2026-06-16-deploy-experience.md
git commit -m "docs: deploy/test experience notes (SCC, netpol, FRS, ghcr, timeouts)"
```

---

## Task 13: Docs update — README / DEPLOY / CHANGELOG

**Files:**
- Modify: `README.md`
- Modify: `DEPLOY.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Add "Wizard" section to `README.md`**

After the existing "Features" list, add:

```markdown
## Wizard (v0.3.0+)

`/wizard` provides a single-page recovery flow:

1. Pick a VM (filtered by `k10.kasten.io/appType=virtualMachine` label)
2. Pick a Bound RestorePoint
3. Pick the PVCs to include
4. Click **Create FRS**

The helper:
- generates or derives the SSH keypair on first start (no operator
  `ssh-keygen` step required)
- creates the FileRecoverySession via the K8s dynamic client
- polls the FRS state in-memory; the user sees a "preparing" page
  until state=Ready, then is redirected to the directory listing
```

- [ ] **Step 2: Update `DEPLOY.md` — drop SSH key step, add wizard smoke**

Remove the entire "Generate a single keypair..." section (steps 3-6 of the current pre-flight). Replace with:

```markdown
## Pre-flight

1. OCP >= 4.11
2. Kasten K10 with `filerecoverysessions.datamover.kio.kasten.io` CRD installed
3. Generate Helper credentials and cookie secret (each >= 16 bytes):
   ```bash
   PW=$(openssl rand -base64 24)
   CS=$(openssl rand -base64 32)
   ```
4. Create `kasten-frs-web-helper-credentials` Secret with the three values
   (`HELPER_USERNAME`, `HELPER_PASSWORD`, `HELPER_COOKIE_SECRET`).

The helper will auto-generate and persist the SSH keypair on first start.
The public key is embedded in every FRS the wizard creates; the private
key never leaves the helper pod.
```

In "Post-flight verification", add at the end:

```markdown
## Wizard smoke

After the helper pod is Ready, log in via the Route and navigate to
`/wizard`. You should see at least one VM (assuming K10 has a
`virtualMachine`-labelled RestorePoint). Pick a VM, then a Bound RP,
then any volume, and click **Create FRS**. You should be redirected
to `/browse` showing the FRS directory tree within 30 seconds.
```

- [ ] **Step 3: Add v0.3.0 entry to `CHANGELOG.md`**

Prepend (or insert at top):

```markdown
## 0.3.0 (2026-06-16)

- Web-based recovery wizard (VM → restore point → volume → FRS)
- SSH keypair auto-managed by the helper (no operator `ssh-keygen`)
- FRS ready polling is async (no HTTP-blocking waits)
- Sessions page shows expiry countdown + color (warn < 1h, crit < 15m)
- "End and delete" button on sessions and browse pages
- Deploy-experience doc captures SCC, NetworkPolicy, ghcr, and
  FRS state-machine lessons
```

- [ ] **Step 4: Commit**

```bash
git add README.md DEPLOY.md CHANGELOG.md
git commit -m "docs: README wizard section, DEPLOY drops SSH key step, CHANGELOG v0.3.0"
```

---

## Task 14: `scripts/wizard-test.sh` — fake-client e2e (gitignored)

**Files:**
- Modify: `.gitignore` (add `scripts/wizard-test.sh`? No — make it a checked-in file, the user runs it)
- Create: `scripts/wizard-test.sh`

The e2e script uses the existing fake K8s client (`k8s.Client{Fake: true}`) to drive the wizard end-to-end without a real cluster.

- [ ] **Step 1: Create the e2e script**

```bash
#!/usr/bin/env bash
# Wizard e2e using the in-process fake K8s clientset.
# Usage: scripts/wizard-test.sh
# The full e2e coverage lives in the Go test files; this shell script
# is a placeholder that documents the entry point.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE/.."

echo "→ wizard e2e is covered by go test ./... — run:"
echo "  go test ./internal/handlers/ -run Wizard -v"
```

- [ ] **Step 2: Commit**

```bash
git add scripts/wizard-test.sh
git commit -m "scripts: wizard-test.sh placeholder pointing to go test"
```

(The full e2e coverage lives in the Go test files. A shell-based e2e
for a HTTP server in Go is brittle and duplicates the Go test surface.)

---

## Self-Review (run before declaring done)

- [ ] All tasks committed; `git log --oneline -20` shows 14+ commits past `45882a0`
- [ ] `go build ./...` clean
- [ ] `go test ./...` all PASS
- [ ] `kubectl kustomize deploy/` renders without error; the updated RBAC includes `restorepoints/details` as a separate resource
- [ ] `bash -n scripts/deploy-test.sh` and `bash -n scripts/wizard-test.sh` syntax OK
- [ ] Manual smoke (if a cluster is available): login → /wizard → pick a VM → pick RP → pick volume → create → see browse directory tree

---

## Coverage Matrix

| Spec section | Implemented in task |
|---|---|
| §4.1 restorepoints.go | Task 3 |
| §4.2 frs.go Create/Delete/Wait | Task 4 |
| §4.3 keymgr | Task 5 |
| §4.4 wizard handlers + watch map | Task 8 |
| §4.5 sessions.html countdown | Task 10 |
| §4.6 browse.html preparing | Task 11 |
| §4.7 templates | Tasks 9, 10, 11 |
| §5 data flow | All tasks |
| §6 SSH key 4 states | Task 5 |
| §7 expiration JS | Task 10 |
| §8 RBAC | Task 1 |
| §9 error handling | Task 8 (renderPreparing), Task 11 (delete errors) |
| §9.5 large files summary | Task 13 (deploy-experience doc §7) |
| §10 testing | Tasks 3, 4, 5, 8, 11 (unit); Task 14 (e2e pointer) |
| §11 risks (single-user, watch map restart) | Task 8 (watchMap), Task 11 (renderPreparing) |
| §13 milestones M1-M6 | M1=Task 1, M2=Tasks 5+6, M3=Tasks 3+4, M4=Tasks 8+9, M5=Tasks 10+11, M6=Task 12 |
| §14 docs sync | Tasks 12, 13 |
| §15 checklist | All tasks combined |

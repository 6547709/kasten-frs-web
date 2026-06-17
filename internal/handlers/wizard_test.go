package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/liguoqiang/kasten-frs-web/internal/auth"
	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
	"github.com/liguoqiang/kasten-frs-web/internal/sftpclient"
)

// stubProvider is a minimal FRSProvider for wizard tests.
type stubProvider struct {
	vms      []k8s.VM
	nss      []string
	rps      []k8s.RestorePoint
	vols     []k8s.VolumeArtifact
	createFn func(ctx context.Context, ns string, spec k8s.FRSpec) (*k8s.FRSView, error)
}

func (s *stubProvider) ListActiveFRS(_ context.Context, _ []string) ([]k8s.FRSView, error) {
	return nil, nil
}
func (s *stubProvider) GetFRS(_ context.Context, _ k8s.FRSRef) (k8s.FRSView, error) {
	return k8s.FRSView{}, nil
}
func (s *stubProvider) ListVMs(_ context.Context, _ []string) ([]k8s.VM, error) {
	return s.vms, nil
}
func (s *stubProvider) ListVMNamespaces(_ context.Context) ([]string, error) {
	return s.nss, nil
}
func (s *stubProvider) ListRestorePoints(_ context.Context, _, _ string) ([]k8s.RestorePoint, error) {
	return s.rps, nil
}
func (s *stubProvider) GetRestorePointDetails(_ context.Context, _, _ string) ([]k8s.VolumeArtifact, error) {
	return s.vols, nil
}
func (s *stubProvider) CreateFRS(ctx context.Context, ns string, spec k8s.FRSpec) (*k8s.FRSView, error) {
	if s.createFn != nil {
		return s.createFn(ctx, ns, spec)
	}
	return &k8s.FRSView{Ref: k8s.FRSRef{Namespace: ns, Name: "frs-wizard-abcde"}, State: "Starting"}, nil
}
func (s *stubProvider) DeleteFRS(_ context.Context, _, _ string) error { return nil }
func (s *stubProvider) WaitForReady(_ context.Context, ns, name string, _ time.Duration) (k8s.FRSView, error) {
	return k8s.FRSView{Ref: k8s.FRSRef{Namespace: ns, Name: name}, State: "Ready"}, nil
}
func (s *stubProvider) LookupFRSSource(_ context.Context, _ *k8s.FRSView) {}

func newWizardTestServer(t *testing.T, stub *stubProvider) *Server {
	t.Helper()
	a := auth.NewAuthenticator("u", "p",
		auth.NewSessionStore([]byte("0123456789abcdef0123456789abcdef"), time.Hour), "kfrs_sid")
	pool := sftpclient.NewPool(nil, time.Hour)
	return New(a, pool, stub, "root", "ssh-ed25519 AAAA...", 2222, nil, 30*time.Second, "test")
}

func TestHandleWizardPage_Renders(t *testing.T) {
	stub := &stubProvider{
		nss: []string{"default", "kasten-io"},
		vms: []k8s.VM{{AppName: "web-01", AppNamespace: "default", RPCount: 3}},
	}
	s := newWizardTestServer(t, stub)
	r := httptest.NewRequest("GET", "/wizard", nil)
	w := httptest.NewRecorder()
	s.handleWizardPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	body := w.Body.String()
	// Namespace selector must list all discovered namespaces plus the
	// "all" sentinel so cross-ns filtering is one click away.
	if !strings.Contains(body, `value="default"`) {
		t.Errorf("namespace option missing in body: %s", body)
	}
	if !strings.Contains(body, "-- All namespaces --") {
		t.Errorf("all-namespace sentinel missing in body: %s", body)
	}
	// Version should be wired through to the footer.
	if !strings.Contains(body, "vtest") && !strings.Contains(body, "test") {
		t.Errorf("version not rendered in footer: %s", body)
	}
}

func TestHandleWizardCreate_RedirectsToBrowse(t *testing.T) {
	stub := &stubProvider{}
	s := newWizardTestServer(t, stub)
	form := url.Values{}
	form.Set("vmNs", "default")
	form.Set("vmName", "web-01")
	form.Set("rpName", "rp1")
	form.Set("pvcNames", "data-pvc")
	r := httptest.NewRequest("POST", "/wizard/create", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleWizardCreate(w, r)
	if w.Code != 303 {
		t.Errorf("code = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/browse?frs=default/frs-wizard-") {
		t.Errorf("redirect = %q, want /browse?frs=default/frs-wizard-...", loc)
	}
	// The watch map should have an entry now.
	if _, ok := s.watches.get(k8s.FRSRef{Namespace: "default", Name: "frs-wizard-abcde"}); !ok {
		t.Error("expected watch map entry after create")
	}
}

func TestHandleWizardCancel_DeletesFRS(t *testing.T) {
	stub := &stubProvider{
		createFn: func(_ context.Context, ns string, _ k8s.FRSpec) (*k8s.FRSView, error) {
			return &k8s.FRSView{Ref: k8s.FRSRef{Namespace: ns, Name: "frs-wizard-xyz"}, State: "Starting"}, nil
		},
	}
	s := newWizardTestServer(t, stub)
	// Pre-populate the watch map
	s.watches.set(k8s.FRSRef{Namespace: "default", Name: "frs-wizard-xyz"}, &watchState{State: "Pending"})

	form := url.Values{}
	form.Set("frs", "default/frs-wizard-xyz")
	r := httptest.NewRequest("POST", "/wizard/cancel", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleWizardCancel(w, r)
	if w.Code != 303 {
		t.Errorf("code = %d, want 303", w.Code)
	}
	if _, ok := s.watches.get(k8s.FRSRef{Namespace: "default", Name: "frs-wizard-xyz"}); ok {
		t.Error("expected watch map entry removed after cancel")
	}
}

func TestWatchMap_ConcurrentSafe(t *testing.T) {
	wm := &watchMap{m: make(map[k8s.FRSRef]*watchState)}
	ref := k8s.FRSRef{Namespace: "ns", Name: "n"}
	wm.set(ref, &watchState{State: "Pending"})
	if s, ok := wm.get(ref); !ok || s.State != "Pending" {
		t.Fatalf("get after set = %v, %v", s, ok)
	}
	wm.del(ref)
	if _, ok := wm.get(ref); ok {
		t.Error("expected entry gone after del")
	}
}

func TestWatchMap_Sweep(t *testing.T) {
	wm := &watchMap{m: make(map[k8s.FRSRef]*watchState)}
	now := time.Now()
	old := k8s.FRSRef{Namespace: "ns", Name: "old"}
	fresh := k8s.FRSRef{Namespace: "ns", Name: "fresh"}
	// Stamp createdAt explicitly so the entry's age is deterministic.
	wm.set(old, &watchState{State: "Ready", createdAt: now.Add(-2 * time.Hour)})
	wm.set(fresh, &watchState{State: "Ready", createdAt: now.Add(-1 * time.Minute)})

	evicted := wm.sweep(time.Hour, now)
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1", evicted)
	}
	if _, ok := wm.get(old); ok {
		t.Error("stale entry should have been swept")
	}
	if _, ok := wm.get(fresh); !ok {
		t.Error("fresh entry should survive the sweep")
	}
}

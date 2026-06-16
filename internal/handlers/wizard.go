package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
)

// watchState tracks the status of an in-flight wizard-created FRS.
// The wizard creates an FRS synchronously, then starts a goroutine
// (watchFRSCreated) that polls WaitForReady and updates this state.
// UI pages (Task 9) read state to show progress to the user.
type watchState struct {
	State string // "Pending" | "Ready" | "Failed" | "Timeout"
	View  k8s.FRSView
	Err   error
	Done  bool
}

// watchMap is a sync.Mutex-protected map of FRSRef → *watchState.
// The wizard uses it to remember FRSes it has created and surface
// their ready/failed status to subsequent page renders.
type watchMap struct {
	mu sync.Mutex
	m  map[k8s.FRSRef]*watchState
}

func (wm *watchMap) get(ref k8s.FRSRef) (*watchState, bool) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	s, ok := wm.m[ref]
	return s, ok
}

func (wm *watchMap) set(ref k8s.FRSRef, s *watchState) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.m[ref] = s
}

func (wm *watchMap) del(ref k8s.FRSRef) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	delete(wm.m, ref)
}

// handleWizardPage renders the wizard landing page: VM picker.
// The wizard_body template lives in web/templates/wizard.html
// (added by Task 9). Until that lands, ExecuteTemplate will return
// an error which we log and otherwise ignore — the rest of the
// wizard flow (HTMX fragments, /wizard/create) is self-contained.
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

// handleWizardVMs re-renders just the VM <select> fragment.
// HTMX targets this on first page load and after a "refresh" click.
func (s *Server) handleWizardVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.frsListVMs(r.Context())
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "VM 列表拉取失败", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "wizard_vms_fragment", map[string]any{
		"VMs": vms,
	}); err != nil {
		slog.Error("render wizard vms", "err", err)
	}
}

// handleWizardRPs returns the <select> of RestorePoints for the
// chosen (ns, vm). HTMX swaps this in when the user picks a VM.
func (s *Server) handleWizardRPs(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	rps, err := s.frsListRPs(r.Context(), ns, name)
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "RP 列表拉取失败", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "wizard_rps_fragment", map[string]any{
		"RPs": rps,
		"VM":  name,
	}); err != nil {
		slog.Error("render wizard rps", "err", err)
	}
}

// handleWizardVolumes returns the per-PVC checkbox list for the
// chosen RestorePoint. HTMX swaps this in when the user picks an RP.
func (s *Server) handleWizardVolumes(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	arts, err := s.frsListVolumes(r.Context(), ns, name)
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Volume 列表拉取失败", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "wizard_vols_fragment", map[string]any{
		"Vols": arts,
		"RP":   name,
	}); err != nil {
		slog.Error("render wizard vols", "err", err)
	}
}

// handleWizardCreate is the wizard's POST endpoint. It validates
// the form, creates the FRS, records a Pending entry in the watch
// map, kicks off the ready-watcher goroutine, and redirects the
// browser straight to /browse for the new FRS.
//
// We use GenerateName="frs-wizard-" on the k8s side, so we don't
// know the assigned name until CreateFRS returns. The redirect
// uses the name in the returned FRSView.
func (s *Server) handleWizardCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "表单错误", err.Error())
		return
	}
	vmNs := r.FormValue("vmNs")
	vmName := r.FormValue("vmName")
	rpName := r.FormValue("rpName")
	pvcNames := r.Form["pvcNames"]
	if vmNs == "" || vmName == "" || rpName == "" || len(pvcNames) == 0 {
		s.renderError(w, http.StatusBadRequest, "参数不完整", "vmNs, vmName, rpName, pvcNames 必填")
		return
	}
	if s.pubKeyPEM == "" {
		s.renderError(w, http.StatusInternalServerError, "无 SSH 公钥", "helper 启动时未加载 SSH 公钥")
		return
	}
	view, err := s.frsCreate(r.Context(), vmNs, k8s.FRSpec{
		RestorePointName: rpName,
		PVCNames:         pvcNames,
		SSHUserPublicKey: s.pubKeyPEM,
	})
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "创建 FRS 失败", err.Error())
		return
	}
	ref := view.Ref
	s.watches.set(ref, &watchState{State: "Pending", View: *view})
	go s.watchFRSCreated(ref, *view)
	http.Redirect(w, r, "/browse?frs="+ref.Namespace+"/"+ref.Name+"&path=/", http.StatusSeeOther)
}

// watchFRSCreated runs in a goroutine after handleWizardCreate. It
// waits for the FRS to reach Ready (or Failed/timeout) and updates
// the watch map entry. The handler returns to the browser
// immediately; the UI polls /browse or a future status endpoint to
// read this state.
func (s *Server) watchFRSCreated(ref k8s.FRSRef, initial k8s.FRSView) {
	timeout := s.frsTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	v, err := s.frsWaitReady(context.Background(), ref, timeout)
	state := &watchState{View: v, Done: true}
	if err != nil {
		if v.State == "Failed" {
			state.State = "Failed"
		} else {
			state.State = "Timeout"
		}
		state.Err = err
	} else {
		state.State = "Ready"
	}
	s.watches.set(ref, state)
	_ = initial // initial view is already in the map; future enhancements could diff.
}

// handleWizardCancel deletes a wizard-created FRS and clears its
// watch-map entry. Always redirects to /wizard so the user can
// pick a different RP/VM.
func (s *Server) handleWizardCancel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "表单错误", err.Error())
		return
	}
	frs := r.FormValue("frs")
	parts := strings.SplitN(frs, "/", 2)
	if len(parts) != 2 {
		s.renderError(w, http.StatusBadRequest, "frs 参数错误", frs)
		return
	}
	if err := s.frsDelete(r.Context(), parts[0], parts[1]); err != nil {
		s.renderError(w, http.StatusBadGateway, "取消失败", err.Error())
		return
	}
	s.watches.del(k8s.FRSRef{Namespace: parts[0], Name: parts[1]})
	http.Redirect(w, r, "/wizard", http.StatusSeeOther)
}

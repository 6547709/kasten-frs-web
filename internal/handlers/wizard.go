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

// handleWizardPage renders the wizard landing page: namespace picker
// + VM picker. VM list is initially empty until the user picks a
// namespace; the empty state is more honest than dumping every VM
// across every namespace, and also disambiguates same-named VMs
// that exist in different namespaces.
func (s *Server) handleWizardPage(w http.ResponseWriter, r *http.Request) {
	nsList, err := s.frsListVMNamespaces(r.Context())
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Namespace 列表拉取失败", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":        "恢复向导",
		"BodyTemplate": "wizard_body",
		"NSList":       nsList,
		"User":         s.auth.Username,
		"Version":      s.version,
	}); err != nil {
		slog.Error("render wizard", "err", err)
	}
}

// handleWizardNamespaces re-renders just the namespace <select> fragment.
func (s *Server) handleWizardNamespaces(w http.ResponseWriter, r *http.Request) {
	nsList, err := s.frsListVMNamespaces(r.Context())
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Namespace 列表拉取失败", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "wizard_namespaces_fragment", map[string]any{
		"NSList": nsList,
	}); err != nil {
		slog.Error("render wizard namespaces", "err", err)
	}
}

// handleWizardVMs re-renders just the VM <ul> fragment. Optional
// query param ns= limits the listing to a single namespace; empty
// ns lists VMs across all namespaces.
func (s *Server) handleWizardVMs(w http.ResponseWriter, r *http.Request) {
	var vms []k8s.VM
	var err error
	if ns := r.URL.Query().Get("ns"); ns != "" {
		vms, err = s.frs.ListVMs(r.Context(), []string{ns})
	} else {
		vms, err = s.frsListVMs(r.Context())
	}
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
// the form, clones the selected PVCs into wizard-managed
// DataVolumes (so K10's datamover finds a matching snapshot in
// the RestorePoint artifact list), creates the FRS, records a
// Pending entry in the watch map, kicks off the ready-watcher
// goroutine, and redirects the browser straight to /browse for
// the new FRS.
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

	// Pull the RP /details so we know each selected PVC's
	// namespace, size, and storage class — needed for the
	// DataVolume clone spec.
	arts, err := s.frsListVolumes(r.Context(), vmNs, rpName)
	if err != nil {
		slog.Error("wizard.create.rp-details", "user", s.auth.Username, "rp", vmNs+"/"+rpName, "err", err)
		s.renderError(w, http.StatusBadGateway, "获取还原点详情失败", err.Error())
		return
	}
	byName := make(map[string]k8s.VolumeArtifact, len(arts))
	for _, a := range arts {
		byName[a.PVCName] = a
	}

	// Clone each selected PVC into a wizard-owned DataVolume, wait
	// for Succeeded, and use the resulting DV name in the FRS
	// spec. K10's datamover refuses FRSes whose pvcName is the
	// source PVC directly with "snapshot not found"; only the
	// clone DV appears in the RP artifact list with snapshot meta.
	dvNames := make([]string, 0, len(pvcNames))
	clonedDVs := make([]string, 0, len(pvcNames)) // for cleanup on FRS failure
	defer func() {
		if len(clonedDVs) == 0 {
			return
		}
		// Best-effort cleanup of DV clones that we created but
		// never handed off to a usable FRS.
		for _, n := range clonedDVs {
			if err := s.frsDeleteDataVolume(context.Background(), vmNs, n); err == nil {
				slog.Info("wizard.create.dv-cleanup", "dv", vmNs+"/"+n)
			}
		}
	}()
	for _, pvc := range pvcNames {
		a, ok := byName[pvc]
		if !ok {
			slog.Error("wizard.create.pvc-not-in-rp", "pvc", pvc, "rp", vmNs+"/"+rpName)
			s.renderError(w, http.StatusBadRequest, "PVC 不在还原点中",
				"PVC "+pvc+" 不是 "+rpName+" 的 artifact")
			return
		}
		srcNS := a.PVCNamespace
		if srcNS == "" {
			srcNS = vmNs
		}
		dvTimeout := s.frsTimeout
		if dvTimeout == 0 {
			dvTimeout = 5 * time.Minute
		}
		slog.Info("wizard.create.clone-dv",
			"user", s.auth.Username, "pvc", pvc,
			"src_ns", srcNS, "size", a.Size, "sc", a.StorageClass,
		)
		dv, err := s.frsCloneDataVolume(r.Context(), vmNs, k8s.DataVolumeSource{
			SourcePVC: pvc, SourcePVCNS: srcNS,
			Size: a.Size, StorageClass: a.StorageClass,
		})
		if err != nil {
			slog.Error("wizard.create.clone-dv.failed", "pvc", pvc, "err", err)
			s.renderError(w, http.StatusBadGateway, "克隆 DataVolume 失败", err.Error())
			return
		}
		dvName := dv.GetName()
		clonedDVs = append(clonedDVs, dvName)
		if err := s.frsWaitDataVolume(r.Context(), vmNs, dvName, dvTimeout); err != nil {
			slog.Error("wizard.create.clone-dv.wait", "dv", vmNs+"/"+dvName, "err", err)
			s.renderError(w, http.StatusGatewayTimeout, "DataVolume 未就绪", err.Error())
			return
		}
		dvNames = append(dvNames, dvName)
		slog.Info("wizard.create.clone-dv.ready", "pvc", pvc, "dv", dvName)
	}
	// All DVs are Succeeded — hand them off to the FRS.
	clonedDVs = nil

	slog.Info("wizard.create",
		"user", s.auth.Username,
		"vm_ns", vmNs, "vm_name", vmName,
		"rp_name", rpName, "dv_count", len(dvNames),
	)
	view, err := s.frsCreate(r.Context(), vmNs, k8s.FRSpec{
		RestorePointName: rpName,
		PVCNames:         dvNames,
		SSHUserPublicKey: s.pubKeyPEM,
	})
	if err != nil {
		slog.Error("wizard.create.failed", "user", s.auth.Username, "vm_ns", vmNs, "vm_name", vmName, "err", err)
		s.renderError(w, http.StatusBadGateway, "创建 FRS 失败", err.Error())
		return
	}
	ref := view.Ref
	s.watches.set(ref, &watchState{State: "Pending", View: *view})
	slog.Info("frs.created", "user", s.auth.Username, "frs", ref.Namespace+"/"+ref.Name)
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
		slog.Warn("frs.wait.terminal",
			"frs", ref.Namespace+"/"+ref.Name,
			"state", state.State,
			"err", err,
		)
	} else {
		state.State = "Ready"
		slog.Info("frs.ready",
			"frs", ref.Namespace+"/"+ref.Name,
			"port", v.Port,
			"service", v.ServiceName+"."+v.ServiceNS+".svc.cluster.local",
		)
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

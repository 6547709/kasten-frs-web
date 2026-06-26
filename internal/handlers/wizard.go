package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
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
	// createdAt is when this entry was last (re)set. The background
	// sweeper uses it to evict stale entries so the map can't grow
	// unbounded when users create FRSes via the wizard but never hit
	// the cancel/delete paths that would otherwise remove them.
	createdAt time.Time
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
	if s.createdAt.IsZero() {
		s.createdAt = time.Now()
	}
	wm.m[ref] = s
}

func (wm *watchMap) del(ref k8s.FRSRef) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	delete(wm.m, ref)
}

// sweep removes entries older than maxAge. Returns the number of
// entries evicted. Called periodically by the background sweeper so a
// long-lived helper process doesn't accumulate watch entries forever.
func (wm *watchMap) sweep(maxAge time.Duration, now time.Time) int {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	var evicted int
	for ref, st := range wm.m {
		if now.Sub(st.createdAt) > maxAge {
			delete(wm.m, ref)
			evicted++
		}
	}
	return evicted
}

// startSweeper launches a goroutine that evicts watch-map entries
// older than maxAge every interval. It exits when ctx is cancelled
// (graceful shutdown). Both args are clamped to sane minimums so a
// misconfiguration can't busy-loop.
func (wm *watchMap) startSweeper(ctx context.Context, interval, maxAge time.Duration, log *slog.Logger) {
	if interval < time.Minute {
		interval = time.Minute
	}
	if maxAge < time.Minute {
		maxAge = time.Hour
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := wm.sweep(maxAge, time.Now()); n > 0 && log != nil {
					log.Debug("watchmap.sweep", "evicted", n, "max_age", maxAge.String())
				}
			}
		}
	}()
}

// handleWizardPage renders the wizard landing page: namespace picker
// + VM picker. VM list is initially empty until the user picks a
// namespace; the empty state is more honest than dumping every VM
// across every namespace, and also disambiguates same-named VMs
// that exist in different namespaces.
func (s *Server) handleWizardPage(w http.ResponseWriter, r *http.Request) {
	nsList, err := s.frsListVMNamespaces(r.Context())
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Failed to list namespaces", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":        "Recovery Wizard",
		"BodyTemplate": "wizard_body",
		"NSList":       nsList,
		"User":         s.auth.Username,
		"Version":      s.version,
		"CSRF":         s.auth.CSRFToken(r),
	}); err != nil {
		s.log(r.Context()).Error("render wizard", "err", err)
	}
}

// handleWizardNamespaces re-renders just the namespace <select> fragment.
func (s *Server) handleWizardNamespaces(w http.ResponseWriter, r *http.Request) {
	nsList, err := s.frsListVMNamespaces(r.Context())
	if err != nil {
		slog.Warn("wizard.nslist.failed", "user", s.auth.Username, "err", err)
		s.renderError(w, http.StatusBadGateway, "Failed to list namespaces", err.Error())
		return
	}
	slog.Info("wizard.nslist", "user", s.auth.Username, "count", len(nsList))
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
	ns := r.URL.Query().Get("ns")
	if ns != "" {
		vms, err = s.frs.ListVMs(r.Context(), []string{ns})
	} else {
		vms, err = s.frsListVMs(r.Context())
	}
	if err != nil {
		slog.Warn("wizard.vmlist.failed", "user", s.auth.Username, "ns", ns, "err", err)
		s.renderError(w, http.StatusBadGateway, "Failed to list VMs", err.Error())
		return
	}
	slog.Info("wizard.vmlist", "user", s.auth.Username, "ns", ns, "count", len(vms))
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
		s.renderError(w, http.StatusBadGateway, "Failed to list RestorePoints", err.Error())
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
		s.renderError(w, http.StatusBadGateway, "Failed to list volumes", err.Error())
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

// formKeys returns the list of form field names present in r.
// Used in the wizard-create 400 log so we can tell whether the
// browser ever sent a vmNs/rpName/pvcNames key at all (vs.
// sent them but empty). A request with zero keys almost always
// means the form was posted via dev tools / curl rather than
// the wizard UI.
func formKeys(r *http.Request) []string {
	if err := r.ParseForm(); err != nil {
		return nil
	}
	keys := make([]string, 0, len(r.Form))
	for k := range r.Form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
		s.renderError(w, http.StatusBadRequest, "Bad form data", err.Error())
		return
	}
	vmNs := r.FormValue("vmNs")
	vmName := r.FormValue("vmName")
	rpName := r.FormValue("rpName")
	pvcNames := r.Form["pvcNames"]
	// Identify the specific missing field(s) so the operator
	// looking at the rendered error page (and the pod log) can
	// tell which step of the wizard didn't populate. The previous
	// generic "vmNs, vmName, rpName, pvcNames are required"
	// message left you guessing whether the user skipped the VM
	// row, the RP row, or the volume checkboxes.
	var missing []string
	if vmNs == "" {
		missing = append(missing, "vmNs (the chosen VM's namespace — not set means no VM row was clicked)")
	}
	if vmName == "" {
		missing = append(missing, "vmName (the chosen VM's name — not set means no VM row was clicked)")
	}
	if rpName == "" {
		missing = append(missing, "rpName (the chosen RestorePoint — not set means no RP row was clicked, or the vol-list for the previous VM was cleared on a re-click and the user never re-picked an RP)")
	}
	if len(pvcNames) == 0 {
		missing = append(missing, "pvcNames (at least one volume checkbox must be selected — not set means the vol-list was empty, or the user never checked a box, or pvcFields innerHTML was cleared between check and submit)")
	}
	if len(missing) > 0 {
		// Log the full form so post-mortem doesn't have to guess
		// whether the user submitted via the wizard UI, a curl
		// replay, or dev tools.
		slog.Warn("wizard.create.missing_params",
			"user", s.auth.Username,
			"vm_ns", vmNs, "vm_name", vmName,
			"rp_name", rpName, "pvc_count", len(pvcNames),
			"raw_form_keys", formKeys(r),
			"missing", missing,
		)
		s.renderError(w, http.StatusBadRequest,
			"Missing wizard parameters",
			"The following required fields were empty when Create FRS was submitted:\n\n  - "+strings.Join(missing, "\n  - ")+"\n\n"+
				"This almost always means a JS error prevented the wizard from filling the hidden inputs, "+
				"or the form was submitted via dev tools / curl (bypassing the button's disabled state). "+
				"Open the browser dev console and check the wizard's app.js handlers; "+
				"the pod log has the full form keys dumped under wizard.create.missing_params.",
		)
		return
	}
	if s.pubKeyPEM == "" {
		s.renderError(w, http.StatusInternalServerError, "No SSH public key", "helper did not load an SSH public key at startup")
		return
	}
	log := s.log(r.Context())
	log.Info("wizard.create",
		"user", s.auth.Username,
		"vm_ns", vmNs, "vm_name", vmName,
		"rp_name", rpName, "pvc_count", len(pvcNames),
	)
	view, err := s.frsCreate(r.Context(), vmNs, k8s.FRSpec{
		RestorePointName: rpName,
		PVCNames:         pvcNames,
		SSHUserPublicKey: s.pubKeyPEM,
	})
	if err != nil {
		log.Error("wizard.create.failed", "user", s.auth.Username, "vm_ns", vmNs, "vm_name", vmName, "err", err)
		s.renderError(w, http.StatusBadGateway, "Failed to create FRS", err.Error())
		return
	}
	ref := view.Ref
	s.watches.set(ref, &watchState{State: "Pending", View: *view})
	log.Info("frs.created", "user", s.auth.Username, "frs", ref.Namespace+"/"+ref.Name)
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
		timeout = 120 * time.Second
	}
	s.watchFRSCreatedWithTimeout(ref, initial, timeout)
}

// watchFRSCreatedWithTimeout is the worker behind both the initial
// wizard create and the "Wait longer" extend button. It loops
// WaitForReady for the given window and writes a terminal state
// into the watch map.
func (s *Server) watchFRSCreatedWithTimeout(ref k8s.FRSRef, initial k8s.FRSView, timeout time.Duration) {
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
			"timeout", timeout,
			"err", err,
		)
	} else {
		state.State = "Ready"
		slog.Info("frs.ready",
			"frs", ref.Namespace+"/"+ref.Name,
			"port", v.Port,
			"service", v.ServiceName+"."+v.ServiceNS+".svc.cluster.local",
			"timeout", timeout,
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
		s.renderError(w, http.StatusBadRequest, "Bad form data", err.Error())
		return
	}
	frs := r.FormValue("frs")
	parts := strings.SplitN(frs, "/", 2)
	if len(parts) != 2 {
		s.renderError(w, http.StatusBadRequest, "Bad frs parameter", frs)
		return
	}
	if err := s.frsDelete(r.Context(), parts[0], parts[1]); err != nil {
		s.renderError(w, http.StatusBadGateway, "Failed to cancel", err.Error())
		return
	}
	s.watches.del(k8s.FRSRef{Namespace: parts[0], Name: parts[1]})
	http.Redirect(w, r, "/wizard", http.StatusSeeOther)
}

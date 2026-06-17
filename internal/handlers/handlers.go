// Package handlers implements HTTP handlers for the helper.
package handlers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/liguoqiang/kasten-frs-web/internal/auth"
	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
	"github.com/liguoqiang/kasten-frs-web/internal/logging"
	"github.com/liguoqiang/kasten-frs-web/internal/metrics"
	"github.com/liguoqiang/kasten-frs-web/internal/sftpclient"
	"github.com/liguoqiang/kasten-frs-web/web"
	"k8s.io/apimachinery/pkg/types"
)

// pageTemplates is loaded once from the embedded web/templates/*.html.
// layout.html defines the layout template; sessions.html / browse.html
// each define a `*_body` template that layout.html includes via an
// if-eq dispatch on .BodyTemplate. Earlier versions of this handler
// used inline `sessionsTmpl` / `browseTmpl` string constants that
// omitted the layout and the styling. We load the canonical templates
// here so the on-disk HTML in web/templates/ is the single source
// of truth.
var pageTemplates = template.Must(
	template.New("").Funcs(template.FuncMap{
		"splitPath":     splitPath,
		"isLastPathSeg": isLastPathSeg,
		"buildPath":     buildPath,
		"parentPath":    parentPath,
		"joinPath":      joinPath,
		"lower":         strings.ToLower,
		"humanSize":     humanSize,
	}).ParseFS(web.Templates(), "templates/*.html"))

// humanSize formats a byte count as a human-friendly string using
// binary (1024-based) units, matching the convention most file
// browsers use (e.g. 1536 → "1.5 KiB", 0 → "0 B"). Negative inputs
// (shouldn't happen for file sizes) are clamped to 0.
func humanSize(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}

// splitPath turns an absolute path "/a/b/c" into a slice of
// path-segments, ["a", "b", "c"]. An empty path or "/" yields nil.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// isLastPathSeg reports whether the i-th segment is the last one.
// Used by the crumb-rendering code to make the final segment a
// non-clickable <span> instead of an <a>.
func isLastPathSeg(i, total int) bool {
	return i == total-1
}

// buildPath returns the absolute path through the i-th segment
// (inclusive) of an absolute path. E.g. buildPath("/a/b/c", 1) = "/a".
// Used to make each crumb in the path bar a link to the
// corresponding parent.
func buildPath(p string, upTo int) string {
	segs := splitPath(p)
	if upTo < 0 {
		upTo = 0
	}
	if upTo > len(segs) {
		upTo = len(segs)
	}
	return "/" + strings.Join(segs[:upTo], "/")
}

// parentPath returns the absolute parent path. "/" returns "/".
func parentPath(p string) string {
	if p == "/" || p == "" {
		return "/"
	}
	p = strings.TrimRight(p, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// joinPath joins a parent path and a leaf name with a single '/'.
func joinPath(parent, leaf string) string {
	if parent == "" || parent == "/" {
		return "/" + leaf
	}
	if strings.HasSuffix(parent, "/") {
		return parent + leaf
	}
	return parent + "/" + leaf
}

// FRSProvider abstracts the K8s FRS calls used by handlers.
// The wizard (Task 8) added ListVMs / ListRestorePoints /
// GetRestorePointDetails / CreateFRS / DeleteFRS / WaitForReady
// on top of the original ListActiveFRS / GetFRS pair.
type FRSProvider interface {
	ListActiveFRS(ctx context.Context, namespaces []string) ([]k8s.FRSView, error)
	ListAllFRS(ctx context.Context, namespaces []string) ([]k8s.FRSView, error)
	GetFRS(ctx context.Context, ref k8s.FRSRef) (k8s.FRSView, error)
	ListVMs(ctx context.Context, namespaces []string) ([]k8s.VM, error)
	ListVMNamespaces(ctx context.Context) ([]string, error)
	ListRestorePoints(ctx context.Context, ns, appName string) ([]k8s.RestorePoint, error)
	GetRestorePointDetails(ctx context.Context, ns, name string) ([]k8s.VolumeArtifact, error)
	CreateFRS(ctx context.Context, ns string, spec k8s.FRSpec) (*k8s.FRSView, error)
	DeleteFRS(ctx context.Context, ns, name string) error
	WaitForReady(ctx context.Context, ns, name string, timeout time.Duration) (k8s.FRSView, error)
	LookupFRSSource(ctx context.Context, v *k8s.FRSView)
}

// Server wires auth, SFTP pool, and FRS provider into a *http.ServeMux.
type Server struct {
	auth        *auth.Authenticator
	pool        *sftpclient.Pool
	frs         FRSProvider
	mux         *http.ServeMux
	username    string
	pubKeyPEM   string
	frsPort     int
	nsWhitelist []string
	version     string
	logger      *slog.Logger
	// watches tracks in-flight FRSes created by the wizard
	// (Task 8). Keyed by FRSRef; entry is set to "Pending" on
	// create and updated to "Ready"/"Failed"/"Timeout" by a
	// background goroutine that runs WaitForReady.
	watches *watchMap
	// frsTimeout bounds how long the wizard's ready-watcher
	// goroutine will wait for an FRS to become Ready before
	// marking it as Timeout in the watch map. Sourced from
	// HELPER_FRS_WAIT_TIMEOUT in config; defaults to 30s.
	frsTimeout time.Duration
}

// New builds a Server. version is shown in the UI footer; pass the
// same string as the image tag (e.g. "v0.3.1") so operators can see
// at a glance which release is running.
func New(a *auth.Authenticator, pool *sftpclient.Pool, frs FRSProvider,
	username, pubKeyPEM string, frsPort int, nsWhitelist []string,
	frsTimeout time.Duration, version string) *Server {
	s := &Server{
		auth:        a,
		pool:        pool,
		frs:         frs,
		mux:         http.NewServeMux(),
		username:    username,
		pubKeyPEM:   pubKeyPEM,
		frsPort:     frsPort,
		nsWhitelist: nsWhitelist,
		version:     version,
		logger:      slog.Default(),
		watches:     &watchMap{m: make(map[k8s.FRSRef]*watchState)},
		frsTimeout:  frsTimeout,
	}
	s.routes()
	return s
}

// Router returns the underlying mux.
func (s *Server) Router() *http.ServeMux { return s.mux }

// StartBackground launches the server's background maintenance
// goroutines (currently: the watch-map sweeper that evicts stale
// wizard-created FRS watch entries). It returns immediately; the
// goroutines exit when ctx is cancelled. Safe to call once at
// startup. Entries are evicted ~1h after creation, swept every 10m.
func (s *Server) StartBackground(ctx context.Context) {
	s.watches.startSweeper(ctx, 10*time.Minute, time.Hour, s.logger)
}

// log returns a request-scoped logger carrying the request_id that
// AccessLog stitched into ctx, so every line emitted while handling
// one request can be correlated. Falls back to the server's base
// logger when no request_id is present.
func (s *Server) log(ctx context.Context) *slog.Logger {
	return logging.FromContext(ctx, s.logger)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.auth.HandleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)
	s.mux.HandleFunc("GET /logout", s.handleLogout) // GET-friendly for plain <a> links

	// Serve embedded static assets (CSS, JS, images) under /static/.
	// Without this, the browser would receive an HTML 404 for the
	// stylesheet <link> in the layout, and strict-MIME-mode browsers
	// (default since Chrome 100+) would refuse to apply it, leaving
	// the page unstyled. http.FileServer sets .css to text/css and
	// .js to application/javascript by extension mapping.
	s.mux.Handle("/static/", http.StripPrefix("/static/",
		http.FileServer(http.FS(web.Static()))))

	authed := http.NewServeMux()
	authed.HandleFunc("GET /sessions", s.handleSessions)
	authed.HandleFunc("POST /sessions/{ns}/{name}/connect", s.handleConnect)
	authed.HandleFunc("POST /sessions/{ns}/{name}/delete", s.handleSessionDelete)
	authed.HandleFunc("GET /browse", s.handleBrowse)
	authed.HandleFunc("POST /browse/extend", s.handleBrowseExtend)
	authed.HandleFunc("GET /download", s.handleDownload)
	authed.HandleFunc("GET /download-zip", s.handleDownloadZip)

	// Wizard (Task 8): namespace picker → VM picker → RP picker → Volume picker → Create FRS.
	authed.HandleFunc("GET /wizard", s.handleWizardPage)
	authed.HandleFunc("GET /wizard/namespaces", s.handleWizardNamespaces)
	authed.HandleFunc("GET /wizard/vms", s.handleWizardVMs)
	authed.HandleFunc("GET /wizard/vms/{ns}/{name}/restorepoints", s.handleWizardRPs)
	authed.HandleFunc("GET /wizard/rps/{ns}/{name}/details", s.handleWizardVolumes)
	authed.HandleFunc("POST /wizard/create", s.handleWizardCreate)
	authed.HandleFunc("POST /wizard/cancel", s.handleWizardCancel)

	authed.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sessions", http.StatusSeeOther)
	})

	s.mux.Handle("/", s.auth.RequireAuth(authed))
}

func (s *Server) handleLoginPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":        "Login",
		"BodyTemplate": "login_body",
		"Version":      s.version,
	}); err != nil {
		slog.Error("render login", "err", err)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: s.auth.CookieName, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	// List ALL FRSes (including Failed/Succeeded/Terminated and
	// expired) so operators can see and clean up garbage CRs left
	// behind when a session times out and K10 deletes its pod. Rows
	// that aren't Ready/Connectable render with only a Delete action.
	frsList, err := s.frs.ListAllFRS(r.Context(), s.nsWhitelist)
	if err != nil {
		metrics.FRSListTotal.WithLabelValues("error").Inc()
		s.renderError(w, http.StatusBadGateway, "Failed to list FRSes", err.Error())
		return
	}
	metrics.FRSListTotal.WithLabelValues("ok").Inc()
	metrics.FRSListSize.Set(float64(len(frsList)))
	// Decorate each FRS with its source app name + restore-point
	// creation time so the sessions table can disambiguate FRSes
	// that share a generated name prefix (e.g. multiple FRSes
	// against the same VM show as frs-wizard-abcde / frs-wizard-fghij).
	s.enrichFRSContext(r.Context(), frsList)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":        "Active FRS Sessions",
		"BodyTemplate": "sessions_body",
		"FRS":          frsList,
		"User":         s.auth.Username,
		"Version":      s.version,
		"CSRF":         s.auth.CSRFToken(r),
	}); err != nil {
		s.log(r.Context()).Error("render sessions", "err", err)
	}
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ref := k8s.FRSRef{Namespace: ns, Name: name}
	view, err := s.frs.GetFRS(r.Context(), ref)
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Failed to query FRS", err.Error())
		return
	}
	if view.Port != int64(s.frsPort) {
		s.renderError(w, http.StatusBadRequest, "FRS port not permitted",
			fmt.Sprintf("FRS reports port %d but only %d is allowed", view.Port, s.frsPort))
		return
	}

	addr := fmt.Sprintf("%s.%s.svc.cluster.local:%d", view.ServiceName, view.ServiceNS, view.Port)
	log := s.log(r.Context())
	log.Info("sftp.connect.start",
		"user", s.auth.Username,
		"frs", ns+"/"+name,
		"service", view.ServiceName+"."+view.ServiceNS,
		"port", view.Port,
	)
	sess, err := s.pool.Client().Dial(r.Context(), addr, view.HostKeySig)
	if err != nil {
		metrics.SFTPConnectTotal.WithLabelValues("failure").Inc()
		log.Error("sftp.connect.failed",
			"user", s.auth.Username,
			"frs", ns+"/"+name,
			"addr", addr,
			"err", err,
		)
		s.renderError(w, http.StatusBadGateway, "SFTP connection failed", err.Error())
		return
	}
	metrics.SFTPConnectTotal.WithLabelValues("success").Inc()
	uid := userIDFromCookie(r, s.auth.CookieName)
	key := sftpclient.SessionKey{UserSessionID: uid, FRS: types.NamespacedName{Namespace: ns, Name: name}}
	s.pool.Store(key, sess)
	metrics.SFTPConnectionsActive.Set(float64(s.pool.Len()))
	log.Info("sftp.connect.ready",
		"user", s.auth.Username,
		"frs", ns+"/"+name,
		"addr", addr,
	)
	http.Redirect(w, r, "/browse?frs="+ns+"/"+name+"&path=/", http.StatusSeeOther)
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	ref, path, err := parseFRSQuery(r)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Invalid frs query", err.Error())
		return
	}

	// partial=ready is the polling endpoint used by the "FRS Preparing"
	// page. Resolve it BEFORE any other branch so renderPreparing
	// / renderError can't accidentally return a full <html> document
	// that htmx would then inject into the preparing wrapper and
	// stack one full layout per poll (the screen-in-screen bug).
	if r.URL.Query().Get("partial") == "ready" {
		s.handlePartialReady(w, r, ref, path)
		return
	}

	// Check watch map first for terminal states from wizard creation.
	if ws, ok := s.watches.get(ref); ok && ws.Done {
		if ws.State == "Ready" {
			// fall through to normal browse
		} else {
			s.renderPreparing(w, r, ref, ws)
			return
		}
	}

	key := sftpclient.SessionKey{
		UserSessionID: userIDFromCookie(r, s.auth.CookieName),
		FRS:           types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name},
	}
	sess, ok := s.pool.Get(key)
	if !ok {
		// Need to (re)connect; check FRS state
		view, err := s.frsGet(r.Context(), ref)
		if err != nil {
			if ws, ok := s.watches.get(ref); ok {
				s.renderPreparing(w, r, ref, ws)
				return
			}
			s.renderError(w, http.StatusBadGateway, "Failed to query FRS", err.Error())
			return
		}
		if view.State != "Ready" {
			// Synthesize a watchState with Done=true so the next poll
			// hits the "ws.Done && ws.State != Ready" branch and shows
			// the preparing page (instead of falling through to the
			// SFTP dial path, which would 502 because the FRS hasn't
			// bound a Service yet).
			s.renderPreparing(w, r, ref, &watchState{State: view.State, View: view, Done: true})
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/sessions/%s/%s/connect", ref.Namespace, ref.Name), http.StatusSeeOther)
		return
	}

	entries, err := sess.ListDir(path)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "Failed to list directory", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":        "Browse " + ref.Namespace + "/" + ref.Name,
		"BodyTemplate": "browse_body",
		"FRS":          ref,
		"Path":         path,
		"Entries":      entries,
		"User":         s.auth.Username,
		"Version":      s.version,
		"CSRF":         s.auth.CSRFToken(r),
	}); err != nil {
		s.log(r.Context()).Error("render browse", "err", err)
	}
}

// handlePartialReady serves the preparing page's polling endpoint.
// Returns a fragment suitable for hx-swap="innerHTML" against the
// preparing wrapper. Critical: NEVER return a full layout here,
// otherwise htmx injects the whole <html> into the wrapper and
// the next poll wraps it in another layer (screen-in-screen).
// Behaviour:
//
//	Ready         -> 204 No Content + HX-Redirect. The browser
//	                  follows the redirect to /browse, which
//	                  renders the proper page from scratch.
//	NotReady      -> 204 No Content (poll again in 2s).
//	Failed/Timeout -> 200 + the preparing body fragment (no
//	                  layout) so the wrapper can swap it in
//	                  without nesting.
//
// If the request did NOT come from htmx (no HX-Request header —
// e.g. user typed the URL in the address bar, or back/forward
// navigation), fall back to the full page render instead of
// returning 204 to a non-htmx browser (which would render as a
// blank page).
func (s *Server) handlePartialReady(w http.ResponseWriter, r *http.Request, ref k8s.FRSRef, path string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	log := s.log(r.Context())
	htmxRequest := r.Header.Get("HX-Request") == "true"
	notReady := func() {
		if htmxRequest {
			// keep-polling: empty body, no swap
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Direct browser navigation: render the preparing page
		// so the user sees something instead of a blank 204.
		ws, ok := s.watches.get(ref)
		if !ok {
			ws = &watchState{State: "Pending", View: k8s.FRSView{Ref: ref}}
		}
		s.renderPreparing(w, r, ref, ws)
	}
	if ws, ok := s.watches.get(ref); ok {
		switch ws.State {
		case "Ready":
			log.Info("frs.partial.ready", "frs", ref.Namespace+"/"+ref.Name)
			w.Header().Set("HX-Redirect", "/browse?frs="+ref.Namespace+"/"+ref.Name+"&path="+url.QueryEscape(path))
			w.WriteHeader(http.StatusNoContent)
			return
		case "Failed", "Timeout":
			log.Info("frs.partial.terminal", "frs", ref.Namespace+"/"+ref.Name, "state", ws.State)
			s.renderPreparingBody(w, r, ref, ws)
			return
		}
	}
	// No watch entry (operator pre-created FRS) or still pending:
	// query K8s, treat empty/Failed as terminal, anything else as
	// keep-polling.
	view, err := s.frsGet(r.Context(), ref)
	if err != nil {
		log.Warn("frs.partial.query", "frs", ref.Namespace+"/"+ref.Name, "err", err)
		notReady()
		return
	}
	switch view.State {
	case "Ready":
		log.Info("frs.partial.ready", "frs", ref.Namespace+"/"+ref.Name)
		w.Header().Set("HX-Redirect", "/browse?frs="+ref.Namespace+"/"+ref.Name+"&path="+url.QueryEscape(path))
		w.WriteHeader(http.StatusNoContent)
	case "Failed":
		s.renderPreparingBody(w, r, ref, &watchState{State: "Failed", View: view, Done: true})
	default:
		notReady()
	}
}

func (s *Server) renderPreparing(w http.ResponseWriter, r *http.Request, ref k8s.FRSRef, ws *watchState) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":        "FRS preparing",
		"BodyTemplate": "browse_preparing_body",
		"FRS":          ref,
		"State":        ws.State,
		"Error":        errString(ws.Err),
		"User":         s.auth.Username,
		"Version":      s.version,
		"CSRF":         s.auth.CSRFToken(r),
	}); err != nil {
		s.log(r.Context()).Error("render preparing", "err", err)
	}
}

// renderPreparingBody writes JUST the <div class="browse-preparing">
// fragment, with no layout chrome. Used by the partial=ready poll
// when the FRS reaches a terminal Failed/Timeout state: htmx
// innerHTML-swaps it into the existing wrapper without ever
// nesting a full <html> inside the page (the screen-in-screen
// regression that the full-page renderPreparing would cause here).
func (s *Server) renderPreparingBody(w http.ResponseWriter, r *http.Request, ref k8s.FRSRef, ws *watchState) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "browse_preparing_body", map[string]any{
		"FRS":   ref,
		"State": ws.State,
		"Error": errString(ws.Err),
		"CSRF":  s.auth.CSRFToken(r),
	}); err != nil {
		s.log(r.Context()).Error("render preparing body", "err", err)
	}
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

// handleBrowseExtend is the "Wait longer" button on the preparing
// page. Per spec §9 + §15, when WaitForReady times out the user
// must be able to extend the wait instead of giving up. The
// optional "sec" form field selects the new wait window (default
// 60s); the form passes it so the button label matches the actual
// wait time. We update the watch map to a fresh Pending state
// with the current FRS view, then start a new watchFRSCreated
// goroutine that polls WaitForReady with the requested timeout.
// The browser is 303-redirected back to /browse where it will see
// the new Pending state.
func (s *Server) handleBrowseExtend(w http.ResponseWriter, r *http.Request) {
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
	ref := k8s.FRSRef{Namespace: parts[0], Name: parts[1]}
	// Parse the requested extend window. Falls back to the server's
	// configured frsTimeout, then 60s. The form's "sec" param lets
	// the button advertise exactly how long the user is asking to
	// wait ("Wait 60s more" → sec=60).
	extend := s.frsTimeout
	if extend == 0 {
		extend = 60 * time.Second
	}
	if secStr := r.FormValue("sec"); secStr != "" {
		if sec, err := strconv.Atoi(secStr); err == nil && sec > 0 && sec <= 600 {
			extend = time.Duration(sec) * time.Second
		}
	}
	// Fetch the current FRS view to seed the watch map with a sensible initial.
	v, err := s.frsGet(r.Context(), ref)
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Failed to query FRS", err.Error())
		return
	}
	s.watches.set(ref, &watchState{State: "Pending", View: v})
	go s.watchFRSCreatedWithTimeout(ref, v, extend)
	http.Redirect(w, r, "/browse?frs="+ref.Namespace+"/"+ref.Name+"&path=/", http.StatusSeeOther)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	ref, path, err := parseFRSQuery(r)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Invalid frs query", err.Error())
		return
	}
	key := sftpclient.SessionKey{UserSessionID: userIDFromCookie(r, s.auth.CookieName),
		FRS: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}
	sess, ok := s.pool.Get(key)
	if !ok {
		s.renderError(w, http.StatusUnauthorized, "SFTP session expired",
			"Please return to FRS Sessions and re-click Browse")
		return
	}
	rc, err := sess.Open(path)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "Failed to open file", err.Error())
		return
	}
	defer rc.Close()
	stat, statErr := sess.Stat(path)
	if statErr != nil {
		// Non-fatal: we can still stream the file without a
		// Content-Length. Log it so a flaky FRS Stat is visible
		// rather than silently swallowed.
		s.log(r.Context()).Warn("download.stat.failed", "frs", ref.Namespace+"/"+ref.Name, "path", path, "err", statErr)
	}
	w.Header().Set("Content-Disposition", contentDispositionFilename(baseName(path)))
	if stat != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	}
	w.WriteHeader(http.StatusOK)
	n, _ := io.Copy(w, rc)
	metrics.DownloadFilesTotal.WithLabelValues(ref.Namespace, ref.Name).Inc()
	metrics.DownloadBytesTotal.WithLabelValues(ref.Namespace, ref.Name).Add(float64(n))
}

// contentDispositionFilename builds a Content-Disposition header value
// that is safe for arbitrary file names. It emits both a plain
// "filename=" (ASCII-sanitised fallback for legacy clients) and an
// RFC 5987 "filename*=UTF-8”…" form so names with spaces, quotes, or
// non-ASCII characters are conveyed correctly and can't break out of
// the header. See RFC 6266 §5.
func contentDispositionFilename(name string) string {
	// ASCII fallback: replace anything outside a conservative set
	// (and the quote/backslash that could break the quoted-string)
	// with '_'.
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r > 0x7e {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	ascii := b.String()
	if ascii == "" {
		ascii = "download"
	}
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(name)
}

// handleDownloadZip streams a directory (or single file) from the FRS
// as a gzipped tar archive. The FRS SFTP client doesn't support
// recursive directory transfers natively — we walk the tree via
// ListDir() and copy each file's contents into the tar stream.
// Single-file requests are packed as a tar with one entry, named
// after the file's basename.
//
// Path-traversal protection: every entry's path is checked against
// the requested root before being written to the archive, so a
// malicious FRS server can't smuggle a "../etc/passwd" entry.
func (s *Server) handleDownloadZip(w http.ResponseWriter, r *http.Request) {
	ref, root, err := parseFRSQuery(r)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Invalid frs query", err.Error())
		return
	}
	if root == "" {
		root = "/"
	}
	key := sftpclient.SessionKey{UserSessionID: userIDFromCookie(r, s.auth.CookieName),
		FRS: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}
	sess, ok := s.pool.Get(key)
	if !ok {
		s.renderError(w, http.StatusUnauthorized, "SFTP session expired",
			"Please return to FRS Sessions and re-click Browse")
		return
	}
	stat, err := sess.Stat(root)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "FRS path does not exist", err.Error())
		return
	}

	// Tar entry names are relative to root. For a single-file request,
	// the root name is the file's basename; for a directory, we walk
	// the tree and use paths relative to root (with a trailing /).
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+sanitizeArchiveName(ref, root, stat.IsDir())+`"`)
	w.WriteHeader(http.StatusOK)

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	walk := func(rel string, abs string, isDir bool) error {
		// Defense in depth: the SFTP server is trusted (we connected
		// to it over a real FRS pod), but make sure we never write
		// a path that escapes the requested root. The root path
		// itself ("/") normalises to itself and is always safe; any
		// other path with a "/../" prefix would escape root.
		clean := path.Clean("/" + rel)
		if clean != "/" && strings.HasPrefix(clean, "/../") {
			return fmt.Errorf("refusing to archive escape %q", rel)
		}

		// Stat the absolute path to get mode + size.
		fi, err := sess.Stat(abs)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		// Header.Name is the path inside the archive; tar uses "/"
		// as the separator. Prefix empty root with ".".
		name := clean
		if name == "/" {
			name = "."
		} else if isDir {
			// Ensure directory entries end with "/"
			name = strings.TrimSuffix(name, "/") + "/"
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !isDir {
			rc, err := sess.Open(abs)
			if err != nil {
				return err
			}
			defer rc.Close()
			if _, err := io.Copy(tw, rc); err != nil {
				return err
			}
		}
		return nil
	}

	// Recursive walker: ListDir → for each entry → recurse / add.
	var rec func(rel, abs string, isDir bool) error
	rec = func(rel, abs string, isDir bool) error {
		if err := walk(rel, abs, isDir); err != nil {
			return err
		}
		if !isDir {
			return nil
		}
		entries, err := sess.ListDir(abs)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childRel := path.Join(rel, e.Name())
			childAbs := path.Join(abs, e.Name())
			if err := rec(childRel, childAbs, e.IsDir()); err != nil {
				return err
			}
		}
		return nil
	}

	if err := rec(root, root, stat.IsDir()); err != nil {
		slog.Error("zip walk", "path", root, "err", err)
		// Body has already been sent with headers; can't render an
		// error page. Just truncate the archive (gzip CRC will fail
		// client-side, but the partial entries are still useful).
		return
	}
}

// sanitizeArchiveName builds the Content-Disposition filename. For
// single-file requests: just the file's basename. For directories:
// the relative path under the FRS, with the leading slash stripped
// and slashes replaced with dashes (tar convention).
func sanitizeArchiveName(ref k8s.FRSRef, path string, isDir bool) string {
	clean := strings.Trim(path, "/")
	clean = strings.ReplaceAll(clean, "/", "-")
	if clean == "" {
		return fmt.Sprintf("%s-%s.tar.gz", ref.Namespace, ref.Name)
	}
	if !strings.HasSuffix(clean, ".tar.gz") {
		clean += ".tar.gz"
	}
	return clean
}

func parseFRSQuery(r *http.Request) (k8s.FRSRef, string, error) {
	q := r.URL.Query()
	frs := q.Get("frs")
	parts := strings.SplitN(frs, "/", 2)
	if len(parts) != 2 {
		return k8s.FRSRef{}, "", fmt.Errorf("invalid frs query")
	}
	return k8s.FRSRef{Namespace: parts[0], Name: parts[1]}, q.Get("path"), nil
}

func userIDFromCookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func baseName(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

// renderError writes a Kasten-styled error page for the given
// HTTP status, title, and message. Use this for user-facing
// failures (FRS unavailable, SFTP not found, etc.) so users see
// the same chrome as the rest of the app. Lower-level failures
// (auth header missing, internal BadGateway on K8s API errors)
// can keep using http.Error() — those aren't user-actionable.
func (s *Server) renderError(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":        title,
		"BodyTemplate": "error_body",
		"Message":      msg,
		"User":         s.auth.Username,
		"Version":      s.version,
	}); err != nil {
		slog.Error("render error page", "title", title, "err", err)
	}
}

// FRSProvider method-forwarders. The wizard (Task 8) handlers
// (in wizard.go) call these rather than reaching through s.frs
// directly. Reasons:
//   1. Each call site stays a single line (e.g. frsListVMs) and
//      doesn't need to re-thread s.nsWhitelist.
//   2. Tests can stub FRSProvider uniformly; the forwarders
//      are the same shape as the underlying interface methods.
//   3. If we later want to inject cross-cutting behavior
//      (retries, metrics, tracing), it lives in one place.

func (s *Server) frsListVMs(ctx context.Context) ([]k8s.VM, error) {
	return s.frs.ListVMs(ctx, s.nsWhitelist)
}

func (s *Server) frsListVMNamespaces(ctx context.Context) ([]string, error) {
	return s.frs.ListVMNamespaces(ctx)
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

// enrichFRSContext decorates each FRSView in-place with the
// source app name + restore-point creation time so the sessions
// table can disambiguate FRSes that share a generated name
// prefix (e.g. frs-wizard-abcde vs frs-wizard-fghij). Errors
// are logged and ignored — the table still renders with empty
// fields if K8s is unreachable.
func (s *Server) enrichFRSContext(ctx context.Context, list []k8s.FRSView) {
	if len(list) == 0 {
		return
	}
	// Internal fan-out, high frequency — Debug keeps the default
	// Info log stream focused on user-visible operations.
	s.log(ctx).Debug("sessions.enrich.start", "count", len(list))
	for i := range list {
		s.frs.LookupFRSSource(ctx, &list[i])
	}
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

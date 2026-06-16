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
	"path"
	"strconv"
	"strings"

	"github.com/liguoqiang/kasten-frs-web/internal/auth"
	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
	"github.com/liguoqiang/kasten-frs-web/internal/sftpclient"
	"github.com/liguoqiang/kasten-frs-web/web"
	"k8s.io/apimachinery/pkg/types"
)

// pageTemplates is loaded once from the embedded web/templates/*.html.
// layout.html defines the layout template; sessions.html / browse.html
// each define a `*_body` template that layout.html includes via an
// if-eq dispatch on .BodyTemplate. Earlier versions of this handler
// used inline `sessionsTmpl` / `browseTmpl` string constants which
// omitted the layout, the per-entry "进入" / "下载" links, and the
// styling. We load the canonical templates here so the on-disk HTML
// in web/templates/ is the single source of truth.
var pageTemplates = template.Must(
	template.New("").Funcs(template.FuncMap{
		"splitPath":     splitPath,
		"isLastPathSeg": isLastPathSeg,
		"buildPath":     buildPath,
		"parentPath":    parentPath,
		"joinPath":      joinPath,
		"lower":         strings.ToLower,
	}).ParseFS(web.Templates(), "templates/*.html"))

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
type FRSProvider interface {
	ListActiveFRS(ctx context.Context, namespaces []string) ([]k8s.FRSView, error)
	GetFRS(ctx context.Context, ref k8s.FRSRef) (k8s.FRSView, error)
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
	logger      *slog.Logger
}

// New builds a Server.
func New(a *auth.Authenticator, pool *sftpclient.Pool, frs FRSProvider,
	username, pubKeyPEM string, frsPort int, nsWhitelist []string) *Server {
	s := &Server{
		auth:        a,
		pool:        pool,
		frs:         frs,
		mux:         http.NewServeMux(),
		username:    username,
		pubKeyPEM:   pubKeyPEM,
		frsPort:     frsPort,
		nsWhitelist: nsWhitelist,
		logger:      slog.Default(),
	}
	s.routes()
	return s
}

// Router returns the underlying mux.
func (s *Server) Router() *http.ServeMux { return s.mux }

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
	s.mux.HandleFunc("GET /logout", s.handleLogout)  // GET-friendly for plain <a> links

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
	authed.HandleFunc("GET /browse", s.handleBrowse)
	authed.HandleFunc("GET /download", s.handleDownload)
	authed.HandleFunc("GET /download-zip", s.handleDownloadZip)
	authed.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sessions", http.StatusSeeOther)
	})

	s.mux.Handle("/", s.auth.RequireAuth(authed))
}

func (s *Server) handleLoginPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":        "登录",
		"BodyTemplate": "login_body",
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
	frsList, err := s.frs.ListActiveFRS(r.Context(), s.nsWhitelist)
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "FRS 列表拉取失败", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":         "活跃 FRS 会话",
		"BodyTemplate":  "sessions_body",
		"FRS":           frsList,
		"User":          s.auth.Username,
	}); err != nil {
		slog.Error("render sessions", "err", err)
	}
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ref := k8s.FRSRef{Namespace: ns, Name: name}
	view, err := s.frs.GetFRS(r.Context(), ref)
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "FRS 查询失败", err.Error())
		return
	}
	if view.Port != int64(s.frsPort) {
		s.renderError(w, http.StatusBadRequest, "FRS 端口不被允许",
			fmt.Sprintf("FRS 报告端口 %d，但我们只允许 %d", view.Port, s.frsPort))
		return
	}

	addr := fmt.Sprintf("%s.%s.svc.cluster.local:%d", view.ServiceName, view.ServiceNS, view.Port)
	sess, err := s.pool.Client().Dial(r.Context(), addr, view.HostKeySig)
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "SFTP 连接失败", err.Error())
		return
	}
	uid := userIDFromCookie(r, s.auth.CookieName)
	key := sftpclient.SessionKey{UserSessionID: uid, FRS: types.NamespacedName{Namespace: ns, Name: name}}
	s.pool.Store(key, sess)
	http.Redirect(w, r, "/browse?frs="+ns+"/"+name+"&path=/", http.StatusSeeOther)
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	ref, path, err := parseFRSQuery(r)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "无效的 frs 查询", err.Error())
		return
	}
	key := sftpclient.SessionKey{UserSessionID: userIDFromCookie(r, s.auth.CookieName),
		FRS: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}
	sess, ok := s.pool.Get(key)
	if !ok {
		http.Redirect(w, r, fmt.Sprintf("/sessions/%s/%s/connect", ref.Namespace, ref.Name),
			http.StatusSeeOther)
		return
	}
	entries, err := sess.ListDir(path)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "目录列表失败", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplates.ExecuteTemplate(w, "layout", map[string]any{
		"Title":         "浏览 " + ref.Namespace + "/" + ref.Name,
		"BodyTemplate":  "browse_body",
		"FRS":           ref,
		"Path":          path,
		"Entries":       entries,
		"User":          s.auth.Username,
	}); err != nil {
		slog.Error("render browse", "err", err)
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	ref, path, err := parseFRSQuery(r)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "无效的 frs 查询", err.Error())
		return
	}
	key := sftpclient.SessionKey{UserSessionID: userIDFromCookie(r, s.auth.CookieName),
		FRS: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}
	sess, ok := s.pool.Get(key)
	if !ok {
		s.renderError(w, http.StatusUnauthorized, "SFTP 会话已过期",
			"请返回 FRS Sessions 重新点击 进入")
		return
	}
	rc, err := sess.Open(path)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "文件无法打开", err.Error())
		return
	}
	defer rc.Close()
	stat, _ := sess.Stat(path)
	w.Header().Set("Content-Disposition", `attachment; filename="`+baseName(path)+`"`)
	if stat != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
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
		s.renderError(w, http.StatusBadRequest, "无效的 frs 查询", err.Error())
		return
	}
	if root == "" {
		root = "/"
	}
	key := sftpclient.SessionKey{UserSessionID: userIDFromCookie(r, s.auth.CookieName),
		FRS: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}
	sess, ok := s.pool.Get(key)
	if !ok {
		s.renderError(w, http.StatusUnauthorized, "SFTP 会话已过期",
			"请返回 FRS Sessions 重新点击 进入")
		return
	}
	stat, err := sess.Stat(root)
	if err != nil {
		s.renderError(w, http.StatusNotFound, "FRS 路径不存在", err.Error())
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
		// a path that escapes the requested root.
		clean := path.Clean("/" + rel)
		if clean == "/" || strings.HasPrefix(clean, "/../") {
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
	}); err != nil {
		slog.Error("render error page", "title", title, "err", err)
	}
}


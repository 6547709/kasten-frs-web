// Package handlers implements HTTP handlers for the helper.
package handlers

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/liguoqiang/kasten-frs-web/internal/auth"
	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
	"github.com/liguoqiang/kasten-frs-web/internal/sftpclient"
	"k8s.io/apimachinery/pkg/types"
)

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
	frsPort     int
	nsWhitelist []string
	logger      *slog.Logger
}

// New builds a Server.
func New(a *auth.Authenticator, pool *sftpclient.Pool, frs FRSProvider,
	username string, frsPort int, nsWhitelist []string) *Server {
	s := &Server{
		auth:        a,
		pool:        pool,
		frs:         frs,
		mux:         http.NewServeMux(),
		username:    username,
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

	authed := http.NewServeMux()
	authed.HandleFunc("GET /sessions", s.handleSessions)
	authed.HandleFunc("POST /sessions/{ns}/{name}/connect", s.handleConnect)
	authed.HandleFunc("GET /browse", s.handleBrowse)
	authed.HandleFunc("GET /download", s.handleDownload)
	authed.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sessions", http.StatusSeeOther)
	})

	s.mux.Handle("/", s.auth.RequireAuth(authed))
}

func (s *Server) handleLoginPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, loginPage)
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
		http.Error(w, "list FRS failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, err := template.New("sessions").Parse(sessionsTmpl)
	if err != nil {
		http.Error(w, "template", http.StatusInternalServerError)
		return
	}
	_ = tmpl.Execute(w, map[string]any{"FRS": frsList})
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	ref := k8s.FRSRef{Namespace: ns, Name: name}
	view, err := s.frs.GetFRS(r.Context(), ref)
	if err != nil {
		http.Error(w, "get FRS: "+err.Error(), http.StatusBadGateway)
		return
	}
	if view.Port != int64(s.frsPort) {
		http.Error(w, fmt.Sprintf("FRS port %d not allowed", view.Port),
			http.StatusBadRequest)
		return
	}

	addr := fmt.Sprintf("%s.%s.svc.cluster.local:%d", view.ServiceName, view.ServiceNS, view.Port)
	sess, err := s.pool.Client().Dial(r.Context(), addr, view.HostKeySig)
	if err != nil {
		http.Error(w, "SFTP connect: "+err.Error(), http.StatusBadGateway)
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
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		http.Error(w, "list: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, _ := template.New("browse").Parse(browseTmpl)
	_ = tmpl.Execute(w, map[string]any{
		"FRS":     ref,
		"Path":    path,
		"Entries": entries,
	})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	ref, path, err := parseFRSQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := sftpclient.SessionKey{UserSessionID: userIDFromCookie(r, s.auth.CookieName),
		FRS: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}
	sess, ok := s.pool.Get(key)
	if !ok {
		http.Error(w, "session expired", http.StatusUnauthorized)
		return
	}
	rc, err := sess.Open(path)
	if err != nil {
		http.Error(w, "open: "+err.Error(), http.StatusNotFound)
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

const loginPage = `<!doctype html><html><body><form method="post" action="/login">
<input name="username"><input name="password" type="password">
<button>Login</button></form></body></html>`

const sessionsTmpl = `<!doctype html><html><body>
<h1>Sessions</h1><table border=1><tr><th>ns</th><th>name</th><th>state</th><th>action</th></tr>
{{range .FRS}}<tr><td>{{.Ref.Namespace}}</td><td>{{.Ref.Name}}</td><td>{{.State}}</td>
<td><form method="post" action="/sessions/{{.Ref.Namespace}}/{{.Ref.Name}}/connect">
<button>connect</button></form></td></tr>{{end}}</table></body></html>`

const browseTmpl = `<!doctype html><html><body>
<h1>{{.FRS.Namespace}}/{{.FRS.Name}} at {{.Path}}</h1>
<ul>
{{range .Entries}}<li>{{.Name}} ({{.Size}} bytes)</li>{{end}}
</ul>
</body></html>`

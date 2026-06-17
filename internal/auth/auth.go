package auth

import (
	"crypto/subtle"
	"net/http"

	"github.com/liguoqiang/kasten-frs-web/internal/metrics"
)

// Authenticator holds the helper's single-account credentials and session store.
type Authenticator struct {
	Username   string
	Password   string
	Sessions   *SessionStore
	CookieName string
}

// NewAuthenticator builds an Authenticator.
func NewAuthenticator(username, password string, sessions *SessionStore, cookieName string) *Authenticator {
	return &Authenticator{
		Username:   username,
		Password:   password,
		Sessions:   sessions,
		CookieName: cookieName,
	}
}

// HandleLogin processes POST /login. On success sets the session cookie
// and 303s to /sessions. On failure responds 401 with a generic error.
func (a *Authenticator) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user := r.FormValue("username")
	pass := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(user), []byte(a.Username)) != 1 ||
		subtle.ConstantTimeCompare([]byte(pass), []byte(a.Password)) != 1 {
		metrics.LoginAttemptsTotal.WithLabelValues("failure").Inc()
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	_, cookie, err := a.Sessions.Issue()
	if err != nil {
		metrics.LoginAttemptsTotal.WithLabelValues("error").Inc()
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	metrics.LoginAttemptsTotal.WithLabelValues("success").Inc()
	http.SetCookie(w, &http.Cookie{
		Name:     a.CookieName,
		Value:    cookie,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(a.Sessions.TTL().Seconds()),
	})
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}

// RequireAuth wraps a handler, redirecting unauthenticated users to
// /login. For unsafe methods (POST/PUT/PATCH/DELETE) it additionally
// enforces a CSRF token bound to the session cookie: the request must
// carry a "csrf_token" form field (or X-CSRF-Token header) matching
// the token derived from the caller's session cookie. This defends
// against cross-site form posts even on browsers that ignore the
// cookie's SameSite=Strict attribute.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(a.CookieName)
		if err != nil || !a.Sessions.Verify(c.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if isUnsafeMethod(r.Method) {
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				// ParseForm is safe to call here; downstream
				// handlers call it again, which is idempotent.
				_ = r.ParseForm()
				token = r.FormValue("csrf_token")
			}
			if !a.Sessions.VerifyCSRF(c.Value, token) {
				http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// CSRFToken returns the CSRF token for the request's session cookie,
// or "" if the request carries no valid session cookie. Handlers pass
// this into templates so forms can embed it as a hidden field.
func (a *Authenticator) CSRFToken(r *http.Request) string {
	c, err := r.Cookie(a.CookieName)
	if err != nil {
		return ""
	}
	return a.Sessions.CSRFToken(c.Value)
}

func isUnsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

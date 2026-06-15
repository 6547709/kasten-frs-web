package auth

import (
	"crypto/subtle"
	"net/http"
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
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	_, cookie, err := a.Sessions.Issue()
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
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

// RequireAuth wraps a handler, redirecting unauthenticated users to /login.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(a.CookieName)
		if err != nil || !a.Sessions.Verify(c.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
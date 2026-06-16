package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestAuth(t *testing.T) *Authenticator {
	t.Helper()
	secret := make([]byte, 32)
	return &Authenticator{
		Username:   "admin",
		Password:   "secret",
		Sessions:   NewSessionStore(secret, time.Hour),
		CookieName: "kfrs_sid",
	}
}

func TestLogin_Success(t *testing.T) {
	a := newTestAuth(t)
	body := strings.NewReader("username=admin&password=secret")
	req := httptest.NewRequest(http.MethodPost, "/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	a.HandleLogin(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Set-Cookie"); !strings.HasPrefix(got, "kfrs_sid=") {
		t.Errorf("missing session cookie: %q", got)
	}
}

func TestLogin_BadPassword(t *testing.T) {
	a := newTestAuth(t)
	body := strings.NewReader("username=admin&password=wrong")
	req := httptest.NewRequest(http.MethodPost, "/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	a.HandleLogin(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestMiddleware_RequiresValidCookie(t *testing.T) {
	a := newTestAuth(t)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	a.RequireAuth(next).ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if called {
		t.Fatal("next handler should not be called when unauthenticated")
	}
}

func TestMiddleware_AllowsValidCookie(t *testing.T) {
	a := newTestAuth(t)
	_, cookie, _ := a.Sessions.Issue()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: a.CookieName, Value: cookie})
	rr := httptest.NewRecorder()
	a.RequireAuth(next).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !called {
		t.Fatal("next handler should be called when authenticated")
	}
}

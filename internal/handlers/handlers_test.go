package handlers

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/liguoqiang/kasten-frs-web/internal/auth"
	"github.com/liguoqiang/kasten-frs-web/internal/sftpclient"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	ts, cleanup := sftpclient.StartSFTPTestServer(t)
	t.Cleanup(cleanup)

	hostKeySig := "[" + ts.Addr().String() + "] " + ts.HostKeyString()
	c, _ := sftpclient.NewClient(sftpclient.ClientConfig{
		Username:   "root",
		Signer:     ts.Signer(),
		HostKeySig: hostKeySig,
	})
	pool := sftpclient.NewPool(c, 30*time.Minute)

	a := auth.NewAuthenticator("admin", "secret",
		auth.NewSessionStore([]byte("0123456789abcdef0123456789abcdef"), 8*60*60*1e9),
		"kfrs_sid",
	)
	srv := New(a, pool, FakeFRSProvider(t, ts.Addr().String(), hostKeySig),
		"root", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEY test@local", 2222, []string{ts.Addr().String()},
		30*time.Second, "test")
	return srv
}

func TestRoot_RedirectsToLogin(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Location"), "/login") {
		t.Errorf("location = %q", rr.Header().Get("Location"))
	}
}

func TestLoginFlow(t *testing.T) {
	s := newTestServer(t)
	form := url.Values{"username": {"admin"}, "password": {"secret"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d", rr.Code)
	}
	ck := rr.Header().Get("Set-Cookie")
	if !strings.HasPrefix(ck, "kfrs_sid=") {
		t.Fatalf("no cookie: %q", ck)
	}
	cookieValue := strings.SplitN(ck, "=", 2)[1]
	cookieValue = strings.SplitN(cookieValue, ";", 2)[0]

	// GET /sessions with cookie
	req2 := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req2.AddCookie(&http.Cookie{Name: "kfrs_sid", Value: cookieValue})
	rr2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("sessions status = %d", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "my-frs") {
		t.Errorf("expected FRS name in body, got: %q", rr2.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
}

// silence unused
var _ = base64.RawURLEncoding
var _ = context.Background
var _ = time.Minute

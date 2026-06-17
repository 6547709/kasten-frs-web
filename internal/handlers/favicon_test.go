package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFaviconServed(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/favicon.png", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("favicon status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct == "" {
		t.Fatalf("favicon content-type empty")
	} else {
		t.Logf("favicon content-type = %s, bytes = %d", ct, rr.Body.Len())
	}
	if rr.Body.Len() == 0 {
		t.Fatal("favicon body empty")
	}
}

func TestLoginPageHasFaviconLink(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d", rr.Code)
	}
	if body := rr.Body.String(); !contains(body, `rel="icon"`) || !contains(body, "/static/favicon.png") {
		t.Fatalf("login head missing favicon link")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

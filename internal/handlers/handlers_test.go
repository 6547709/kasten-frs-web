package handlers

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

func TestCSRF_EnforcedOnPost(t *testing.T) {
	s := newTestServer(t)
	// Log in to obtain a valid session cookie.
	form := url.Values{"username": {"admin"}, "password": {"secret"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	ck := rr.Header().Get("Set-Cookie")
	cookieValue := strings.SplitN(strings.SplitN(ck, "=", 2)[1], ";", 2)[0]
	cookie := &http.Cookie{Name: "kfrs_sid", Value: cookieValue}

	// POST without a CSRF token → 403.
	noTok := httptest.NewRequest(http.MethodPost, "/sessions/default/my-frs/delete", nil)
	noTok.AddCookie(cookie)
	rrNo := httptest.NewRecorder()
	s.Router().ServeHTTP(rrNo, noTok)
	if rrNo.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF token: status = %d, want 403", rrNo.Code)
	}

	// POST with the correct CSRF token (header form) → not 403.
	token := s.auth.Sessions.CSRFToken(cookieValue)
	withTok := httptest.NewRequest(http.MethodPost, "/sessions/default/my-frs/delete", nil)
	withTok.AddCookie(cookie)
	withTok.Header.Set("X-CSRF-Token", token)
	rrYes := httptest.NewRecorder()
	s.Router().ServeHTTP(rrYes, withTok)
	if rrYes.Code == http.StatusForbidden {
		t.Fatalf("POST with valid CSRF token was rejected (403)")
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

// TestNewBrowseEntry locks the viewmodel contract. The point of
// newBrowseEntry is to give the html/template a struct whose
// fields it can always find via reflection, even when the
// underlying os.FileInfo is pkg/sftp's concrete *fs.fileInfo
// (which doesn't expose ResolvedPath() as a method template
// can see). If this test changes, re-read the template's
// range over .Entries to make sure all fields it touches are
// still populated.
func TestNewBrowseEntry(t *testing.T) {
	t.Run("regular dir uses parent/name as ClickPath", func(t *testing.T) {
		dir, err := os.Stat("/tmp")
		if err != nil {
			t.Skip("needs /tmp")
		}
		v := newBrowseEntry(dir, "/parent")
		if v.Name != "tmp" {
			t.Errorf("Name = %q, want tmp", v.Name)
		}
		if !v.IsDir {
			t.Error("IsDir should be true for /tmp")
		}
		if v.ClickPath != "/parent/tmp" {
			t.Errorf("ClickPath = %q, want /parent/tmp", v.ClickPath)
		}
		if v.IsSymlink {
			t.Error("IsSymlink should be false for /tmp")
		}
	})

	t.Run("symlink with ResolvedPath uses resolved path", func(t *testing.T) {
		fi := &fileInfoWithDirStub{
			name:     "Documents and Settings",
			resolved: "/parent/Users",
			mode:     os.ModeSymlink | os.ModeDir,
		}
		v := newBrowseEntry(fi, "/parent")
		if v.Name != "Documents and Settings" {
			t.Errorf("Name = %q", v.Name)
		}
		if !v.IsDir {
			t.Error("IsDir should be true (wrapper returned true)")
		}
		if v.ClickPath != "/parent/Users" {
			t.Errorf("ClickPath = %q, want /parent/Users (resolved, not literal)", v.ClickPath)
		}
		if !v.IsSymlink {
			t.Error("IsSymlink should be true (Mode has ModeSymlink)")
		}
	})

	t.Run("plain file uses parent/name", func(t *testing.T) {
		fi := &fileInfoWithDirStub{name: "readme.txt", mode: 0}
		v := newBrowseEntry(fi, "/parent")
		if v.IsDir {
			t.Error("IsDir should be false")
		}
		if v.ClickPath != "/parent/readme.txt" {
			t.Errorf("ClickPath = %q, want /parent/readme.txt", v.ClickPath)
		}
	})
}

// fileInfoWithDirStub implements os.FileInfo + a ResolvedPath()
// method so we can exercise newBrowseEntry's wrapper-aware branch
// without standing up the full SFTP stack.
type fileInfoWithDirStub struct {
	name     string
	resolved string
	mode     os.FileMode // explicit mode so tests can choose symlink-vs-file
}

func (f *fileInfoWithDirStub) Name() string       { return f.name }
func (f *fileInfoWithDirStub) Size() int64        { return 0 }
func (f *fileInfoWithDirStub) Mode() os.FileMode  { return f.mode }
func (f *fileInfoWithDirStub) ModTime() time.Time { return time.Time{} }
func (f *fileInfoWithDirStub) IsDir() bool        { return f.Mode()&os.ModeDir != 0 }
func (f *fileInfoWithDirStub) Sys() any           { return nil }
func (f *fileInfoWithDirStub) ResolvedPath() string {
	if f.resolved == "" {
		// For the "plain file uses parent/name" case we DON'T
		// want the wrapper to claim a resolved path; the wrapper
		// is supposed to be opt-in via ResolvedPath().
		return ""
	}
	return f.resolved
}

// silence unused
var _ = base64.RawURLEncoding
var _ = context.Background
var _ = time.Minute

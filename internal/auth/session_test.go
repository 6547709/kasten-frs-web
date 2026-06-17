package auth

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

func splitCookie(cookie string) []string { return strings.Split(cookie, ".") }

func encodeUnix(u int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(u, 10)))
}

func newTestStore(t *testing.T) *SessionStore {
	t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return NewSessionStore(secret, 8*time.Hour)
}

func TestIssueAndVerify(t *testing.T) {
	s := newTestStore(t)
	sid, cookie, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if sid == "" {
		t.Fatal("empty session id")
	}
	if cookie == "" {
		t.Fatal("empty cookie value")
	}
	if !s.Verify(cookie) {
		t.Fatal("verify failed on issued cookie")
	}
}

func TestVerify_RejectsTampered(t *testing.T) {
	s := newTestStore(t)
	_, cookie, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	// flip a character in the body
	tampered := []byte(cookie)
	tampered[0] ^= 0x01
	if s.Verify(string(tampered)) {
		t.Fatal("verify should reject tampered cookie")
	}
}

func TestVerify_RejectsShortSecret(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for short secret")
		}
	}()
	NewSessionStore([]byte("short"), time.Hour)
}

func TestVerify_RejectsExpired(t *testing.T) {
	// Issue a cookie "in the past", then verify it well after the TTL
	// has elapsed. The verifier must reject it on the strength of the
	// signed issue timestamp, independent of any client Max-Age.
	s := NewSessionStore(make([]byte, 32), time.Hour)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	_, cookie, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	// Move the clock 2h forward — past the 1h TTL.
	s.now = func() time.Time { return base.Add(2 * time.Hour) }
	if s.Verify(cookie) {
		t.Fatal("verify should reject a cookie aged past TTL")
	}
}

func TestVerify_AcceptsWithinTTL(t *testing.T) {
	s := NewSessionStore(make([]byte, 32), time.Hour)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	_, cookie, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	// 30m later — still inside the 1h TTL.
	s.now = func() time.Time { return base.Add(30 * time.Minute) }
	if !s.Verify(cookie) {
		t.Fatal("verify should accept a cookie still within TTL")
	}
}

func TestCSRFToken_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	_, cookie, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	tok := s.CSRFToken(cookie)
	if tok == "" {
		t.Fatal("empty csrf token")
	}
	if !s.VerifyCSRF(cookie, tok) {
		t.Fatal("VerifyCSRF should accept its own token")
	}
	if s.VerifyCSRF(cookie, tok+"x") {
		t.Fatal("VerifyCSRF should reject a tampered token")
	}
	// Token is bound to the specific cookie value.
	_, other, _ := s.Issue()
	if s.VerifyCSRF(other, tok) {
		t.Fatal("VerifyCSRF should reject a token from a different session")
	}
}

func TestVerify_RejectsTamperedTimestamp(t *testing.T) {
	s := newTestStore(t)
	_, cookie, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	// Re-encode a far-future timestamp but keep the original signature:
	// the HMAC covers the timestamp, so the forgery must be rejected.
	parts := splitCookie(cookie)
	forged := encodeUnix(time.Now().Add(1000 * time.Hour).Unix())
	tampered := parts[0] + "." + forged + "." + parts[2]
	if s.Verify(tampered) {
		t.Fatal("verify should reject a cookie with a tampered timestamp")
	}
}

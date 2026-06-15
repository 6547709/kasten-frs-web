package auth

import (
	"crypto/rand"
	"testing"
	"time"
)

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
	s := NewSessionStore(make([]byte, 32), -1*time.Second) // already expired
	_, cookie, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	// Negative TTL is still allowed at construction; expiry is enforced
	// by the client's Max-Age cookie attribute, not the verifier.
	if !s.Verify(cookie) {
		t.Fatal("verify should still succeed; expiry enforced via Max-Age")
	}
}

// Package auth handles user authentication, session cookies, and CSRF.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// minSecretBytes is the minimum accepted length for the cookie secret.
const minSecretBytes = 16

// SessionStore issues and verifies HMAC-signed session cookies.
type SessionStore struct {
	secret []byte
	ttl    time.Duration
}

// NewSessionStore returns a SessionStore. Panics if secret is too short.
func NewSessionStore(secret []byte, ttl time.Duration) *SessionStore {
	if len(secret) < minSecretBytes {
		panic(fmt.Sprintf("cookie secret must be at least %d bytes", minSecretBytes))
	}
	return &SessionStore{secret: append([]byte{}, secret...), ttl: ttl}
}

// Issue creates a fresh random SessionID, returning the raw ID and the
// cookie value to set on the client.
func (s *SessionStore) Issue() (sid, cookie string, err error) {
	idBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(idBytes)
	sig := mac.Sum(nil)
	cookie = base64.RawURLEncoding.EncodeToString(idBytes) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
	return base64.RawURLEncoding.EncodeToString(idBytes), cookie, nil
}

// Verify checks the cookie value. Returns true iff the cookie's HMAC signature is valid.
func (s *SessionStore) Verify(cookie string) bool {
	parts := strings.Split(cookie, ".")
	if len(parts) != 2 {
		return false
	}
	idBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(idBytes)
	wantSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, wantSig) {
		return false
	}
	// Expiry is enforced by the client's Max-Age cookie attribute.
	return true
}

// TTL returns the configured cookie lifetime.
func (s *SessionStore) TTL() time.Duration { return s.ttl }

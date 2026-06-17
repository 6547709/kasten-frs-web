// Package auth handles user authentication, session cookies, and CSRF.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// minSecretBytes is the minimum accepted length for the cookie secret.
const minSecretBytes = 16

// SessionStore issues and verifies HMAC-signed session cookies.
type SessionStore struct {
	secret []byte
	ttl    time.Duration
	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// NewSessionStore returns a SessionStore. Panics if secret is too short.
func NewSessionStore(secret []byte, ttl time.Duration) *SessionStore {
	if len(secret) < minSecretBytes {
		panic(fmt.Sprintf("cookie secret must be at least %d bytes", minSecretBytes))
	}
	return &SessionStore{secret: append([]byte{}, secret...), ttl: ttl, now: time.Now}
}

// clock returns the configured time source, defaulting to time.Now.
func (s *SessionStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Issue creates a fresh random SessionID, returning the raw ID and the
// cookie value to set on the client.
//
// Cookie format: base64(id).base64(issuedUnix).base64(sig), where the
// HMAC signature covers BOTH the random id and the issue timestamp.
// Binding the timestamp into the signature lets Verify enforce expiry
// server-side: a client cannot extend its session by editing the
// cookie's Max-Age (or the embedded timestamp) because any change
// invalidates the HMAC.
func (s *SessionStore) Issue() (sid, cookie string, err error) {
	idBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	issued := strconv.FormatInt(s.clock().Unix(), 10)
	idB64 := base64.RawURLEncoding.EncodeToString(idBytes)
	tsB64 := base64.RawURLEncoding.EncodeToString([]byte(issued))
	sig := s.sign(idB64, tsB64)
	cookie = idB64 + "." + tsB64 + "." + base64.RawURLEncoding.EncodeToString(sig)
	return idB64, cookie, nil
}

// sign computes the HMAC-SHA256 over the id and timestamp components.
func (s *SessionStore) sign(idB64, tsB64 string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(idB64))
	mac.Write([]byte{'.'})
	mac.Write([]byte(tsB64))
	return mac.Sum(nil)
}

// Verify checks the cookie value. Returns true iff the cookie's HMAC
// signature is valid AND the embedded issue timestamp has not aged
// past the configured TTL.
func (s *SessionStore) Verify(cookie string) bool {
	parts := strings.Split(cookie, ".")
	if len(parts) != 3 {
		return false
	}
	idB64, tsB64, sigB64 := parts[0], parts[1], parts[2]
	gotSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	wantSig := s.sign(idB64, tsB64)
	if !hmac.Equal(gotSig, wantSig) {
		return false
	}
	// Signature is valid, so the timestamp is authentic — enforce
	// server-side expiry. A non-positive TTL means "no expiry".
	tsRaw, err := base64.RawURLEncoding.DecodeString(tsB64)
	if err != nil {
		return false
	}
	issuedUnix, err := strconv.ParseInt(string(tsRaw), 10, 64)
	if err != nil {
		return false
	}
	if s.ttl > 0 {
		expiry := time.Unix(issuedUnix, 0).Add(s.ttl)
		if s.clock().After(expiry) {
			return false
		}
	}
	return true
}

// TTL returns the configured cookie lifetime.
func (s *SessionStore) TTL() time.Duration { return s.ttl }

// CSRFToken derives a stateless CSRF token bound to a specific session
// cookie. token = base64(HMAC(secret, "csrf:" + cookieValue)). Because
// it is keyed by the server-only secret AND the per-session cookie, an
// attacker cannot forge it without already knowing the victim's cookie
// (which HttpOnly + SameSite already protect). Embedding it in forms
// and re-checking on unsafe requests defends against cross-site POSTs
// even on browsers that ignore SameSite.
func (s *SessionStore) CSRFToken(cookieValue string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("csrf:"))
	mac.Write([]byte(cookieValue))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyCSRF reports whether token matches the expected CSRF token for
// the given session cookie, using a constant-time comparison.
func (s *SessionStore) VerifyCSRF(cookieValue, token string) bool {
	want := s.CSRFToken(cookieValue)
	return hmac.Equal([]byte(want), []byte(token))
}

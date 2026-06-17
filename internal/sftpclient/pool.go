package sftpclient

import (
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// SessionKey identifies a pooled session.
type SessionKey struct {
	UserSessionID string
	FRS           types.NamespacedName
}

// poolEntry wraps a stored Session with usage timestamps.
type poolEntry struct {
	sess       *Session
	lastUsedAt time.Time
}

// Pool stores SFTP sessions keyed by user+FRS, expiring idle entries.
type Pool struct {
	mu      sync.RWMutex
	entries map[SessionKey]*poolEntry
	ttl     time.Duration
	client  *Client
}

// NewPool creates a Pool. TTL is the idle expiry window.
func NewPool(client *Client, ttl time.Duration) *Pool {
	return &Pool{
		entries: make(map[SessionKey]*poolEntry),
		ttl:     ttl,
		client:  client,
	}
}

// Store inserts a session under key.
func (p *Pool) Store(key SessionKey, sess *Session) {
	p.mu.Lock()
	p.entries[key] = &poolEntry{sess: sess, lastUsedAt: time.Now()}
	p.mu.Unlock()
}

// Get returns the session and bumps lastUsedAt.
func (p *Pool) Get(key SessionKey) (*Session, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if !ok {
		slog.Debug("sftp.pool.miss", "frs", key.FRS.Namespace+"/"+key.FRS.Name)
		return nil, false
	}
	if time.Since(e.lastUsedAt) > p.ttl {
		slog.Info("sftp.pool.expired",
			"frs", key.FRS.Namespace+"/"+key.FRS.Name,
			"idle", time.Since(e.lastUsedAt).String(),
		)
		delete(p.entries, key)
		go e.sess.Close()
		return nil, false
	}
	e.lastUsedAt = time.Now()
	slog.Debug("sftp.pool.hit", "frs", key.FRS.Namespace+"/"+key.FRS.Name)
	return e.sess, true
}

// Close removes a session and closes the underlying SFTP connection.
func (p *Pool) Close(key SessionKey) {
	p.mu.Lock()
	e, ok := p.entries[key]
	if ok {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	if ok {
		_ = e.sess.Close()
	}
}

// Sweep removes all expired entries. Intended for a periodic timer.
func (p *Pool) Sweep() {
	p.mu.Lock()
	var toClose []*Session
	now := time.Now()
	for k, e := range p.entries {
		if now.Sub(e.lastUsedAt) > p.ttl {
			delete(p.entries, k)
			toClose = append(toClose, e.sess)
		}
	}
	p.mu.Unlock()
	if n := len(toClose); n > 0 {
		slog.Info("sftp.pool.sweep", "closed", n)
	}
	for _, s := range toClose {
		_ = s.Close()
	}
}

// Client returns the underlying SFTP client used for new dials.
func (p *Pool) Client() *Client { return p.client }

// CloseAllForFRS closes all pooled SFTP sessions associated with a particular FRS ref.
// Called when the user clicks "End and delete" to release the helper's
// connection promptly (instead of waiting for the 30m idle TTL).
func (p *Pool) CloseAllForFRS(ns, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.entries {
		if k.FRS.Namespace == ns && k.FRS.Name == name {
			delete(p.entries, k)
			go e.sess.Close()
		}
	}
}

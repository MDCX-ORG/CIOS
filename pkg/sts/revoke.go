// pkg/sts/revoke.go — Revoker interface and in-memory jti
// blacklist (PRMT-103 §4, §5).
//
// The Revoker interface exists so a future PRMT can swap in a
// Redis-backed implementation without touching the STS code
// path. This PRMT ships only the in-memory implementation;
// production deployments that need cross-instance revocation
// will provide their own Revoker (out of scope here).
package sts

import "sync"

// Revoker is the abstraction over the jti blacklist. The two
// methods are intentionally minimal so any backing store (mem,
// Redis, NATS KV) can satisfy it.
//
// PRMT-103 §6 forbids leaking revocation reasons to the client,
// so both methods return only what callers need: a yes/no on
// IsRevoked, and nothing on Revoke (fire-and-forget).
type Revoker interface {
	// IsRevoked returns true iff the given jti has been revoked.
	IsRevoked(jti string) bool
	// Revoke records jti as revoked. Implementations should be
	// idempotent — revoking an already-revoked jti is a no-op.
	Revoke(jti string)
}

// NewMemRevoker returns an in-memory Revoker backed by a map
// guarded by an RWMutex. The map is process-local; revoking on
// one gateway instance does NOT propagate to others. Cross-
// instance revocation is intentionally out of scope for
// PRMT-103 — the §6 MUST NOT list is silent on the matter, and
// spec-009 §7.1 does not pin a backing store.
//
// Reads dominate (every Verify call hits IsRevoked), so RWMutex
// avoids serialising verification on a plain Mutex.
func NewMemRevoker() Revoker {
	return &memRevoker{}
}

type memRevoker struct {
	mu      sync.RWMutex
	revoked map[string]struct{}
}

// IsRevoked returns true iff jti has been revoked. Reading under
// RLock allows concurrent verifies; Revoke takes the write lock.
func (m *memRevoker) IsRevoked(jti string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.revoked[jti]
	return ok
}

// Revoke records jti as revoked. Idempotent: revoking twice does
// not error and does not change observable state.
func (m *memRevoker) Revoke(jti string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revoked == nil {
		m.revoked = make(map[string]struct{})
	}
	m.revoked[jti] = struct{}{}
}

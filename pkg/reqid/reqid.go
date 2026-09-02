// Package reqid — single source of truth for the per-request id
// shape "01H" + 16 uppercase base32 chars from 10 random bytes.
// Merged in PRMT-030 §A from core.newRequestID and cli.newRequestID.
// Both were byte-for-byte equivalent; the spec requirement is
// PRMT-011 §4.2 (uniqueness within the dedup window — a real
// UUIDv7 is M1).
package reqid

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// New returns a non-cryptographic ULID-ish identifier: "01H" + 16
// uppercase base32 chars from 10 random bytes (trimming the trailing
// '=' padding character that base32 emits). Two implementations of
// this existed pre-PRMT-030 (core + cli); this package is the single
// authority both import.
func New() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "01H" + strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), "=")
}

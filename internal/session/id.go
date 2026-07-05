// Package session implements the Agent conversation and memory subsystem
// described in section 7.7 of the go-rewrite design doc. It owns the Session
// lifecycle (create / append / commit / archive), conversation compression
// (v2 truncation and v3 summary-based), memory extraction, and skill export.
//
// The package depends only on the standard library, the project's own
// internal/domain types, and testify for tests. The LLM is abstracted behind
// the LLMClient interface so unit tests inject fakes and P12 wires the real
// internal/models/vlm client.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// newID returns a process-unique identifier of the form "<prefix>_<hex>".
// It uses crypto/rand so IDs are unpredictable across callers without
// pulling in an extra ULID/UUID dependency. The prefix carries the entity
// kind (e.g. "sess", "turn", "mem") for log readability.
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback to timestamp-based ID if entropy is unavailable.
		// This should never happen in practice on Linux getrandom.
		return fmt.Sprintf("%s_%x", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// newTurnID returns an identifier for a Turn.
func newTurnID() string { return newID("turn") }

// newSessionID returns an identifier for a Session.
func newSessionID() string { return newID("sess") }

// newMemoryID returns an identifier for an ExtractedMemory.
func newMemoryID() string { return newID("mem") }

// now returns the current wall-clock time. It is a package-level variable so
// tests can substitute a deterministic clock.
var now = func() time.Time { return time.Now().UTC() }

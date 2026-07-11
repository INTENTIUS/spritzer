// Package id generates identifiers in the shapes spritzer uses. A sprite's id
// is normally the caller-chosen name, and a checkpoint's id is its label; these
// helpers mint fallbacks when a caller does not supply one, and mirror the
// identifier utilities in the sibling mudflaps emulator.
package id

import (
	"crypto/rand"
	"encoding/hex"
)

// Sprite returns a 14-character hex identifier, suitable as a fallback sprite id
// when a caller does not name one.
func Sprite() string {
	b := make([]byte, 7)
	mustRead(b)
	return "sprite_" + hex.EncodeToString(b)
}

// Checkpoint returns a "cp_"-prefixed hex identifier, suitable as a fallback
// checkpoint id.
func Checkpoint() string {
	b := make([]byte, 6)
	mustRead(b)
	return "cp_" + hex.EncodeToString(b)
}

// Nonce returns a random hex string.
func Nonce() string {
	b := make([]byte, 16)
	mustRead(b)
	return hex.EncodeToString(b)
}

func mustRead(b []byte) {
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read never fails on the platforms we target; if it
		// somehow does there is nothing sensible to recover to.
		panic("spritzer: entropy source unavailable: " + err.Error())
	}
}

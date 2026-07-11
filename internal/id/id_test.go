package id

import (
	"strings"
	"testing"
)

func TestSpriteIDsAreUniqueAndPrefixed(t *testing.T) {
	a, b := Sprite(), Sprite()
	if a == b {
		t.Fatalf("two Sprite() ids collided: %q", a)
	}
	if !strings.HasPrefix(a, "sprite_") {
		t.Fatalf("Sprite() = %q, want sprite_ prefix", a)
	}
}

func TestCheckpointAndNonceShapes(t *testing.T) {
	if cp := Checkpoint(); !strings.HasPrefix(cp, "cp_") {
		t.Fatalf("Checkpoint() = %q, want cp_ prefix", cp)
	}
	if n := Nonce(); len(n) != 32 {
		t.Fatalf("Nonce() length = %d, want 32 hex chars", len(n))
	}
}

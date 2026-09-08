package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Refresh tokens carry no structure and no claims — their only job is to be
// unguessable and to hash to a stable lookup key.

func TestNewRefreshToken_IsOpaqueAndHighEntropy(t *testing.T) {
	raw, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken: %v", err)
	}

	// 32 random bytes, base64url without padding.
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if len(decoded) != 32 {
		t.Errorf("entropy = %d bytes, want 32", len(decoded))
	}

	// It must not be a JWT: the whole point of 00013 is that the refresh
	// credential carries no self-contained authority.
	if strings.Count(raw, ".") == 2 {
		t.Error("refresh token looks like a JWT; it must be opaque")
	}

	if hash == raw {
		t.Error("stored hash must never equal the token itself")
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars of SHA-256", len(hash))
	}
	if strings.Contains(hash, raw) {
		t.Error("hash must not contain the raw token")
	}
}

func TestNewRefreshToken_IsUnique(t *testing.T) {
	seen := make(map[string]bool, 256)
	for i := 0; i < 256; i++ {
		raw, _, err := NewRefreshToken()
		if err != nil {
			t.Fatalf("NewRefreshToken: %v", err)
		}
		if seen[raw] {
			t.Fatal("crypto/rand produced a duplicate refresh token")
		}
		seen[raw] = true
	}
}

func TestHashRefreshToken_IsDeterministic(t *testing.T) {
	raw, hash, _ := NewRefreshToken()
	if got := HashRefreshToken(raw); got != hash {
		t.Errorf("hash is not stable: %q vs %q", got, hash)
	}
	// A different token must not collide.
	other, _, _ := NewRefreshToken()
	if HashRefreshToken(other) == hash {
		t.Error("distinct tokens hashed to the same value")
	}
}

func TestSession_IsImpersonation(t *testing.T) {
	if (&Session{Kind: SessionNormal}).IsImpersonation() {
		t.Error("normal session reported as impersonation")
	}
	if !(&Session{Kind: SessionImpersonation}).IsImpersonation() {
		t.Error("impersonation session not reported as such")
	}
}

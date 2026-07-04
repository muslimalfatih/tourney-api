package auth

import (
	"strings"
	"testing"
)

func TestHashPassword_RoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash is not argon2id PHC format: %q", hash)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("correct password did not verify")
	}
}

func TestVerifyPassword_WrongPassword(t *testing.T) {
	hash, _ := HashPassword("the-right-one")
	ok, err := VerifyPassword("the-wrong-one", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("wrong password verified as correct")
	}
}

func TestHashPassword_SaltIsRandom(t *testing.T) {
	// The same password must hash to different strings (random salt), yet both
	// must verify.
	h1, _ := HashPassword("same")
	h2, _ := HashPassword("same")
	if h1 == h2 {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
	if ok, _ := VerifyPassword("same", h1); !ok {
		t.Error("h1 did not verify")
	}
	if ok, _ := VerifyPassword("same", h2); !ok {
		t.Error("h2 did not verify")
	}
}

func TestVerifyPassword_MalformedHash(t *testing.T) {
	if _, err := VerifyPassword("x", "not-a-valid-hash"); err == nil {
		t.Error("expected an error for a malformed hash")
	}
}

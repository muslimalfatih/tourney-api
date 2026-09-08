package auth

import (
	"strconv"
	"strings"
	"testing"
)

const testPepper = "test-pepper-at-least-16-chars-long"

func TestGenerateCode_ShapeAndLeadingZeros(t *testing.T) {
	sawLeadingZero := false
	for i := 0; i < 3000; i++ {
		code, err := GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("code %q has length %d, want 6", code, len(code))
		}
		if _, err := strconv.Atoi(code); err != nil {
			t.Fatalf("code %q is not numeric", code)
		}
		if code[0] == '0' {
			sawLeadingZero = true
		}
	}
	// Leading zeros must survive. If they were trimmed the space would shrink
	// from 10^6 to 9*10^5 and nobody would notice.
	if !sawLeadingZero {
		t.Error("no code with a leading zero in 3000 draws; formatting may be trimming them")
	}
}

func TestGenerateCode_CoversTheRange(t *testing.T) {
	// A crude uniformity smoke test: with 6000 draws every leading digit
	// should appear. Catches a broken generator or a biased modulo.
	seen := map[byte]bool{}
	for i := 0; i < 6000; i++ {
		code, _ := GenerateCode()
		seen[code[0]] = true
	}
	for d := byte('0'); d <= '9'; d++ {
		if !seen[d] {
			t.Errorf("leading digit %q never appeared in 6000 draws", string(d))
		}
	}
}

func TestGenerateCode_IsNotPredictable(t *testing.T) {
	seen := make(map[string]int)
	for i := 0; i < 2000; i++ {
		code, _ := GenerateCode()
		seen[code]++
	}
	// 2000 draws from 10^6 should almost never repeat, and certainly never
	// cluster. Anything appearing three times means the source is not random.
	for code, n := range seen {
		if n > 2 {
			t.Errorf("code %q drawn %d times in 2000; generator is not uniform", code, n)
		}
	}
}

func TestHashCode_NeverContainsTheCode(t *testing.T) {
	const code = "482913"
	h := HashCode("user@example.com", code, testPepper)
	if strings.Contains(h, code) {
		t.Error("stored hash contains the plaintext code")
	}
	if len(h) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(h))
	}
}

func TestVerifyCode_RoundTrip(t *testing.T) {
	const email, code = "user@example.com", "482913"
	h := HashCode(email, code, testPepper)

	if !VerifyCode(email, code, h, testPepper) {
		t.Error("correct code failed to verify")
	}
	if VerifyCode(email, "482914", h, testPepper) {
		t.Error("wrong code verified")
	}
}

// The property the spec calls out explicitly: a code issued for one address
// must not authenticate another. Binding the address into the HMAC makes that
// arithmetic rather than a matter of getting the WHERE clause right.
func TestVerifyCode_IsBoundToItsEmail(t *testing.T) {
	const code = "482913"
	h := HashCode("alice@example.com", code, testPepper)

	if VerifyCode("bob@example.com", code, h, testPepper) {
		t.Error("a code issued for alice verified for bob")
	}
	// Case and whitespace must not defeat the binding either.
	if !VerifyCode("  Alice@Example.COM ", code, h, testPepper) {
		t.Error("normalization mismatch: the same address failed to verify")
	}
}

func TestVerifyCode_DependsOnThePepper(t *testing.T) {
	const email, code = "user@example.com", "482913"
	h := HashCode(email, code, testPepper)

	// Someone who reads the table but not the pepper cannot check a guess.
	if VerifyCode(email, code, h, "a-different-pepper-16-chars") {
		t.Error("code verified under the wrong pepper; the pepper is not keying the HMAC")
	}
}

package auth

import "testing"

func TestNormalizeEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"organizer@example.com", "organizer@example.com"},
		{"Organizer@Example.COM", "organizer@example.com"},
		{"  spaced@example.com  ", "spaced@example.com"},
		{"\tTabbed@Example.com\n", "tabbed@example.com"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeEmail(c.in); got != c.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Plus-addressing and dots must survive: collapsing them would let one
// invitation admit addresses the admin never approved.
func TestNormalizeEmail_KeepsPlusTagsAndDots(t *testing.T) {
	for _, in := range []string{"a+tag@example.com", "first.last@example.com"} {
		if got := NormalizeEmail(in); got != in {
			t.Errorf("NormalizeEmail(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestNormalizeEmail_IsIdempotent(t *testing.T) {
	once := NormalizeEmail("  MiXeD@Example.Com ")
	if twice := NormalizeEmail(once); twice != once {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
}

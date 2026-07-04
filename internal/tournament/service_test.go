package tournament

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Bali Open 2026":     "bali-open-2026",
		"  Ubud   Masters  ": "ubud-masters",
		"Men's Singles!":     "men-s-singles",
		"UPPER lower":        "upper-lower",
		"already-a-slug":     "already-a-slug",
		"--edge--":           "edge",
		"":                   "tournament", // empty falls back
		"###":                "tournament", // all-symbol falls back
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDate(t *testing.T) {
	// Valid date.
	got, err := parseDate("2026-08-12")
	if err != nil {
		t.Fatalf("parseDate valid: %v", err)
	}
	if got == nil || got.Year() != 2026 || got.Month() != 8 || got.Day() != 12 {
		t.Errorf("parseDate returned wrong date: %v", got)
	}

	// Empty is allowed (nil, no error).
	if d, err := parseDate("  "); err != nil || d != nil {
		t.Errorf("empty date should be (nil, nil), got (%v, %v)", d, err)
	}

	// Invalid format errors.
	if _, err := parseDate("12/08/2026"); err == nil {
		t.Error("invalid date format should error")
	}
}

package event

import "testing"

func TestSlugify(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Mixed Doubles", "mixed-doubles"},
		{"Panjer CUP", "panjer-cup"},
		{"Café Ünïcode", "cafe-unicode"}, // accents fold to ASCII, not dropped
		{"  Men's  Singles!! ", "men-s-singles"},
		{"???", ""}, // nothing survives -> caller falls back to "division"
		{"U19 / Open", "u19-open"},
	} {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildSlug(t *testing.T) {
	taken := map[string]bool{}
	for _, c := range []struct{ name, category, want string }{
		{"Mens Singles", "Beginner", "mens-singles"},                  // plain name, first come
		{"Mens Singles", "Intermediate", "mens-singles-intermediate"}, // name+category qualifies
		{"Mens Singles", "Beginner", "mens-singles-beginner"},         // name taken, name+category still free
		{"Mens Singles", "Beginner", "mens-singles-2"},                // now name AND name+category both taken
		{"???", "", "division"},                                       // empty slug falls back
		{"Bracket", "", "bracket-2"},                                  // reserved word never handed out
	} {
		got := buildSlug(c.name, c.category, taken)
		if got != c.want {
			t.Errorf("buildSlug(%q,%q) = %q, want %q", c.name, c.category, got, c.want)
		}
		taken[got] = true
	}
}

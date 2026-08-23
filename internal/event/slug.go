package event

import (
	"errors"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// reservedSlugs are the path segments a division slug may not take, because the
// public router puts divisions at /tournaments/:tournament/:division — the same
// depth as the tournament-level pages. A division slugged "schedule" would be
// shadowed by the schedule route and become unreachable.
//
// MUST be updated whenever a tournament-level route is added on the web side
// (tourney-web src/routes/(public)/tournaments/[slug]/). There is no compile-time
// link between the two, so this list is the only thing preventing a silently
// unreachable division.
var reservedSlugs = map[string]bool{
	"schedule": true, "players": true, "participants": true, "matches": true,
	"bracket": true, "groups": true, "about": true, "admin": true, "api": true,
}

// Slugify converts a division name into a URL-safe token: lowercased, accents
// folded to ASCII (Café → cafe), every other run of non-alphanumerics collapsed
// to a single dash. Returns "" when nothing survives, which callers resolve to
// a fallback — never leave a slug empty.
func Slugify(s string) string {
	// NFD splits "é" into "e" + combining acute; dropping Mn (nonspacing marks)
	// then leaves plain ASCII. Without this the accented rune is simply not
	// [a-z0-9] and would collapse to a dash, turning "Café" into "caf".
	folded, _, err := transform.String(
		transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC),
		s,
	)
	if err != nil {
		folded = s
	}

	var b strings.Builder
	lastDash := true // leading dashes are suppressed by starting "already dashed"
	for _, r := range strings.ToLower(folded) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// buildSlug picks the slug for one division given the slugs already taken in
// that tournament. Order of preference:
//
//	mixed-doubles                 the plain name — the common case
//	mens-singles-intermediate     name + category, when the plain one is taken
//	mens-singles-2                positional, when even that is taken
//
// Qualifying only on collision keeps the common URL short instead of making
// every division pay for the rare ambiguous one.
func buildSlug(name, category string, taken map[string]bool) string {
	base := Slugify(name)
	if base == "" {
		base = "division"
	}

	candidates := []string{base}
	if category != "" {
		if q := Slugify(name + " " + category); q != "" && q != base {
			candidates = append(candidates, q)
		}
	}
	for _, c := range candidates {
		if !taken[c] && !reservedSlugs[c] {
			return c
		}
	}
	// Reserved words land here too, so "bracket" becomes "bracket-2" rather
	// than an unreachable division.
	for n := 2; ; n++ {
		c := base + "-" + strconv.Itoa(n)
		if !taken[c] && !reservedSlugs[c] {
			return c
		}
	}
}

// isUniqueViolation reports whether err is Postgres 23505 (unique_violation).
// Used to distinguish "another request just took this slug, retry" from a real
// failure, so Create doesn't retry on, say, a connection error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

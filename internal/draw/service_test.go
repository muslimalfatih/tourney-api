package draw

import "testing"

// Reproduces the Renon Cup 2026 bug: a single_elim/group_knockout event with
// participants but no generated draw yet has zero matches for a round, so
// roundsMap[rn] is a map-miss. A bare `roundsMap[rn]` returns Go's zero value
// for []BracketMatch, which is nil — and json.Marshal renders that as
// `"matches": null`. Every frontend consumer (public bracket view, the
// bracket adapter, the organizer round-robin builder) assumed Matches is
// always an array and threw `Cannot read properties of null (reading
// 'length')` on first paint. roundMatches must never hand back nil.
func TestRoundMatches_MissingKeyIsEmptyNotNil(t *testing.T) {
	roundsMap := map[int][]BracketMatch{}

	got := roundMatches(roundsMap, 1)
	if got == nil {
		t.Fatal("roundMatches returned nil for a missing key — this is what serializes as JSON null instead of []")
	}
	if len(got) != 0 {
		t.Fatalf("roundMatches returned %d matches for a missing key, want 0", len(got))
	}
}

func TestRoundMatches_PresentKeyPassesThrough(t *testing.T) {
	want := []BracketMatch{{MatchNo: 1}, {MatchNo: 2}}
	roundsMap := map[int][]BracketMatch{1: want}

	got := roundMatches(roundsMap, 1)
	if len(got) != 2 {
		t.Fatalf("roundMatches(present key) = %d matches, want 2", len(got))
	}
}

// The realistic mid-tournament case the code-review agent flagged: round 1 is
// fully populated (real matches), round 2 hasn't been seeded from round-1
// winners yet. The overall response isn't "empty" (order is non-empty), so a
// single top-level nil-guard wouldn't catch this — only guarding the map
// lookup itself does.
func TestRoundMatches_SparseRoundsStayNonNil(t *testing.T) {
	roundsMap := map[int][]BracketMatch{
		1: {{MatchNo: 1}, {MatchNo: 2}},
		// round 2: no key at all — winners not seeded in yet
	}

	if got := roundMatches(roundsMap, 1); len(got) != 2 {
		t.Errorf("round 1: got %d matches, want 2", len(got))
	}
	if got := roundMatches(roundsMap, 2); got == nil || len(got) != 0 {
		t.Errorf("round 2 (unseeded): got %v, want non-nil empty slice", got)
	}
}

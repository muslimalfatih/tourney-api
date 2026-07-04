package draw

import (
	"testing"

	"github.com/google/uuid"
)

// mkEntries builds n entries; if seeded, entry i gets seed i+1, else seed 0.
func mkEntries(n int, seeded bool) []Entry {
	es := make([]Entry, n)
	for i := 0; i < n; i++ {
		seed := 0
		if seeded {
			seed = i + 1
		}
		es[i] = Entry{ParticipantID: uuid.New(), Seed: seed}
	}
	return es
}

func TestSingleElimination_MatchCount(t *testing.T) {
	// A single-elim bracket over a power-of-two field has n-1 matches; padded
	// fields still resolve to size-1 where size is the next power of two.
	cases := []struct {
		n        int
		wantSize int // next power of two
	}{
		{2, 2}, {3, 4}, {4, 4}, {5, 8}, {8, 8}, {9, 16}, {16, 16},
	}
	for _, c := range cases {
		got := GenerateSingleElimination(mkEntries(c.n, true))
		want := c.wantSize - 1
		if len(got) != want {
			t.Errorf("n=%d: got %d matches, want %d", c.n, len(got), want)
		}
	}
}

func TestSingleElimination_TooFew(t *testing.T) {
	if got := GenerateSingleElimination(mkEntries(1, false)); got != nil {
		t.Errorf("n=1 should return nil, got %d matches", len(got))
	}
	if got := GenerateSingleElimination(nil); got != nil {
		t.Errorf("nil entries should return nil")
	}
}

func TestSingleElimination_SeedsMeetLate(t *testing.T) {
	// With 4 seeds, seed 1 and seed 2 must be on opposite halves so they can
	// only meet in the final (they must not share a first-round match).
	entries := mkEntries(4, true)
	matches := GenerateSingleElimination(entries)

	seed1, seed2 := entries[0].ParticipantID, entries[1].ParticipantID
	for _, m := range matches {
		if m.Round != 1 {
			continue
		}
		has := func(id uuid.UUID) bool {
			return (m.Slot1.ParticipantID != nil && *m.Slot1.ParticipantID == id) ||
				(m.Slot2.ParticipantID != nil && *m.Slot2.ParticipantID == id)
		}
		if has(seed1) && has(seed2) {
			t.Error("seed 1 and seed 2 met in the first round; seeding is wrong")
		}
	}
}

func TestSingleElimination_ByesForOddField(t *testing.T) {
	// 3 entries in a size-4 bracket → exactly one bye in the first round.
	matches := GenerateSingleElimination(mkEntries(3, true))
	byes := 0
	for _, m := range matches {
		if m.Round == 1 && (m.Slot1.IsBye || m.Slot2.IsBye) {
			byes++
		}
	}
	if byes != 1 {
		t.Errorf("3-entry bracket: got %d byes, want 1", byes)
	}
}

func TestSingleElimination_ProgressionWired(t *testing.T) {
	// Every non-final match must feed a next match; exactly one match (the
	// final) has NextMatchIdx == -1.
	matches := GenerateSingleElimination(mkEntries(8, true))
	finals := 0
	for i, m := range matches {
		if m.NextMatchIdx == -1 {
			finals++
			continue
		}
		if m.NextMatchIdx < 0 || m.NextMatchIdx >= len(matches) {
			t.Errorf("match %d: NextMatchIdx %d out of range", i, m.NextMatchIdx)
		}
		if m.NextSlot != 1 && m.NextSlot != 2 {
			t.Errorf("match %d: NextSlot %d invalid", i, m.NextSlot)
		}
	}
	if finals != 1 {
		t.Errorf("expected exactly 1 final, got %d", finals)
	}
}

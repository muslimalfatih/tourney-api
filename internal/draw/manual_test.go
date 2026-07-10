package draw

import (
	"testing"

	"github.com/google/uuid"
)

// mkPairs builds n round-1 pairs, each with two fresh participant ids.
func mkPairs(n int) []Pair {
	ps := make([]Pair, n)
	for i := 0; i < n; i++ {
		a, b := uuid.New(), uuid.New()
		ps[i] = Pair{A: &a, B: &b}
	}
	return ps
}

func TestFromPairs_MatchCount(t *testing.T) {
	// firstRoundCount pads to the next power of two; total matches = size-1
	// where size = firstRoundCount*2.
	cases := []struct {
		pairs int
		want  int // total matches
	}{
		{1, 1}, {2, 3}, {3, 7}, {4, 7}, {8, 15},
	}
	for _, c := range cases {
		got := GenerateFromPairs(mkPairs(c.pairs))
		if len(got) != c.want {
			t.Errorf("pairs=%d: got %d matches, want %d", c.pairs, len(got), c.want)
		}
	}
}

func TestFromPairs_RespectsOrder(t *testing.T) {
	// Round-1 slots must be exactly the pairs given, in order — no seeding
	// reshuffle.
	pairs := mkPairs(4)
	matches := GenerateFromPairs(pairs)
	r1 := 0
	for _, m := range matches {
		if m.Round != 1 {
			continue
		}
		p := pairs[r1]
		if m.Slot1.ParticipantID == nil || *m.Slot1.ParticipantID != *p.A {
			t.Errorf("round-1 match %d slot1 mismatch", r1)
		}
		if m.Slot2.ParticipantID == nil || *m.Slot2.ParticipantID != *p.B {
			t.Errorf("round-1 match %d slot2 mismatch", r1)
		}
		r1++
	}
	if r1 != 4 {
		t.Errorf("expected 4 round-1 matches, got %d", r1)
	}
}

func TestFromPairs_ByeSlot(t *testing.T) {
	// A pair with a nil side becomes a bye in that slot.
	a := uuid.New()
	matches := GenerateFromPairs([]Pair{{A: &a, B: nil}, mkPairs(1)[0]})
	if matches[0].Slot2.IsBye != true {
		t.Error("nil side should produce a bye slot")
	}
	if matches[0].Slot1.ParticipantID == nil || *matches[0].Slot1.ParticipantID != a {
		t.Error("filled side should keep its participant")
	}
}

func TestFromPairs_DropsEmptyAndTooFew(t *testing.T) {
	if got := GenerateFromPairs([]Pair{{A: nil, B: nil}}); got != nil {
		t.Errorf("all-empty pairs should return nil, got %d", len(got))
	}
	if got := GenerateFromPairs(nil); got != nil {
		t.Error("nil pairs should return nil")
	}
}

func TestFromPairs_ProgressionWired(t *testing.T) {
	// Exactly one final; every other match feeds a valid next slot.
	matches := GenerateFromPairs(mkPairs(4))
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
		t.Errorf("expected 1 final, got %d", finals)
	}
}

func TestValidatePairs_Rules(t *testing.T) {
	a, b, cc, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	valid := map[uuid.UUID]bool{a: true, b: true, cc: true, d: true}
	unknown := uuid.New()

	// Good: two full, distinct pairs.
	if _, err := validatePairs([]Pair{{A: &a, B: &b}, {A: &cc, B: &d}}, valid); err != nil {
		t.Errorf("valid pairs rejected: %v", err)
	}
	// Half-filled match.
	if _, err := validatePairs([]Pair{{A: &a, B: nil}}, valid); err != ErrInvalidPairs {
		t.Errorf("half-filled should be ErrInvalidPairs, got %v", err)
	}
	// Duplicate team across pairs.
	if _, err := validatePairs([]Pair{{A: &a, B: &b}, {A: &a, B: &cc}}, valid); err != ErrInvalidPairs {
		t.Errorf("duplicate team should be ErrInvalidPairs, got %v", err)
	}
	// Unknown team.
	if _, err := validatePairs([]Pair{{A: &a, B: &unknown}}, valid); err != ErrInvalidPairs {
		t.Errorf("unknown team should be ErrInvalidPairs, got %v", err)
	}
	// No real matchups.
	if _, err := validatePairs([]Pair{{A: nil, B: nil}}, valid); err != ErrNotEnough {
		t.Errorf("no matchups should be ErrNotEnough, got %v", err)
	}
}

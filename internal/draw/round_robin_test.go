package draw

import (
	"testing"

	"github.com/google/uuid"
)

func TestRoundRobin_MatchCount(t *testing.T) {
	// A single round robin over n entries has C(n,2) = n(n-1)/2 matches.
	for _, n := range []int{2, 3, 4, 5, 6, 8} {
		got := GenerateRoundRobin(mkEntries(n, false))
		want := n * (n - 1) / 2
		if len(got) != want {
			t.Errorf("n=%d: got %d matches, want %d", n, len(got), want)
		}
	}
}

func TestRoundRobin_EveryPairOnce(t *testing.T) {
	entries := mkEntries(6, false)
	matches := GenerateRoundRobin(entries)

	// Count how often each unordered pair meets; each must be exactly 1.
	pairKey := func(a, b uuid.UUID) [2]uuid.UUID {
		if a.String() < b.String() {
			return [2]uuid.UUID{a, b}
		}
		return [2]uuid.UUID{b, a}
	}
	seen := map[[2]uuid.UUID]int{}
	for _, m := range matches {
		if m.Slot1.ParticipantID == nil || m.Slot2.ParticipantID == nil {
			t.Fatal("round robin match has an empty slot")
		}
		seen[pairKey(*m.Slot1.ParticipantID, *m.Slot2.ParticipantID)]++
	}
	expectedPairs := len(entries) * (len(entries) - 1) / 2
	if len(seen) != expectedPairs {
		t.Errorf("got %d distinct pairs, want %d", len(seen), expectedPairs)
	}
	for pair, count := range seen {
		if count != 1 {
			t.Errorf("pair %v met %d times, want 1", pair, count)
		}
	}
}

func TestRoundRobin_EachPlaysNMinus1(t *testing.T) {
	entries := mkEntries(5, false)
	matches := GenerateRoundRobin(entries)
	appearances := map[uuid.UUID]int{}
	for _, m := range matches {
		appearances[*m.Slot1.ParticipantID]++
		appearances[*m.Slot2.ParticipantID]++
	}
	for id, c := range appearances {
		if c != len(entries)-1 {
			t.Errorf("entry %v played %d matches, want %d", id, c, len(entries)-1)
		}
	}
}

func TestRoundRobin_NoProgression(t *testing.T) {
	// Round robin has no advancement graph.
	for _, m := range GenerateRoundRobin(mkEntries(4, false)) {
		if m.NextMatchIdx != -1 {
			t.Errorf("round robin match should not feed forward, got NextMatchIdx %d", m.NextMatchIdx)
		}
	}
}

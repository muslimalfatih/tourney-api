package draw

import (
	"strings"
	"testing"
)

func TestGroupKnockout_MatchCount(t *testing.T) {
	// 8 entries, 2 groups of 4, top 2 advance:
	//   group stage: 2 * C(4,2) = 12
	//   knockout:    4 advancers → 2 semis + 1 final = 3
	//   total: 15
	entries := mkEntries(8, true)
	matches := GenerateGroupKnockout(entries, GroupKnockoutConfig{Groups: 2, Advance: 2})
	if len(matches) != 15 {
		t.Fatalf("got %d matches, want 15", len(matches))
	}

	group, knockout := 0, 0
	for _, m := range matches {
		if m.GroupIndex >= 0 {
			group++
		} else {
			knockout++
		}
	}
	if group != 12 {
		t.Errorf("group matches: got %d, want 12", group)
	}
	if knockout != 3 {
		t.Errorf("knockout matches: got %d, want 3", knockout)
	}
}

func TestGroupKnockout_SnakeSeeding(t *testing.T) {
	// With 2 groups and seeds 1..8, snake distribution puts seeds 1 and 2 in
	// different groups (top seeds must be separated).
	entries := mkEntries(8, true)
	matches := GenerateGroupKnockout(entries, GroupKnockoutConfig{Groups: 2, Advance: 2})

	seed1, seed2 := entries[0].ParticipantID, entries[1].ParticipantID
	groupOf := map[string]int{} // participant → group index
	for _, m := range matches {
		if m.GroupIndex < 0 {
			continue
		}
		if m.Slot1.ParticipantID != nil {
			groupOf[m.Slot1.ParticipantID.String()] = m.GroupIndex
		}
		if m.Slot2.ParticipantID != nil {
			groupOf[m.Slot2.ParticipantID.String()] = m.GroupIndex
		}
	}
	if groupOf[seed1.String()] == groupOf[seed2.String()] {
		t.Error("seed 1 and seed 2 landed in the same group; snake seeding is wrong")
	}
}

func TestGroupKnockout_KnockoutPlaceholders(t *testing.T) {
	// The knockout's first round must carry Winner/Runner-up group labels, not
	// concrete participants (resolved later).
	matches := GenerateGroupKnockout(mkEntries(8, true), GroupKnockoutConfig{Groups: 2, Advance: 2})
	labels := 0
	for _, m := range matches {
		if m.GroupIndex >= 0 {
			continue
		}
		for _, s := range []Slot{m.Slot1, m.Slot2} {
			if s.SourceLabel != "" {
				if !strings.Contains(s.SourceLabel, "Group") {
					t.Errorf("unexpected knockout label %q", s.SourceLabel)
				}
				labels++
			}
		}
	}
	// 4 advancers fill the two semifinals → 4 labelled slots.
	if labels != 4 {
		t.Errorf("got %d placeholder labels, want 4", labels)
	}
}

func TestGroupKnockout_Rejects(t *testing.T) {
	// Too few entries or invalid config must return nil.
	if GenerateGroupKnockout(mkEntries(3, false), GroupKnockoutConfig{Groups: 2, Advance: 2}) != nil {
		t.Error("3 entries should be rejected")
	}
	if GenerateGroupKnockout(mkEntries(8, false), GroupKnockoutConfig{Groups: 1, Advance: 2}) != nil {
		t.Error("1 group should be rejected")
	}
}

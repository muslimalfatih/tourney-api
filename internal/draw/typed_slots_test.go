package draw

import "testing"

// The typed advancer slots are what resolution reads — ranks and group
// indices must be exact, and labels must use the group's REAL name (the
// legacy letter-based labels silently never resolved custom names).
func TestAdvancerSlots_RanksGroupsAndNames(t *testing.T) {
	specs := []GroupSpec{
		{Name: "Pool North", AdvanceCount: 3},
		{Name: "Pool South", AdvanceCount: 2},
	}
	slots := advancerSlots(specs)

	want := []struct {
		label string
		group int
		rank  int
	}{
		{"Winner Pool North", 0, 1},
		{"Runner-up Pool South", 1, 2}, // cross-paired with the other group
		{"#3 Pool North", 0, 3},        // rank 3 — never resolvable pre-typing
		{"Winner Pool South", 1, 1},
		{"Runner-up Pool North", 0, 2},
	}
	if len(slots) != len(want) {
		t.Fatalf("got %d slots, want %d", len(slots), len(want))
	}
	for i, w := range want {
		got := slots[i]
		if got.SourceLabel != w.label || got.SourceGroupIdx != w.group || got.SourceRank != w.rank {
			t.Errorf("slot %d = {%q g%d r%d}, want {%q g%d r%d}",
				i, got.SourceLabel, got.SourceGroupIdx, got.SourceRank, w.label, w.group, w.rank)
		}
	}
}

// A single group advancing three keeps rank identity through the knockout
// builder, and the pad slot is a bye — never an empty.
func TestBuildKnockoutFromSlots_PadsWithByes(t *testing.T) {
	ms := buildKnockoutFromSlots(advancerSlots([]GroupSpec{{Name: "Group A", AdvanceCount: 3}}))
	if len(ms) != 3 { // 2 first-round + final
		t.Fatalf("got %d matches, want 3", len(ms))
	}
	r1 := []Slot{ms[0].Slot1, ms[0].Slot2, ms[1].Slot1, ms[1].Slot2}
	byes, placements := 0, 0
	for _, s := range r1 {
		if s.IsBye {
			byes++
		}
		if s.SourceLabel != "" {
			placements++
			if s.SourceRank < 1 || s.SourceRank > 3 {
				t.Errorf("placement rank %d out of range", s.SourceRank)
			}
		}
	}
	if byes != 1 || placements != 3 {
		t.Errorf("round 1 = %d byes + %d placements, want 1 + 3", byes, placements)
	}
	// Final is winner-fed on both sides: no labels, no byes.
	f := ms[2]
	if f.Slot1.SourceLabel != "" || f.Slot2.SourceLabel != "" || f.Slot1.IsBye || f.Slot2.IsBye {
		t.Errorf("final slots must be pure winner feeds, got %+v / %+v", f.Slot1, f.Slot2)
	}
}

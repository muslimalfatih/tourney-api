package draw

import "fmt"

// GroupKnockoutConfig controls the group-stage → knockout split.
type GroupKnockoutConfig struct {
	Groups  int // number of groups
	Advance int // how many top finishers per group advance to the knockout
}

// GenerateGroupKnockout builds a two-stage draw: a round-robin within each
// group, followed by a single-elimination knockout that the group finishers
// feed into. Because group results aren't known at generation time, the
// knockout slots carry SourceLabels ("Winner Group A", "Runner-up Group B")
// and are resolved later as groups complete.
//
// Entries are distributed across groups by snake seeding so seeded entries are
// spread evenly. The knockout is seeded so group winners meet runners-up from
// other groups, and two entries from the same group can't meet before the
// bracket forces it.
//
// It returns the flat match slice. Group matches have GroupIndex >= 0; knockout
// matches have GroupIndex -1.
func GenerateGroupKnockout(entries []Entry, cfg GroupKnockoutConfig) []Match {
	n := len(entries)
	if n < 4 || cfg.Groups < 2 || cfg.Advance < 1 {
		return nil
	}
	// Cap advancers to what actually fits and forms a power-of-two knockout.
	totalAdvance := cfg.Groups * cfg.Advance
	if totalAdvance < 2 || totalAdvance > n {
		return nil
	}

	// 1. Distribute entries into groups by snake seeding.
	groups := make([][]Entry, cfg.Groups)
	order := make([]Entry, len(entries))
	copy(order, entries)
	sortBySeed(order)
	for i, e := range order {
		round := i / cfg.Groups
		pos := i % cfg.Groups
		if round%2 == 1 { // reverse every other pass (snake)
			pos = cfg.Groups - 1 - pos
		}
		groups[pos] = append(groups[pos], e)
	}

	var matches []Match

	// 2. Round-robin within each group.
	for gi, g := range groups {
		rr := GenerateRoundRobin(g)
		for _, m := range rr {
			m.GroupIndex = gi
			m.NextMatchIdx = -1 // group matches don't feed directly
			matches = append(matches, m)
		}
	}

	// 3. Knockout bracket over the advancers, using placeholder feed labels.
	//    Build a seeded slot order: standard cross-group pairing where the
	//    first knockout round pairs group winners against other groups'
	//    runners-up.
	advancers := knockoutSeedLabels(cfg.Groups, cfg.Advance)
	koStart := len(matches)
	koMatches := buildKnockoutFromLabels(advancers)
	// Offset the internal NextMatchIdx of the knockout matches by koStart so
	// they reference absolute positions in the combined slice.
	for i := range koMatches {
		if koMatches[i].NextMatchIdx >= 0 {
			koMatches[i].NextMatchIdx += koStart
		}
		koMatches[i].GroupIndex = -1
	}
	matches = append(matches, koMatches...)

	return matches
}

// knockoutSeedLabels produces the ordered advancer labels for the knockout's
// first round. For a standard 2-groups-of-2 this yields:
//
//	[Winner A, Runner-up B, Winner B, Runner-up A]
//
// so A1 plays B2 and B1 plays A2.
func knockoutSeedLabels(numGroups, advance int) []string {
	letters := func(i int) string { return string(rune('A' + i)) }
	posName := func(p int) string {
		switch p {
		case 0:
			return "Winner"
		case 1:
			return "Runner-up"
		default:
			return fmt.Sprintf("#%d", p+1)
		}
	}
	// Flatten (group, position) into seeded order: all winners first (spread),
	// then runners-up, cross-paired. Simple robust scheme: interleave winners
	// with the opposite group's runners-up.
	var labels []string
	for g := 0; g < numGroups; g++ {
		labels = append(labels, fmt.Sprintf("%s Group %s", posName(0), letters(g)))
		if advance > 1 {
			other := (g + 1) % numGroups
			labels = append(labels, fmt.Sprintf("%s Group %s", posName(1), letters(other)))
		}
	}
	return labels
}

// buildKnockoutFromLabels is the legacy label-only entry point, kept for the
// (currently unreachable) auto generator and its tests. The live path uses
// buildKnockoutFromSlots with typed slots.
func buildKnockoutFromLabels(labels []string) []Match {
	slots := make([]Slot, len(labels))
	for i, l := range labels {
		slots[i] = labelSlot(l)
	}
	return buildKnockoutFromSlots(slots)
}

// buildKnockoutFromSlots builds a single-elim bracket whose first-round slots
// are the given feeds (typed group placements, fixed entries, or byes — the
// pad to a power of two is byes).
func buildKnockoutFromSlots(entries []Slot) []Match {
	size := nextPowerOfTwo(len(entries))
	for len(entries) < size {
		entries = append(entries, Slot{IsBye: true})
	}
	rounds := log2(size)

	var matches []Match
	firstRoundCount := size / 2
	for i := 0; i < firstRoundCount; i++ {
		matches = append(matches, Match{
			Round:        1,
			MatchInRound: i,
			Slot1:        entries[i*2],
			Slot2:        entries[i*2+1],
		})
	}
	prevStart, prevCount := 0, firstRoundCount
	for r := 2; r <= rounds; r++ {
		roundCount := prevCount / 2
		roundStart := len(matches)
		for i := 0; i < roundCount; i++ {
			matches = append(matches, Match{Round: r, MatchInRound: i})
		}
		for i := 0; i < prevCount; i++ {
			m := &matches[prevStart+i]
			m.NextMatchIdx = roundStart + i/2
			m.NextSlot = (i % 2) + 1
		}
		prevStart, prevCount = roundStart, roundCount
	}
	if len(matches) > 0 {
		matches[len(matches)-1].NextMatchIdx = -1
	}
	return matches
}

func labelSlot(label string) Slot {
	if label == "" {
		return Slot{IsBye: true}
	}
	return Slot{SourceLabel: label}
}

// sortBySeed orders entries by seed ascending (seed 0 = unseeded, sorted last),
// stable. Simple insertion sort keeps the file dependency-free.
func sortBySeed(es []Entry) {
	key := func(e Entry) int {
		if e.Seed == 0 {
			return 1 << 30
		}
		return e.Seed
	}
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && key(es[j]) < key(es[j-1]); j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
}

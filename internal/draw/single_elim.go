// Package draw contains the pure tournament-format generators. Each generator
// takes participants (and optional seeds) and returns a match structure the
// match module persists. These functions are deliberately free of HTTP and DB
// concerns so they are trivially unit-testable and so a bracket-logic library
// (e.g. brackets-manager) could later back the same signatures via an adapter
// without leaking into the rest of the codebase.
package draw

import "github.com/google/uuid"

// Entry is one competitor in a draw (a participant id + its seed; seed 0 means
// unseeded).
type Entry struct {
	ParticipantID uuid.UUID
	Seed          int
}

// Slot is a position in a match. Exactly one of ParticipantID / IsBye is set
// once the draw is placed; both nil means the slot is filled by the winner of a
// prior match (a "feed"). SourceLabel describes an unresolved feed for display
// (e.g. "Winner Group A") until the group stage completes.
type Slot struct {
	ParticipantID *uuid.UUID
	IsBye         bool
	SourceLabel   string
}

// Match is a generated match in the bracket. NextMatchIndex/NextSlot describe
// where the winner advances, forming the progression graph the frontend renders
// and the backend persists as matches.next_match_id.
//
// GroupIndex is -1 for knockout matches, or the 0-based group number for group
// stage matches (group-knockout format).
type Match struct {
	Round        int
	MatchInRound int
	Slot1        Slot
	Slot2        Slot
	NextMatchIdx int // index into the flat match slice; -1 for the final
	NextSlot     int // 1 or 2
	GroupIndex   int // -1 = knockout; >=0 = group stage
}

// GenerateSingleElimination builds a seeded single-elimination bracket. The
// bracket is padded up to the next power of two with byes, and seeds are placed
// using the standard bracket-seeding order so top seeds meet latest.
func GenerateSingleElimination(entries []Entry) []Match {
	n := len(entries)
	if n < 2 {
		return nil
	}

	size := nextPowerOfTwo(n)
	ordered := seedPositions(size)           // bracket slot → seed rank (1-based)
	placed := placeEntries(entries, ordered) // bracket slot → *Entry (nil = bye)

	rounds := log2(size)
	var matches []Match

	// Round 1 from the placed slots.
	firstRoundCount := size / 2
	for i := 0; i < firstRoundCount; i++ {
		matches = append(matches, Match{
			Round:        1,
			MatchInRound: i,
			Slot1:        entryToSlot(placed[i*2]),
			Slot2:        entryToSlot(placed[i*2+1]),
		})
	}

	// Subsequent rounds are empty (winner-fed) matches.
	prevRoundStart := 0
	prevRoundCount := firstRoundCount
	for r := 2; r <= rounds; r++ {
		roundCount := prevRoundCount / 2
		roundStart := len(matches)
		for i := 0; i < roundCount; i++ {
			matches = append(matches, Match{Round: r, MatchInRound: i})
		}
		// Wire the previous round's winners into this round.
		for i := 0; i < prevRoundCount; i++ {
			m := &matches[prevRoundStart+i]
			m.NextMatchIdx = roundStart + i/2
			m.NextSlot = (i % 2) + 1
		}
		prevRoundStart = roundStart
		prevRoundCount = roundCount
	}
	// The final has no next match.
	if len(matches) > 0 {
		last := &matches[len(matches)-1]
		last.NextMatchIdx = -1
		last.NextSlot = 0
	}

	// Single elimination has no group stage.
	for i := range matches {
		matches[i].GroupIndex = -1
	}

	return matches
}

func entryToSlot(e *Entry) Slot {
	if e == nil {
		return Slot{IsBye: true}
	}
	id := e.ParticipantID
	return Slot{ParticipantID: &id}
}

// placeEntries maps bracket slots to entries using the seed order. Seeded
// entries go to their designated slots; unseeded fill the remainder in order;
// empty slots become byes.
func placeEntries(entries []Entry, order []int) []*Entry {
	size := len(order)
	bySeed := make(map[int]*Entry)
	var unseeded []*Entry
	for i := range entries {
		e := &entries[i]
		if e.Seed > 0 {
			bySeed[e.Seed] = e
		} else {
			unseeded = append(unseeded, e)
		}
	}

	placed := make([]*Entry, size)
	ui := 0
	for slot, seedRank := range order {
		if e, ok := bySeed[seedRank]; ok {
			placed[slot] = e
			continue
		}
		if ui < len(unseeded) {
			placed[slot] = unseeded[ui]
			ui++
		}
		// else: leave nil → bye
	}
	return placed
}

// seedPositions returns, for a bracket of the given power-of-two size, the seed
// rank assigned to each slot index, using the standard recursive seeding so
// seed 1 and seed 2 land on opposite halves, etc.
func seedPositions(size int) []int {
	seeds := []int{1, 2}
	for len(seeds) < size {
		next := make([]int, 0, len(seeds)*2)
		rounds := len(seeds) * 2
		for _, s := range seeds {
			next = append(next, s, rounds+1-s)
		}
		seeds = next
	}
	return seeds
}

func nextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func log2(n int) int {
	r := 0
	for n > 1 {
		n >>= 1
		r++
	}
	return r
}

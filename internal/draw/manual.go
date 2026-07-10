package draw

import "github.com/google/uuid"

// Pair is one organizer-supplied round-1 matchup. A nil side is an empty slot
// that becomes a bye; a pair with both sides nil is dropped before generation.
// JSON tags let the Match-builder payload bind straight into it.
type Pair struct {
	A *uuid.UUID `json:"team_a_id"`
	B *uuid.UUID `json:"team_b_id"`
}

// GenerateFromPairs builds a single-elimination bracket from explicit round-1
// pairings instead of seeding. Round 1 is taken verbatim from pairs (in the
// order given); the pair count is padded up to the next power of two with
// all-bye matches so the winner-fed rounds wire cleanly. Later rounds are built
// and wired identically to GenerateSingleElimination, so the two paths persist
// and render the same way — the only difference is how round 1 is populated.
//
// Seeding intentionally plays no part here: the organizer's order IS the draw.
// A future "seed then auto-pair" mode can layer on top by ordering pairs via
// seedPositions before calling this, without changing this function.
func GenerateFromPairs(pairs []Pair) []Match {
	// Drop fully-empty pairs; they carry no information.
	cleaned := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		if p.A == nil && p.B == nil {
			continue
		}
		cleaned = append(cleaned, p)
	}
	if len(cleaned) == 0 {
		return nil
	}

	// Round-1 match count is padded to a power of two so the bracket is balanced.
	firstRoundCount := nextPowerOfTwo(len(cleaned))
	var matches []Match

	// Round 1 straight from the supplied pairs; padding slots become byes.
	for i := 0; i < firstRoundCount; i++ {
		var p Pair
		if i < len(cleaned) {
			p = cleaned[i]
		}
		matches = append(matches, Match{
			Round:        1,
			MatchInRound: i,
			Slot1:        pairSlot(p.A),
			Slot2:        pairSlot(p.B),
		})
	}

	// Rounds above 1 are identical to the seeded builder.
	return wireFedRounds(matches, firstRoundCount)
}

// pairSlot turns an optional participant id into a filled or bye slot.
func pairSlot(id *uuid.UUID) Slot {
	if id == nil {
		return Slot{IsBye: true}
	}
	v := *id
	return Slot{ParticipantID: &v}
}

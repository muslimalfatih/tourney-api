package draw

// GenerateRoundRobin builds a single round-robin schedule where every entry
// plays every other entry exactly once, using the circle method (polygon
// rotation). With an odd number of entries a bye is rotated through, so one
// entry sits out each round.
//
// Unlike single elimination there is no progression graph: every match is a
// concrete pairing with both participants known up front, and NextMatchIdx is
// -1 for all of them. A winner is decided by standings, not advancement.
func GenerateRoundRobin(entries []Entry) []Match {
	n := len(entries)
	if n < 2 {
		return nil
	}

	// Work on a slot list padded with a bye (index -1) when odd.
	idx := make([]int, n)
	for i := range entries {
		idx[i] = i
	}
	bye := -1
	if n%2 == 1 {
		idx = append(idx, bye)
	}
	size := len(idx)   // even
	rounds := size - 1 // each entry plays every other once
	half := size / 2

	var matches []Match
	matchInRound := make([]int, rounds)

	for r := 0; r < rounds; r++ {
		for i := 0; i < half; i++ {
			a := idx[i]
			b := idx[size-1-i]
			if a == bye || b == bye {
				continue // the entry paired with the bye sits out
			}
			matches = append(matches, Match{
				Round:        r + 1,
				MatchInRound: matchInRound[r],
				Slot1:        slotFor(entries, a),
				Slot2:        slotFor(entries, b),
				NextMatchIdx: -1, // round robin: no advancement
				NextSlot:     0,
			})
			matchInRound[r]++
		}
		// Rotate: keep idx[0] fixed, rotate the rest clockwise by one.
		last := idx[size-1]
		copy(idx[2:], idx[1:size-1])
		idx[1] = last
	}

	return matches
}

func slotFor(entries []Entry, i int) Slot {
	id := entries[i].ParticipantID
	return Slot{ParticipantID: &id}
}

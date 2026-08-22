// Scoring validation (Refactor Phase 3.4): the authoritative tennis/padel
// rules for what a submitted score may look like. Pure functions — no DB, no
// HTTP — so every rule is unit-testable. The service runs ValidateScore BEFORE
// the SaveScore transaction; an invalid submission therefore writes nothing
// and broadcasts nothing.
package match

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ScoringConfig is a division's validated scoring configuration
// (events.scoring jsonb; '{}' = all defaults).
type ScoringConfig struct {
	BestOf      int    `json:"best_of"`      // 1 | 3
	DecidingSet string `json:"deciding_set"` // "full" | "match_tiebreak"
	GoldenPoint bool   `json:"golden_point"` // padel; display-only
}

func DefaultScoringConfig() ScoringConfig {
	return ScoringConfig{BestOf: 3, DecidingSet: "full", GoldenPoint: false}
}

// Violation is one structured validation failure, rendered into the 422
// response's details.
type Violation struct {
	Set     int    `json:"set,omitempty"` // 0 = not set-specific
	Field   string `json:"field,omitempty"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// ParseScoringConfig validates and normalizes a raw events.scoring document.
// Absent keys take defaults; unknown keys and invalid values are violations.
func ParseScoringConfig(raw []byte) (ScoringConfig, []Violation) {
	cfg := DefaultScoringConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return cfg, []Violation{{Rule: "config.invalid_json", Message: "scoring config is not a JSON object"}}
	}
	var v []Violation
	for key, val := range doc {
		switch key {
		case "best_of":
			var n int
			if err := json.Unmarshal(val, &n); err != nil || (n != 1 && n != 3) {
				v = append(v, Violation{Field: "best_of", Rule: "config.best_of", Message: "best_of must be 1 or 3"})
				continue
			}
			cfg.BestOf = n
		case "deciding_set":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || (s != "full" && s != "match_tiebreak") {
				v = append(v, Violation{Field: "deciding_set", Rule: "config.deciding_set", Message: `deciding_set must be "full" or "match_tiebreak"`})
				continue
			}
			cfg.DecidingSet = s
		case "golden_point":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				v = append(v, Violation{Field: "golden_point", Rule: "config.golden_point", Message: "golden_point must be a boolean"})
				continue
			}
			cfg.GoldenPoint = b
		default:
			v = append(v, Violation{Field: key, Rule: "config.unknown_key", Message: fmt.Sprintf("unknown scoring config key %q", key)})
		}
	}
	// A 1-set match has no deciding-set concept beyond its only set; forbid a
	// contradictory combination rather than silently ignoring it.
	if cfg.BestOf == 1 && cfg.DecidingSet == "match_tiebreak" {
		v = append(v, Violation{Field: "deciding_set", Rule: "config.match_tiebreak_best_of_1", Message: "match_tiebreak requires best_of 3"})
	}
	if len(v) > 0 {
		return DefaultScoringConfig(), v
	}
	return cfg, nil
}

// Completion states (wire values). Mapping to match_status:
//
//	incomplete → live        no winner; partial legal sets; nothing advances
//	normal     → completed   winner derived from complete, decided sets
//	walkover   → walkover    explicit winner, NO sets
//	retired    → retired     explicit winner, partial legal sets allowed
//	cancelled  → cancelled   no winner, partial legal sets allowed; counts
//	                          nowhere, but ends the match for progression
const (
	CompletionIncomplete = "incomplete"
	CompletionNormal     = "normal"
	CompletionWalkover   = "walkover"
	CompletionRetired    = "retired"
	CompletionCancelled  = "cancelled"
)

func statusForCompletion(c string) string {
	switch c {
	case CompletionNormal:
		return "completed"
	case CompletionWalkover:
		return "walkover"
	case CompletionRetired:
		return "retired"
	case CompletionCancelled:
		return "cancelled"
	default:
		return "live"
	}
}

// decidedStatus reports whether a status represents a finished match — one
// that ends play for progression purposes. Counting toward standings is
// narrower: cancelled is decided but never counted.
func decidedStatus(s string) bool {
	switch s {
	case "completed", "walkover", "retired", "cancelled", "bye":
		return true
	}
	return false
}

// ValidateScore checks a submission against the division config and, when
// valid, returns the winner slot (1/2; 0 = none) and the target match status.
//
// Set legality (regular sets): 6-x with x≤4, 7-5, or 7-6 with tiebreak
// metadata (winner ≥7, leads by exactly two beyond 7-7 territory). When the
// config uses a 10-point match tiebreak as the deciding set, that set carries
// the tiebreak POINTS in the games fields (≥10, lead by two, exact two beyond
// 10) and must have no set-tiebreak metadata.
func ValidateScore(cfg ScoringConfig, sets []SetScore, completion string, winnerSlot int) (int, string, []Violation) {
	var v []Violation

	switch completion {
	case CompletionIncomplete, CompletionNormal, CompletionWalkover, CompletionRetired, CompletionCancelled:
	default:
		return 0, "", []Violation{{Field: "completion", Rule: "completion.invalid",
			Message: "completion must be incomplete, normal, walkover, retired, or cancelled"}}
	}

	// Winner requirements per completion state.
	switch completion {
	case CompletionWalkover, CompletionRetired:
		if winnerSlot != 1 && winnerSlot != 2 {
			v = append(v, Violation{Field: "winner_slot", Rule: "winner.required",
				Message: completion + " requires winner_slot 1 or 2"})
		}
	default:
		if winnerSlot != 0 {
			v = append(v, Violation{Field: "winner_slot", Rule: "winner.forbidden",
				Message: "winner_slot is only accepted for walkover and retired"})
		}
	}

	// Walkover records no play.
	if completion == CompletionWalkover {
		if len(sets) > 0 {
			v = append(v, Violation{Rule: "sets.walkover", Message: "a walkover records no sets"})
		}
		if len(v) > 0 {
			return 0, "", v
		}
		return winnerSlot, statusForCompletion(completion), nil
	}

	if len(sets) == 0 && completion != CompletionCancelled {
		return 0, "", append(v, Violation{Rule: "sets.required", Message: "at least one set is required"})
	}

	// Numbering: unique, consecutive from 1, within best_of.
	ordered := make([]SetScore, len(sets))
	copy(ordered, sets)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SetNumber < ordered[j].SetNumber })
	for i, st := range ordered {
		if st.SetNumber != i+1 {
			return 0, "", append(v, Violation{Set: st.SetNumber, Field: "set_number", Rule: "sets.numbering",
				Message: "sets must be numbered consecutively from 1 with no duplicates"})
		}
	}
	if len(ordered) > cfg.BestOf {
		return 0, "", append(v, Violation{Rule: "sets.too_many",
			Message: fmt.Sprintf("best of %d allows at most %d sets", cfg.BestOf, cfg.BestOf)})
	}

	setsToWin := cfg.BestOf/2 + 1
	won1, won2 := 0, 0
	decidedAfter := 0 // set number at which the match became decided

	for idx, st := range ordered {
		setNo := idx + 1
		isDecider := cfg.DecidingSet == "match_tiebreak" && cfg.BestOf > 1 && setNo == cfg.BestOf
		last := idx == len(ordered)-1
		// Completed states require every set legal-complete except that
		// retired/cancelled/incomplete may leave the LAST set partial.
		allowPartial := last && completion != CompletionNormal

		winner, complete, sv := validateSet(st, isDecider, allowPartial)
		for i := range sv {
			sv[i].Set = setNo
		}
		v = append(v, sv...)

		if decidedAfter > 0 && (complete || st.P1Games > 0 || st.P2Games > 0) {
			v = append(v, Violation{Set: setNo, Rule: "sets.after_decided",
				Message: fmt.Sprintf("the match was already decided after set %d", decidedAfter)})
			continue
		}
		if complete {
			if winner == 1 {
				won1++
			} else {
				won2++
			}
			if decidedAfter == 0 && (won1 == setsToWin || won2 == setsToWin) {
				decidedAfter = setNo
			}
		}
	}
	if len(v) > 0 {
		return 0, "", v
	}

	switch completion {
	case CompletionNormal:
		if decidedAfter == 0 {
			return 0, "", []Violation{{Rule: "sets.unfinished",
				Message: fmt.Sprintf("a normal completion needs a side to win %d set(s)", setsToWin)}}
		}
		derived := 1
		if won2 > won1 {
			derived = 2
		}
		return derived, statusForCompletion(completion), nil
	default:
		// incomplete / retired / cancelled: entering more play after the
		// match is decided is meaningless.
		if decidedAfter > 0 && decidedAfter < len(ordered) {
			return 0, "", []Violation{{Rule: "sets.after_decided",
				Message: fmt.Sprintf("the match was already decided after set %d", decidedAfter)}}
		}
		if completion == CompletionIncomplete && decidedAfter > 0 {
			return 0, "", []Violation{{Rule: "completion.decided",
				Message: "these sets decide the match — submit completion \"normal\""}}
		}
		return winnerSlot, statusForCompletion(completion), nil
	}
}

// validateSet checks a single set. Returns (winner side 1|2|0, complete?,
// violations). allowPartial permits an in-progress set (only ever the last).
func validateSet(st SetScore, isDecider, allowPartial bool) (int, bool, []Violation) {
	a, b := st.P1Games, st.P2Games
	ta, tb := st.P1Tiebreak, st.P2Tiebreak
	if a < 0 || b < 0 || (ta != nil && *ta < 0) || (tb != nil && *tb < 0) {
		return 0, false, []Violation{{Field: "games", Rule: "set.negative", Message: "scores cannot be negative"}}
	}

	if isDecider {
		// 10-point match tiebreak carried in the games fields.
		if ta != nil || tb != nil {
			return 0, false, []Violation{{Field: "tiebreak", Rule: "set.decider_tiebreak_fields",
				Message: "a deciding match tiebreak carries its points in the games fields"}}
		}
		hi, lo := a, b
		if b > a {
			hi, lo = b, a
		}
		complete := hi >= 10 && hi-lo >= 2 && (hi == 10 || hi-lo == 2)
		if complete {
			if a > b {
				return 1, true, nil
			}
			return 2, true, nil
		}
		if allowPartial {
			return 0, false, nil
		}
		return 0, false, []Violation{{Field: "games", Rule: "set.decider_illegal",
			Message: "a match tiebreak is won at 10+ points with a two-point lead"}}
	}

	// Regular set shapes.
	hi, lo, hiSide := a, b, 1
	if b > a {
		hi, lo, hiSide = b, a, 2
	}
	switch {
	case hi == 6 && lo <= 4:
		if ta != nil || tb != nil {
			return 0, false, []Violation{{Field: "tiebreak", Rule: "set.tiebreak_unexpected",
				Message: "a set not reaching 7-6 has no tiebreak"}}
		}
		return hiSide, true, nil
	case hi == 7 && lo == 5:
		if ta != nil || tb != nil {
			return 0, false, []Violation{{Field: "tiebreak", Rule: "set.tiebreak_unexpected",
				Message: "a 7-5 set has no tiebreak"}}
		}
		return hiSide, true, nil
	case hi == 7 && lo == 6:
		if ta == nil || tb == nil {
			if allowPartial {
				return 0, false, nil // tiebreak in progress
			}
			return 0, false, []Violation{{Field: "tiebreak", Rule: "set.tiebreak_required",
				Message: "a 7-6 set requires explicit tiebreak values"}}
		}
		tw, tl := *ta, *tb
		twSide := 1
		if tl > tw {
			tw, tl, twSide = tl, tw, 2
		}
		if !(tw >= 7 && tw-tl >= 2 && (tw == 7 || tw-tl == 2)) {
			return 0, false, []Violation{{Field: "tiebreak", Rule: "set.tiebreak_illegal",
				Message: "a tiebreak is won at 7+ points with a two-point lead"}}
		}
		if twSide != hiSide {
			return 0, false, []Violation{{Field: "tiebreak", Rule: "set.tiebreak_winner_mismatch",
				Message: "the tiebreak winner must match the set winner"}}
		}
		return hiSide, true, nil
	}

	// Not a complete shape.
	if allowPartial {
		partialOK := hi <= 6 || (hi == 7 && lo == 6) || (hi == 6 && lo == 6)
		if hi <= 5 || (hi == 6 && lo >= 5) {
			partialOK = true
		}
		if hi > 7 || (hi == 7 && lo < 5) {
			partialOK = false
		}
		if partialOK {
			return 0, false, nil
		}
	}
	return 0, false, []Violation{{Field: "games", Rule: "set.illegal",
		Message: fmt.Sprintf("%d-%d is not a legal set score", a, b)}}
}

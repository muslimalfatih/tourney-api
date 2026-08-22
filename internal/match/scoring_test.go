package match

import "testing"

func ip(n int) *int { return &n }

func set(n, a, b int) SetScore { return SetScore{SetNumber: n, P1Games: a, P2Games: b} }
func settb(n, a, b, ta, tb int) SetScore {
	return SetScore{SetNumber: n, P1Games: a, P2Games: b, P1Tiebreak: ip(ta), P2Tiebreak: ip(tb)}
}

var std = DefaultScoringConfig() // best_of 3, full deciding set

func TestValidateScore_Matrix(t *testing.T) {
	mtb := ScoringConfig{BestOf: 3, DecidingSet: "match_tiebreak"}
	bo1 := ScoringConfig{BestOf: 1, DecidingSet: "full"}

	cases := []struct {
		name       string
		cfg        ScoringConfig
		sets       []SetScore
		completion string
		winnerSlot int
		wantWinner int
		wantStatus string
		wantRule   string // "" = valid
	}{
		// --- legal normal completions -----------------------------------
		{"straight sets", std, []SetScore{set(1, 6, 3), set(2, 6, 4)}, "normal", 0, 1, "completed", ""},
		{"three sets", std, []SetScore{set(1, 6, 3), set(2, 4, 6), set(3, 7, 5)}, "normal", 0, 1, "completed", ""},
		{"7-6 with tiebreak", std, []SetScore{settb(1, 7, 6, 7, 3), set(2, 6, 0)}, "normal", 0, 1, "completed", ""},
		{"extended tiebreak 9-7", std, []SetScore{settb(1, 6, 7, 7, 9), set(2, 0, 6)}, "normal", 0, 2, "completed", ""},
		{"deciding match tiebreak", mtb, []SetScore{set(1, 6, 3), set(2, 4, 6), set(3, 10, 7)}, "normal", 0, 1, "completed", ""},
		{"extended match tiebreak 12-10", mtb, []SetScore{set(1, 6, 3), set(2, 4, 6), set(3, 10, 12)}, "normal", 0, 2, "completed", ""},
		{"best of 1", bo1, []SetScore{set(1, 7, 5)}, "normal", 0, 1, "completed", ""},

		// --- illegal set shapes -----------------------------------------
		{"6-5 is not a set", std, []SetScore{set(1, 6, 5), set(2, 6, 0)}, "normal", 0, 0, "", "set.illegal"},
		{"99-3 rejected", std, []SetScore{set(1, 99, 3), set(2, 6, 0)}, "normal", 0, 0, "", "set.illegal"},
		{"negative games", std, []SetScore{set(1, -1, 6), set(2, 6, 0)}, "normal", 0, 0, "", "set.negative"},
		{"8-6 is not a set", std, []SetScore{set(1, 8, 6), set(2, 6, 0)}, "normal", 0, 0, "", "set.illegal"},
		{"7-6 without tiebreak", std, []SetScore{set(1, 7, 6), set(2, 6, 0)}, "normal", 0, 0, "", "set.tiebreak_required"},
		{"tiebreak 7-6 not two clear", std, []SetScore{settb(1, 7, 6, 7, 6), set(2, 6, 0)}, "normal", 0, 0, "", "set.tiebreak_illegal"},
		{"tiebreak 8-5 beyond 7 not exact", std, []SetScore{settb(1, 7, 6, 8, 5), set(2, 6, 0)}, "normal", 0, 0, "", "set.tiebreak_illegal"},
		{"tiebreak winner mismatch", std, []SetScore{settb(1, 7, 6, 2, 7), set(2, 6, 0)}, "normal", 0, 0, "", "set.tiebreak_winner_mismatch"},
		{"tiebreak on 6-3 set", std, []SetScore{settb(1, 6, 3, 7, 2), set(2, 6, 0)}, "normal", 0, 0, "", "set.tiebreak_unexpected"},
		{"match tiebreak 10-9", mtb, []SetScore{set(1, 6, 3), set(2, 4, 6), set(3, 10, 9)}, "normal", 0, 0, "", "set.decider_illegal"},
		{"match tiebreak 12-9", mtb, []SetScore{set(1, 6, 3), set(2, 4, 6), set(3, 12, 9)}, "normal", 0, 0, "", "set.decider_illegal"},
		{"match tiebreak with tb fields", mtb, []SetScore{set(1, 6, 3), set(2, 4, 6), settb(3, 10, 7, 10, 7)}, "normal", 0, 0, "", "set.decider_tiebreak_fields"},

		// --- chain rules -------------------------------------------------
		{"extra set after decided", std, []SetScore{set(1, 6, 0), set(2, 6, 0), set(3, 6, 0)}, "normal", 0, 0, "", "sets.after_decided"},
		{"too many sets", std, []SetScore{set(1, 6, 0), set(2, 0, 6), set(3, 6, 0), set(4, 6, 0)}, "normal", 0, 0, "", "sets.too_many"},
		{"duplicate set numbers", std, []SetScore{set(1, 6, 0), set(1, 6, 0)}, "normal", 0, 0, "", "sets.numbering"},
		{"gap in numbering", std, []SetScore{set(1, 6, 0), set(3, 6, 0)}, "normal", 0, 0, "", "sets.numbering"},
		{"unfinished normal", std, []SetScore{set(1, 6, 0)}, "normal", 0, 0, "", "sets.unfinished"},
		{"empty normal", std, nil, "normal", 0, 0, "", "sets.required"},
		{"second set of best-of-1", bo1, []SetScore{set(1, 6, 0), set(2, 6, 0)}, "normal", 0, 0, "", "sets.too_many"},

		// --- completion states -------------------------------------------
		{"walkover clean", std, nil, "walkover", 2, 2, "walkover", ""},
		{"walkover with sets", std, []SetScore{set(1, 6, 0)}, "walkover", 1, 0, "", "sets.walkover"},
		{"walkover without winner", std, nil, "walkover", 0, 0, "", "winner.required"},
		{"retired partial", std, []SetScore{set(1, 6, 3), set(2, 2, 1)}, "retired", 1, 1, "retired", ""},
		{"retired mid tiebreak", std, []SetScore{set(1, 6, 6)}, "retired", 2, 2, "retired", ""},
		{"retired without winner", std, []SetScore{set(1, 6, 3)}, "retired", 0, 0, "", "winner.required"},
		{"retired after decided", std, []SetScore{set(1, 6, 0), set(2, 6, 0)}, "retired", 2, 2, "retired", ""},
		{"cancelled no sets", std, nil, "cancelled", 0, 0, "cancelled", ""},
		{"cancelled partial", std, []SetScore{set(1, 3, 3)}, "cancelled", 0, 0, "cancelled", ""},
		{"cancelled with winner", std, nil, "cancelled", 1, 0, "", "winner.forbidden"},
		{"incomplete partial", std, []SetScore{set(1, 6, 3), set(2, 5, 5)}, "incomplete", 0, 0, "live", ""},
		{"incomplete but decided", std, []SetScore{set(1, 6, 0), set(2, 6, 0)}, "incomplete", 0, 0, "", "completion.decided"},
		{"normal with winner_slot", std, []SetScore{set(1, 6, 0), set(2, 6, 0)}, "normal", 1, 0, "", "winner.forbidden"},
		{"bogus completion", std, []SetScore{set(1, 6, 0)}, "finished", 0, 0, "", "completion.invalid"},
		{"partial illegal games", std, []SetScore{set(1, 9, 1)}, "incomplete", 0, 0, "", "set.illegal"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			winner, status, violations := ValidateScore(c.cfg, c.sets, c.completion, c.winnerSlot)
			if c.wantRule == "" {
				if len(violations) != 0 {
					t.Fatalf("want valid, got violations %v", violations)
				}
				if winner != c.wantWinner || status != c.wantStatus {
					t.Fatalf("got (winner %d, status %s), want (%d, %s)", winner, status, c.wantWinner, c.wantStatus)
				}
				return
			}
			found := false
			for _, v := range violations {
				if v.Rule == c.wantRule {
					found = true
				}
			}
			if !found {
				t.Fatalf("want violation %q, got %v", c.wantRule, violations)
			}
		})
	}
}

func TestParseScoringConfig(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantRule string
		want     ScoringConfig
	}{
		{"empty means defaults", ``, "", DefaultScoringConfig()},
		{"empty object", `{}`, "", DefaultScoringConfig()},
		{"full valid", `{"best_of":3,"deciding_set":"match_tiebreak","golden_point":true}`, "",
			ScoringConfig{BestOf: 3, DecidingSet: "match_tiebreak", GoldenPoint: true}},
		{"best_of 5 rejected", `{"best_of":5}`, "config.best_of", ScoringConfig{}},
		{"bad deciding_set", `{"deciding_set":"super"}`, "config.deciding_set", ScoringConfig{}},
		{"unknown key", `{"sets_to_win":2}`, "config.unknown_key", ScoringConfig{}},
		{"mtb with best_of 1", `{"best_of":1,"deciding_set":"match_tiebreak"}`, "config.match_tiebreak_best_of_1", ScoringConfig{}},
		{"not an object", `[1,2]`, "config.invalid_json", ScoringConfig{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, violations := ParseScoringConfig([]byte(c.raw))
			if c.wantRule == "" {
				if len(violations) != 0 {
					t.Fatalf("want valid, got %v", violations)
				}
				if cfg != c.want {
					t.Fatalf("got %+v, want %+v", cfg, c.want)
				}
				return
			}
			found := false
			for _, v := range violations {
				if v.Rule == c.wantRule {
					found = true
				}
			}
			if !found {
				t.Fatalf("want %q, got %v", c.wantRule, violations)
			}
		})
	}
}

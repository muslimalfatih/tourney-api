//go:build integration

package inttest

import (
	"context"
	"testing"
	"time"
)

// TestScoringValidation covers the Phase 3.4 contract end-to-end against the
// real database: structured 422s that write nothing and broadcast nothing,
// the distinct completion states (walkover / retired / cancelled) with their
// standings and progression semantics, and the scoring-config plumbing.
func TestScoringValidation(t *testing.T) {
	e := setup(t)
	tok := e.login(t, organizerEmail)
	names := []string{"SV A / 1", "SV B / 2", "SV C / 3", "SV D / 4"}
	_, evID, pids := makeElimFixture(t, e, tok, "itest-scoring", names, [][2]int{{0, 1}, {2, 3}})
	ms := listMatches(t, e, tok, evID)
	m1, m2 := ms[1], ms[2]

	// --- 422: illegal score writes nothing and broadcasts nothing ----------
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, stStream := streamEvents(ctx, t, e, "itest-scoring")
	if stStream != 200 {
		t.Fatalf("stream: %d", stStream)
	}
	expectEvent(t, ch, "connected", 2*time.Second)

	illegal := []map[string]any{
		{"sets": []map[string]any{{"set_number": 1, "games_a": 6, "games_b": 5}, {"set_number": 2, "games_a": 6, "games_b": 0}}, "completion": "normal"},
		{"sets": []map[string]any{{"set_number": 1, "games_a": 7, "games_b": 6}, {"set_number": 2, "games_a": 6, "games_b": 0}}, "completion": "normal"}, // 7-6 without tiebreak
		{"sets": []map[string]any{{"set_number": 1, "games_a": 6, "games_b": 0}}, "completion": "normal"},                                                // unfinished
		{"sets": []map[string]any{{"set_number": 1, "games_a": 6, "games_b": 0}, {"set_number": 2, "games_a": 6, "games_b": 0}, {"set_number": 3, "games_a": 6, "games_b": 0}}, "completion": "normal"},
		{"sets": []map[string]any{}, "completion": "walkover"}, // winner_slot missing
	}
	for i, body := range illegal {
		st, res := e.call(t, "PATCH", "/matches/"+m1.id+"/score", body, tok)
		if st != 422 {
			t.Fatalf("illegal case %d = %d, want 422 (%v)", i, st, res)
		}
		errObj := res["error"].(map[string]any)
		if errObj["code"] != "invalid_score" {
			t.Errorf("case %d code = %v", i, errObj["code"])
		}
		if _, ok := errObj["details"].(map[string]any)["violations"]; !ok {
			t.Errorf("case %d carries no violations detail: %v", i, res)
		}
	}
	var scoreRows int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM match_scores WHERE match_id = $1`, m1.id).Scan(&scoreRows); err != nil {
		t.Fatal(err)
	}
	if scoreRows != 0 {
		t.Errorf("illegal submissions wrote %d set rows, want 0", scoreRows)
	}
	expectNoEvent(t, ch, "match.score", 1200*time.Millisecond)

	// --- legal 7-6 with tiebreak metadata ----------------------------------
	st, res := e.call(t, "PATCH", "/matches/"+m1.id+"/score", map[string]any{
		"sets": []map[string]any{
			{"set_number": 1, "games_a": 7, "games_b": 6, "tiebreak_a": 7, "tiebreak_b": 3},
			{"set_number": 2, "games_a": 6, "games_b": 4},
		},
		"completion": "normal",
	}, tok)
	if st != 200 {
		t.Fatalf("legal 7-6 rejected: %d %v", st, res)
	}
	expectEvent(t, ch, "match.score", 3*time.Second)

	// --- walkover: no sets, explicit winner, advances the bracket ----------
	st, res = e.call(t, "PATCH", "/matches/"+m2.id+"/score", map[string]any{
		"sets": []map[string]any{}, "completion": "walkover", "winner_slot": 2,
	}, tok)
	if st != 200 {
		t.Fatalf("walkover: %d %v", st, res)
	}
	ms = listMatches(t, e, tok, evID)
	if ms[2].status != "walkover" {
		t.Errorf("walkover status = %s", ms[2].status)
	}
	if ms[3].slotPID[2] != pids[3] {
		t.Errorf("walkover winner not advanced: final slot2 = %q, want %q", ms[3].slotPID[2], pids[3])
	}

	// --- audit records the completion type ---------------------------------
	var walkoverAudits int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_logs
		WHERE action = 'match.score' AND target_id = $1 AND diff->>'completion' = 'walkover'`,
		m2.id).Scan(&walkoverAudits); err != nil {
		t.Fatal(err)
	}
	if walkoverAudits != 1 {
		t.Errorf("walkover audit rows = %d, want 1", walkoverAudits)
	}

	// --- retired: partial sets + explicit winner ---------------------------
	final := ms[3]
	st, res = e.call(t, "PATCH", "/matches/"+final.id+"/score", map[string]any{
		"sets":       []map[string]any{{"set_number": 1, "games_a": 6, "games_b": 3}, {"set_number": 2, "games_a": 2, "games_b": 1}},
		"completion": "retired", "winner_slot": 1,
	}, tok)
	if st != 200 {
		t.Fatalf("retired: %d %v", st, res)
	}
	if listMatches(t, e, tok, evID)[3].status != "retired" {
		t.Errorf("retired status not applied")
	}

	// --- walkover counts in standings, cancelled does not ------------------
	// Round-robin fixture (published — standings is a public read).
	f := makeFixture(t, e, tok, "itest-scoring-rr")
	if st, res = e.call(t, "POST", "/tournaments/"+f.tournamentID+"/publish", nil, tok); st != 200 {
		t.Fatalf("publish rr: %d %v", st, res)
	}
	st, _ = e.call(t, "PATCH", "/matches/"+f.matchID+"/score", map[string]any{
		"sets": []map[string]any{}, "completion": "walkover", "winner_slot": 1,
	}, tok)
	if st != 200 {
		t.Fatal("rr walkover")
	}
	st, res = e.call(t, "GET", "/public/events/"+f.eventID+"/standings", nil, "")
	if st != 200 {
		t.Fatalf("standings: %d %v", st, res)
	}
	rows := res["data"].(map[string]any)["standings"].([]any)
	top := rows[0].(map[string]any)
	if top["won"].(float64) != 1 {
		t.Errorf("walkover not counted in standings: %v", top)
	}

	// Cancel it → the win disappears from standings entirely.
	st, _ = e.call(t, "PATCH", "/matches/"+f.matchID+"/score", map[string]any{
		"sets": []map[string]any{}, "completion": "cancelled",
	}, tok)
	if st != 200 {
		t.Fatal("cancel correction")
	}
	_, res = e.call(t, "GET", "/public/events/"+f.eventID+"/standings", nil, "")
	for _, rr := range res["data"].(map[string]any)["standings"].([]any) {
		if rr.(map[string]any)["won"].(float64) != 0 {
			t.Errorf("cancelled match still counted: %v", rr)
		}
	}

	// --- scoring config: best_of 1 enforced, invalid config 422 ------------
	st, res = e.call(t, "PATCH", "/events/"+f.eventID, map[string]any{
		"scoring": map[string]any{"best_of": 1},
	}, tok)
	if st != 200 {
		t.Fatalf("set scoring config: %d %v", st, res)
	}
	st, res = e.call(t, "PATCH", "/matches/"+f.matchID+"/score", map[string]any{
		"sets":       []map[string]any{{"set_number": 1, "games_a": 6, "games_b": 2}, {"set_number": 2, "games_a": 6, "games_b": 2}},
		"completion": "normal",
	}, tok)
	if st != 422 {
		t.Errorf("two sets under best_of 1 = %d, want 422 (%v)", st, res)
	}
	st, res = e.call(t, "PATCH", "/matches/"+f.matchID+"/score", map[string]any{
		"sets":       []map[string]any{{"set_number": 1, "games_a": 6, "games_b": 2}},
		"completion": "normal",
	}, tok)
	if st != 200 {
		t.Errorf("single set under best_of 1 = %d, want 200 (%v)", st, res)
	}

	st, res = e.call(t, "PATCH", "/events/"+f.eventID, map[string]any{
		"scoring": map[string]any{"best_of": 5},
	}, tok)
	if st != 422 || res["error"].(map[string]any)["code"] != "invalid_scoring_config" {
		t.Errorf("invalid config = %d %v, want 422 invalid_scoring_config", st, res)
	}
}

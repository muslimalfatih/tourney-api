//go:build integration

package inttest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// helpers ------------------------------------------------------------------

// makeElimFixture: published tournament + single_elim division + pairs +
// bracket built from explicit R1 pairings (participant-id pairs).
func makeElimFixture(t *testing.T, e *env, tok, slug string, pairNames []string, r1 [][2]int) (tid, evID string, pids []string) {
	t.Helper()
	st, res := e.call(t, "POST", "/tournaments",
		map[string]any{"name": "IT " + slug, "slug": slug, "sport": "tennis"}, tok)
	if st != 201 {
		t.Fatalf("create tournament: %d %v", st, res)
	}
	tid = res["data"].(map[string]any)["id"].(string)
	st, res = e.call(t, "POST", "/tournaments/"+tid+"/events",
		map[string]any{"name": "IT Elim", "discipline": "doubles", "format": "single_elim"}, tok)
	if st != 201 {
		t.Fatalf("create event: %d %v", st, res)
	}
	evID = res["data"].(map[string]any)["id"].(string)
	for _, n := range pairNames {
		st, res = e.call(t, "POST", "/events/"+evID+"/participants", map[string]any{"display_name": n}, tok)
		if st != 201 {
			t.Fatalf("add pair: %d %v", st, res)
		}
		pids = append(pids, res["data"].(map[string]any)["id"].(string))
	}
	var pairs []map[string]any
	for _, p := range r1 {
		pairs = append(pairs, map[string]any{"team_a_id": pids[p[0]], "team_b_id": pids[p[1]]})
	}
	st, res = e.call(t, "POST", "/events/"+evID+"/bracket/build", map[string]any{"matches": pairs}, tok)
	if st != 200 && st != 201 {
		t.Fatalf("build: %d %v", st, res)
	}
	if st, res = e.call(t, "POST", "/tournaments/"+tid+"/publish", nil, tok); st != 200 {
		t.Fatalf("publish: %d %v", st, res)
	}
	return tid, evID, pids
}

type matchView struct {
	id, status string
	no         int
	slotPID    map[int]string // slot -> participant id ("" when empty)
	sets       []map[string]any
}

func listMatches(t *testing.T, e *env, tok, evID string) map[int]matchView {
	t.Helper()
	st, res := e.call(t, "GET", "/events/"+evID+"/matches", nil, tok)
	if st != 200 {
		t.Fatalf("list matches: %d %v", st, res)
	}
	out := map[int]matchView{}
	for _, raw := range res["data"].([]any) {
		m := raw.(map[string]any)
		mv := matchView{
			id:      m["id"].(string),
			status:  m["status"].(string),
			no:      int(m["match_no"].(float64)),
			slotPID: map[int]string{},
		}
		if parts, ok := m["participants"].([]any); ok {
			for _, rp := range parts {
				sv := rp.(map[string]any)
				pid, _ := sv["participant_id"].(string)
				mv.slotPID[int(sv["slot"].(float64))] = pid
			}
		}
		if sets, ok := m["sets"].([]any); ok {
			for _, rs := range sets {
				mv.sets = append(mv.sets, rs.(map[string]any))
			}
		}
		out[mv.no] = mv
	}
	return out
}

// score posts a result oriented so the pair in winnerSlot wins.
// complete=true submits two identical sets as completion "normal";
// complete=false submits ONE set as completion "incomplete" (a reopen /
// partial score — two decided sets would be rejected as completion.decided).
func score(t *testing.T, e *env, tok, matchID string, winnerSlot int, complete bool, games [2]int) (int, map[string]any) {
	t.Helper()
	p1, p2 := games[0], games[1]
	if winnerSlot == 2 {
		p1, p2 = p2, p1
	}
	sets := []map[string]any{{"set_number": 1, "games_a": p1, "games_b": p2}}
	completion := "incomplete"
	if complete {
		sets = append(sets, map[string]any{"set_number": 2, "games_a": p1, "games_b": p2})
		completion = "normal"
	}
	return e.call(t, "PATCH", "/matches/"+matchID+"/score", map[string]any{
		"sets": sets, "completion": completion,
	}, tok)
}

func lockedDetails(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	errObj, _ := res["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "downstream_phase_locked" {
		t.Fatalf("expected downstream_phase_locked, got %v", res)
	}
	det, _ := errObj["details"].(map[string]any)
	if det == nil {
		t.Fatalf("409 carries no details: %v", res)
	}
	var out []map[string]any
	for _, l := range det["locked"].([]any) {
		out = append(out, l.(map[string]any))
	}
	if len(out) == 0 {
		t.Fatalf("locked list empty: %v", res)
	}
	return out
}

// tests ---------------------------------------------------------------------

func TestCorrectionSingleElim(t *testing.T) {
	e := setup(t)
	tok := e.login(t, organizerEmail)
	names := []string{"CX A / 1", "CX B / 2", "CX C / 3", "CX D / 4"}
	_, evID, pids := makeElimFixture(t, e, tok, "itest-corr-elim", names, [][2]int{{0, 1}, {2, 3}})

	ms := listMatches(t, e, tok, evID)
	m1, m2, final := ms[1], ms[2], ms[3]

	// Baseline: completing M1 advances slot-1 pair into the final.
	if st, res := score(t, e, tok, m1.id, 1, true, [2]int{6, 3}); st != 200 {
		t.Fatalf("complete M1: %d %v", st, res)
	}
	ms = listMatches(t, e, tok, evID)
	if ms[3].slotPID[1] != pids[0] {
		t.Fatalf("baseline advancement failed: final slot1 = %q", ms[3].slotPID[1])
	}

	// 1. Winner change while the final is untouched → downstream rebuilt.
	if st, res := score(t, e, tok, m1.id, 2, true, [2]int{6, 4}); st != 200 {
		t.Fatalf("correct M1: %d %v", st, res)
	}
	ms = listMatches(t, e, tok, evID)
	if ms[3].slotPID[1] != pids[1] {
		t.Errorf("corrected winner not rebuilt downstream: final slot1 = %q, want %q", ms[3].slotPID[1], pids[1])
	}

	// 2. Un-complete → stale winner removed, slot back to unresolved.
	if st, res := score(t, e, tok, m1.id, 2, false, [2]int{6, 4}); st != 200 {
		t.Fatalf("uncomplete M1: %d %v", st, res)
	}
	ms = listMatches(t, e, tok, evID)
	if ms[1].status != "live" {
		t.Errorf("uncompleted match status = %s, want live", ms[1].status)
	}
	if ms[3].slotPID[1] != "" {
		t.Errorf("stale winner not cleared: final slot1 = %q", ms[3].slotPID[1])
	}

	// Restore M1 (pair 0 wins) and complete M2 so the final is fully seeded.
	if st, _ := score(t, e, tok, m1.id, 1, true, [2]int{6, 2}); st != 200 {
		t.Fatal("re-complete M1")
	}
	if st, _ := score(t, e, tok, m2.id, 1, true, [2]int{6, 1}); st != 200 {
		t.Fatal("complete M2")
	}

	// 3. Downstream live → 409 with details, and the transaction rolls back.
	if st, res := e.call(t, "PATCH", "/matches/"+final.id+"/status", map[string]string{"status": "live"}, tok); st != 200 {
		t.Fatalf("final live: %d %v", st, res)
	}
	// Rollback proof reads the set rows straight from the database (the list
	// endpoint doesn't hydrate sets).
	setRow := func() (n int, p1 int) {
		if err := e.pool.QueryRow(context.Background(), `
			SELECT count(*), COALESCE(min(p1_games) FILTER (WHERE set_number = 1), -1)
			FROM match_scores WHERE match_id = $1`, m1.id).Scan(&n, &p1); err != nil {
			t.Fatal(err)
		}
		return
	}
	nBefore, p1Before := setRow()
	st, res := score(t, e, tok, m1.id, 2, true, [2]int{7, 5})
	if st != 409 {
		t.Fatalf("locked correction = %d, want 409 (%v)", st, res)
	}
	locked := lockedDetails(t, res)
	if locked[0]["match_id"] != final.id || int(locked[0]["slot"].(float64)) != 1 {
		t.Errorf("locked details wrong: %v", locked)
	}
	after := listMatches(t, e, tok, evID)
	if after[3].slotPID[1] != pids[0] {
		t.Errorf("locked correction leaked into downstream slot")
	}
	nAfter, p1After := setRow()
	if nAfter != nBefore || p1After != p1Before {
		t.Errorf("409 did not roll back the set rewrite: (%d,%d) -> (%d,%d)", nBefore, p1Before, nAfter, p1After)
	}
	if after[1].status != "completed" {
		t.Errorf("source match status mutated despite rollback: %s", after[1].status)
	}

	// 5. Same-winner re-score with a live downstream → allowed (sets only).
	if st, res := score(t, e, tok, m1.id, 1, true, [2]int{6, 0}); st != 200 {
		t.Errorf("same-winner re-score should pass: %d %v", st, res)
	}

	// 4. Downstream with score data (but back to scheduled) still locks.
	if st, _ := score(t, e, tok, final.id, 1, false, [2]int{3, 2}); st != 200 { // partial → live + score rows
		t.Fatal("partial-score final")
	}
	if st, _ := e.call(t, "PATCH", "/matches/"+final.id+"/status", map[string]string{"status": "scheduled"}, tok); st != 200 {
		t.Fatal("final back to scheduled")
	}
	if st, res := score(t, e, tok, m1.id, 2, true, [2]int{7, 5}); st != 409 {
		t.Errorf("scored-but-scheduled downstream must lock: %d %v", st, res)
	}

	// 13. Completed matches cannot be demoted via the status endpoint.
	st, res = e.call(t, "PATCH", "/matches/"+m1.id+"/status", map[string]string{"status": "live"}, tok)
	if st != 409 || res["error"].(map[string]any)["code"] != "completed_immutable" {
		t.Errorf("status demotion = %d %v, want 409 completed_immutable", st, res)
	}

	// 11. Audit trail: entries exist for this match with before/after diffs.
	var auditCount int
	var lastDiff string
	err := e.pool.QueryRow(context.Background(), `
		SELECT count(*), max(diff::text) FILTER (WHERE diff::text LIKE '%"downstream_rebuilt"%')
		FROM audit_logs WHERE action = 'match.score' AND target_id = $1`, m1.id).
		Scan(&auditCount, &lastDiff)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if auditCount < 4 {
		t.Errorf("expected >=4 audit rows for M1 corrections, got %d", auditCount)
	}
	if !strings.Contains(lastDiff, `"before"`) || !strings.Contains(lastDiff, `"after"`) {
		t.Errorf("audit diff lacks before/after: %s", lastDiff)
	}
}

func TestCorrectionByesAndMultipleConsumers(t *testing.T) {
	e := setup(t)
	tok := e.login(t, organizerEmail)
	names := []string{"CB A / 1", "CB B / 2", "CB C / 3", "CB D / 4", "CB E / 5", "CB F / 6"}
	// Three R1 pairings over six pairs → generator pads to four with a bye.
	_, evID, _ := makeElimFixture(t, e, tok, "itest-corr-bye", names,
		[][2]int{{0, 1}, {2, 3}, {4, 5}})

	ms := listMatches(t, e, tok, evID)
	var byeNo int
	for no, m := range ms {
		if m.status == "bye" {
			byeNo = no
		}
	}
	if byeNo == 0 {
		t.Fatal("no bye match generated")
	}

	// 10. A second consumer of match 1's winner, crafted directly: point the
	// bye-fed semifinal slot at match 1 as well.
	m1 := ms[1]
	var hijacked string
	err := e.pool.QueryRow(context.Background(), `
		UPDATE match_participants mp SET source_type = 'match_winner', source_match_id = $1
		WHERE mp.id = (
			SELECT mp2.id FROM match_participants mp2
			JOIN matches m ON m.id = mp2.match_id
			WHERE m.event_id = $2 AND mp2.source_type = 'match_winner'
			  AND mp2.source_match_id = (SELECT id FROM matches WHERE event_id = $2 AND match_no = $3)
			LIMIT 1)
		RETURNING mp.match_id::text`, m1.id, evID, byeNo).Scan(&hijacked)
	if err != nil {
		t.Fatalf("craft second consumer: %v", err)
	}

	// Complete match 1 → BOTH consumers receive the winner.
	if st, res := score(t, e, tok, m1.id, 1, true, [2]int{6, 3}); st != 200 {
		t.Fatalf("complete M1: %d %v", st, res)
	}
	var fed int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM match_participants
		WHERE source_match_id = $1 AND participant_id IS NOT NULL`, m1.id).Scan(&fed); err != nil {
		t.Fatal(err)
	}
	if fed != 2 {
		t.Errorf("multiple consumers: %d slots fed, want 2", fed)
	}

	// 9. Bye slots survive corrections untouched.
	if st, _ := score(t, e, tok, m1.id, 2, true, [2]int{6, 4}); st != 200 {
		t.Fatal("correct M1")
	}
	var byes int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM match_participants mp
		JOIN matches m ON m.id = mp.match_id
		WHERE m.event_id = $1 AND mp.source_type = 'bye' AND mp.participant_id IS NULL`, evID).
		Scan(&byes); err != nil {
		t.Fatal(err)
	}
	if byes == 0 {
		t.Errorf("bye slots were disturbed by the correction")
	}
}

func TestCorrectionGroupsAndSSE(t *testing.T) {
	e := setup(t)
	tok := e.login(t, organizerEmail)

	// Published group_knockout: one group "Pool Y" of four, advance 2 → the
	// knockout is a single final fed by Winner + Runner-up.
	st, res := e.call(t, "POST", "/tournaments",
		map[string]any{"name": "IT corr groups", "slug": "itest-corr-gk", "sport": "tennis"}, tok)
	if st != 201 {
		t.Fatal("create tournament")
	}
	tid := res["data"].(map[string]any)["id"].(string)
	st, res = e.call(t, "POST", "/tournaments/"+tid+"/events",
		map[string]any{"name": "IT GK", "discipline": "doubles", "format": "group_knockout"}, tok)
	if st != 201 {
		t.Fatal("create event")
	}
	evID := res["data"].(map[string]any)["id"].(string)
	var pids []string
	for _, n := range []string{"CG A / 1", "CG B / 2", "CG C / 3", "CG D / 4"} {
		st, res = e.call(t, "POST", "/events/"+evID+"/participants", map[string]any{"display_name": n}, tok)
		if st != 201 {
			t.Fatal("add pair")
		}
		pids = append(pids, res["data"].(map[string]any)["id"].(string))
	}
	st, _ = e.call(t, "PUT", "/events/"+evID+"/groups", map[string]any{
		"groups": []map[string]any{{"name": "Pool Y", "advance_count": 2, "team_ids": pids}},
	}, tok)
	if st != 200 && st != 201 {
		t.Fatal("set groups")
	}
	if st, _ = e.call(t, "POST", "/tournaments/"+tid+"/publish", nil, tok); st != 200 {
		t.Fatal("publish")
	}

	// Complete all six group matches: lower index always wins → standings
	// A(3W), B(2W), C(1W), D(0W). idx lookup by participant id.
	idx := map[string]int{}
	for i, p := range pids {
		idx[p] = i
	}
	ms := listMatches(t, e, tok, evID)
	var koNo int
	var h2h [4][4]string // matchID by pair indices
	for no, m := range ms {
		a, b := m.slotPID[1], m.slotPID[2]
		if a == "" || b == "" {
			koNo = no
			continue
		}
		h2h[idx[a]][idx[b]] = m.id
		h2h[idx[b]][idx[a]] = m.id
		winnerSlot := 1
		if idx[b] < idx[a] {
			winnerSlot = 2
		}
		if st, res := score(t, e, tok, m.id, winnerSlot, true, [2]int{6, 3}); st != 200 {
			t.Fatalf("complete group match: %d %v", st, res)
		}
	}
	ms = listMatches(t, e, tok, evID)
	ko := ms[koNo]
	if ko.slotPID[1] != pids[0] || ko.slotPID[2] != pids[1] {
		t.Fatalf("initial resolution wrong: %v", ko.slotPID)
	}

	// 6. Correct the A-vs-B head-to-head so B overtakes A → placements swap.
	abID := h2h[0][1]
	abView := matchViewByID(ms, abID)
	winnerSlot := 1
	if abView.slotPID[1] == pids[0] {
		winnerSlot = 2 // make B (index 1) win
	}
	if st, res := score(t, e, tok, abID, winnerSlot, true, [2]int{7, 5}); st != 200 {
		t.Fatalf("group correction: %d %v", st, res)
	}
	ms = listMatches(t, e, tok, evID)
	ko = ms[koNo]
	if ko.slotPID[1] != pids[1] || ko.slotPID[2] != pids[0] {
		t.Errorf("placements not rebuilt after group correction: %v", ko.slotPID)
	}

	// 7 + 12. Knockout live: a placement-changing correction must 409 with the
	// affected slots — and the public stream must stay silent for the failed
	// attempt (nothing emitted after rollback).
	if st, _ := e.call(t, "PATCH", "/matches/"+ko.id+"/status", map[string]string{"status": "live"}, tok); st != 200 {
		t.Fatal("ko live")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, stStream := streamEvents(ctx, t, e, "itest-corr-gk")
	if stStream != 200 {
		t.Fatalf("stream connect: %d", stStream)
	}
	expectEvent(t, ch, "connected", 2*time.Second)

	winnerSlot = 3 - winnerSlot // flip back → A overtakes B again
	st, res = score(t, e, tok, abID, winnerSlot, true, [2]int{6, 2})
	if st != 409 {
		t.Fatalf("locked group correction = %d, want 409 (%v)", st, res)
	}
	details := lockedDetails(t, res)
	if details[0]["match_id"] != ko.id {
		t.Errorf("locked details point at %v, want KO final %s", details[0], ko.id)
	}
	expectNoEvent(t, ch, "match.score", 1500*time.Millisecond)

	// 8. Reopen a group match with the knockout back to scheduled → group is
	// incomplete → placements clear to unresolved.
	if st, _ := e.call(t, "PATCH", "/matches/"+ko.id+"/status", map[string]string{"status": "scheduled"}, tok); st != 200 {
		t.Fatal("ko scheduled")
	}
	cdID := h2h[2][3]
	cdView := matchViewByID(listMatches(t, e, tok, evID), cdID)
	reopenSlot := 1
	if cdView.slotPID[1] != pids[2] {
		reopenSlot = 2
	}
	if st, res := score(t, e, tok, cdID, reopenSlot, false, [2]int{6, 3}); st != 200 {
		t.Fatalf("reopen group match: %d %v", st, res)
	}
	ms = listMatches(t, e, tok, evID)
	ko = ms[koNo]
	if ko.slotPID[1] != "" || ko.slotPID[2] != "" {
		t.Errorf("placements not cleared for incomplete group: %v", ko.slotPID)
	}

	// A successful correction after all that DOES broadcast.
	if st, _ := score(t, e, tok, cdID, reopenSlot, true, [2]int{6, 3}); st != 200 {
		t.Fatal("re-complete group match")
	}
	expectEvent(t, ch, "match.score", 3*time.Second)
}

func matchViewByID(ms map[int]matchView, id string) matchView {
	for _, m := range ms {
		if m.id == id {
			return m
		}
	}
	return matchView{}
}

var _ = json.Marshal // keep import if helpers change

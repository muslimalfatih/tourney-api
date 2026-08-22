//go:build integration

package inttest

import (
	"context"
	"sync"
	"testing"
)

// TestDuplicateFixtures covers Phase 3.7 against the real DB: unordered-pair
// duplicate detection on manual creates, the allow_rematch override with its
// audit trail, cancelled-fixture recovery, idempotent round-robin generation,
// and concurrency (two racing creates / generators never both slip through).
func TestDuplicateFixtures(t *testing.T) {
	e := setup(t)
	tok := e.login(t, organizerEmail)

	makeRR := func(slug string, names []string) (evID string, pids []string) {
		st, res := e.call(t, "POST", "/tournaments",
			map[string]any{"name": "IT " + slug, "slug": slug, "sport": "tennis"}, tok)
		if st != 201 {
			t.Fatalf("create tournament: %d %v", st, res)
		}
		tid := res["data"].(map[string]any)["id"].(string)
		st, res = e.call(t, "POST", "/tournaments/"+tid+"/events",
			map[string]any{"name": "IT Dup Div", "discipline": "doubles", "format": "round_robin"}, tok)
		if st != 201 {
			t.Fatalf("create event: %d %v", st, res)
		}
		evID = res["data"].(map[string]any)["id"].(string)
		for _, n := range names {
			st, res = e.call(t, "POST", "/events/"+evID+"/participants", map[string]any{"display_name": n}, tok)
			if st != 201 {
				t.Fatalf("add pair: %d %v", st, res)
			}
			pids = append(pids, res["data"].(map[string]any)["id"].(string))
		}
		return evID, pids
	}
	create := func(evID, a, b string, allow bool) (int, map[string]any) {
		return e.call(t, "POST", "/events/"+evID+"/matches",
			map[string]any{"team_a_id": a, "team_b_id": b, "allow_rematch": allow}, tok)
	}
	dupDetails := func(res map[string]any) map[string]any {
		errObj, _ := res["error"].(map[string]any)
		if errObj == nil || errObj["code"] != "duplicate_fixture" {
			t.Fatalf("expected duplicate_fixture, got %v", res)
		}
		return errObj["details"].(map[string]any)
	}
	matchCount := func(evID string) int {
		var n int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM matches WHERE event_id = $1`, evID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	evID, pids := makeRR("itest-dup", []string{"IT Dup A / 1", "IT Dup B / 2", "IT Dup C / 3"})
	A, B, C := pids[0], pids[1], pids[2]

	// 1. First fixture creates; identical and REVERSED pairs then 409 with the
	//    existing match identified, and allow_rematch cannot override unplayed.
	st, res := create(evID, A, B, false)
	if st != 201 {
		t.Fatalf("first create: %d %v", st, res)
	}
	m1 := res["data"].(map[string]any)["id"].(string)

	for _, attempt := range [][2]string{{A, B}, {B, A}} {
		st, res = create(evID, attempt[0], attempt[1], false)
		if st != 409 {
			t.Fatalf("duplicate = %d, want 409 (%v)", st, res)
		}
		det := dupDetails(res)
		if det["rematchable"] != false || det["match_no"].(float64) != 1 {
			t.Errorf("duplicate details = %v", det)
		}
	}
	st, res = create(evID, A, B, true)
	if st != 409 {
		t.Errorf("allow_rematch overrode an UNPLAYED fixture: %d %v", st, res)
	}
	if got := matchCount(evID); got != 1 {
		t.Fatalf("rejected creates wrote rows: count = %d", got)
	}

	// 2. Decided fixture → confirmation required; override creates + audits.
	st, res = e.call(t, "PATCH", "/matches/"+m1+"/score", map[string]any{
		"sets":       []map[string]any{{"set_number": 1, "games_a": 6, "games_b": 0}, {"set_number": 2, "games_a": 6, "games_b": 0}},
		"completion": "normal",
	}, tok)
	if st != 200 {
		t.Fatalf("complete m1: %d %v", st, res)
	}
	st, res = create(evID, A, B, false)
	if st != 409 {
		t.Fatalf("post-completion duplicate = %d, want 409 (%v)", st, res)
	}
	if det := dupDetails(res); det["rematchable"] != true || det["status"] != "completed" {
		t.Errorf("rematchable details = %v", det)
	}
	st, res = create(evID, B, A, true) // reversed order + override
	if st != 201 {
		t.Fatalf("rematch create: %d %v", st, res)
	}
	rematchID := res["data"].(map[string]any)["id"].(string)
	var rematchAudits int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_logs
		WHERE action = 'match.rematch_override' AND target_id = $1
		  AND diff->>'prior_match_id' = $2`, rematchID, m1).Scan(&rematchAudits); err != nil {
		t.Fatal(err)
	}
	if rematchAudits != 1 {
		t.Errorf("rematch audit rows = %d, want 1", rematchAudits)
	}

	// 3. A cancelled fixture is voided: re-creating its pairing needs no flag.
	st, res = create(evID, A, C, false)
	if st != 201 {
		t.Fatalf("A-C create: %d %v", st, res)
	}
	acID := res["data"].(map[string]any)["id"].(string)
	st, res = e.call(t, "PATCH", "/matches/"+acID+"/score",
		map[string]any{"sets": []map[string]any{}, "completion": "cancelled"}, tok)
	if st != 200 {
		t.Fatalf("cancel A-C: %d %v", st, res)
	}
	st, res = create(evID, A, C, false)
	if st != 201 {
		t.Errorf("recreate after cancellation = %d, want 201 (%v)", st, res)
	}

	// 4. Generator fills only the missing pairing (B-C) and is idempotent.
	st, res = e.call(t, "POST", "/events/"+evID+"/matches/generate", nil, tok)
	if st != 200 {
		t.Fatalf("generate: %d %v", st, res)
	}
	gen := res["data"].(map[string]any)
	if gen["created"].(float64) != 1 || gen["existing"].(float64) != 2 {
		t.Errorf("generate = %v, want created 1 / existing 2", gen)
	}
	st, res = e.call(t, "POST", "/events/"+evID+"/matches/generate", nil, tok)
	if st != 200 || res["data"].(map[string]any)["created"].(float64) != 0 {
		t.Errorf("regenerate = %d %v, want created 0", st, res)
	}

	// 5. Concurrency: racing identical creates → exactly one 201; racing
	//    generators → the full pairing set exists exactly once.
	ev2, p2 := makeRR("itest-dup-race", []string{"IT R A/1", "IT R B/2", "IT R C/3", "IT R D/4"})
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = rawPost(e, "/events/"+ev2+"/matches",
				map[string]any{"team_a_id": p2[0], "team_b_id": p2[1]}, tok)
		}(i)
	}
	wg.Wait()
	ok, conflicted := 0, 0
	for _, s := range statuses {
		switch s {
		case 201:
			ok++
		case 409:
			conflicted++
		}
	}
	if ok != 1 || conflicted != 1 {
		t.Errorf("racing creates = %v, want one 201 and one 409", statuses)
	}

	genStatuses := make([]int, 2)
	for i := range genStatuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			genStatuses[i] = rawPost(e, "/events/"+ev2+"/matches/generate", map[string]any{}, tok)
		}(i)
	}
	wg.Wait()
	for _, s := range genStatuses {
		if s != 200 {
			t.Fatalf("concurrent generate statuses = %v", genStatuses)
		}
	}
	if got := matchCount(ev2); got != 6 { // C(4,2)
		t.Errorf("after racing generators: %d fixtures, want 6", got)
	}
	var dupPairs int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM (
			SELECT least(p1.participant_id, p2.participant_id),
			       greatest(p1.participant_id, p2.participant_id)
			FROM matches m
			JOIN match_participants p1 ON p1.match_id = m.id AND p1.slot = 1
			JOIN match_participants p2 ON p2.match_id = m.id AND p2.slot = 2
			WHERE m.event_id = $1
			GROUP BY 1, 2
			HAVING count(*) > 1) d`, ev2).Scan(&dupPairs); err != nil {
		t.Fatal(err)
	}
	if dupPairs != 0 {
		t.Errorf("racing generators produced %d duplicated pairings", dupPairs)
	}
}

//go:build integration

package inttest

import (
	"testing"
	"time"
)

// TestTypedGroupPlacementResolution is the regression for two silent failures
// of the legacy label-string resolver, both fixed by the typed slot model
// (Phase 3.1/3.2):
//
//  1. Ranks past 2 never resolved ("#3 …" labels weren't in the label map).
//  2. Custom group names never resolved (labels were letter-based, matching
//     was against the real group name).
//
// One group named "Pool X" advancing THREE: before the fix, the winner and
// runner-up resolved and the #3 slot stayed empty forever.
func TestTypedGroupPlacementResolution(t *testing.T) {
	e := setup(t)
	tok := e.login(t, organizerEmail)

	// Tournament + group_knockout division + four pairs.
	st, res := e.call(t, "POST", "/tournaments",
		map[string]any{"name": "IT typed slots", "slug": "itest-typed", "sport": "tennis"}, tok)
	if st != 201 {
		t.Fatalf("create tournament: %d %v", st, res)
	}
	tid := res["data"].(map[string]any)["id"].(string)
	st, res = e.call(t, "POST", "/tournaments/"+tid+"/events",
		map[string]any{"name": "IT GK", "discipline": "doubles", "format": "group_knockout"}, tok)
	if st != 201 {
		t.Fatalf("create event: %d %v", st, res)
	}
	evID := res["data"].(map[string]any)["id"].(string)

	pairNames := []string{"IT Alpha / One", "IT Bravo / Two", "IT Charlie / Three", "IT Delta / Four"}
	pidByName := map[string]string{}
	var pids []string
	for _, name := range pairNames {
		st, res = e.call(t, "POST", "/events/"+evID+"/participants", map[string]any{"display_name": name}, tok)
		if st != 201 {
			t.Fatalf("add pair: %d %v", st, res)
		}
		id := res["data"].(map[string]any)["id"].(string)
		pidByName[name] = id
		pids = append(pids, id)
	}

	// One custom-named group, advance_count 3.
	st, res = e.call(t, "PUT", "/events/"+evID+"/groups", map[string]any{
		"groups": []map[string]any{{"name": "Pool X", "advance_count": 3, "team_ids": pids}},
	}, tok)
	if st != 200 && st != 201 {
		t.Fatalf("set groups: %d %v", st, res)
	}

	// Complete every group match; the LOWER pair index always wins, so the
	// final standings are exactly pairNames order: Alpha, Bravo, Charlie.
	idx := map[string]int{}
	for i, n := range pairNames {
		idx[pidByName[n]] = i
	}
	st, res = e.call(t, "GET", "/events/"+evID+"/matches", nil, tok)
	if st != 200 {
		t.Fatalf("list matches: %d %v", st, res)
	}
	completed := 0
	for _, raw := range res["data"].([]any) {
		m := raw.(map[string]any)
		if m["status"] != "pending" {
			continue
		}
		parts := m["participants"].([]any)
		var slotPID [3]string // 1-indexed by slot
		filled := 0
		for _, rp := range parts {
			sv := rp.(map[string]any)
			if pid, ok := sv["participant_id"].(string); ok && pid != "" {
				slotPID[int(sv["slot"].(float64))] = pid
				filled++
			}
		}
		if filled != 2 {
			continue // knockout placeholder — untouched
		}
		// Winner is the lower spec index; orient the set games accordingly.
		p1, p2 := 6, 3
		if idx[slotPID[2]] < idx[slotPID[1]] {
			p1, p2 = 3, 6
		}
		st, res = e.call(t, "PATCH", "/matches/"+m["id"].(string)+"/score", map[string]any{
			"sets":       []map[string]any{{"set_number": 1, "games_a": p1, "games_b": p2}, {"set_number": 2, "games_a": p1, "games_b": p2}},
			"completion": "normal",
		}, tok)
		if st != 200 {
			t.Fatalf("complete group match: %d %v", st, res)
		}
		completed++
	}
	if completed != 6 { // C(4,2)
		t.Fatalf("completed %d group matches, want 6", completed)
	}

	// The last completion auto-resolves; give the read a moment then assert.
	time.Sleep(300 * time.Millisecond)
	st, res = e.call(t, "GET", "/events/"+evID+"/groups", nil, tok)
	if st != 200 {
		t.Fatalf("read group-knockout: %d %v", st, res)
	}
	data := res["data"].(map[string]any)

	// Collect the knockout round-1 slots: label → resolved participant id.
	resolved := map[string]string{}
	byes := 0
	for _, rr := range data["knockout"].([]any) {
		round := rr.(map[string]any)
		if round["round_number"].(float64) != 1 {
			continue
		}
		for _, rm := range round["matches"].([]any) {
			for _, rs := range rm.(map[string]any)["participants"].([]any) {
				sv := rs.(map[string]any)
				label, _ := sv["source_label"].(string)
				pid, _ := sv["participant_id"].(string)
				if label != "" {
					resolved[label] = pid
				} else if pid == "" {
					byes++
				}
			}
		}
	}

	want := map[string]string{
		"Winner Pool X":    pidByName["IT Alpha / One"],
		"Runner-up Pool X": pidByName["IT Bravo / Two"],
		"#3 Pool X":        pidByName["IT Charlie / Three"], // THE regression
	}
	for label, wantPID := range want {
		got, ok := resolved[label]
		if !ok {
			t.Errorf("knockout slot %q missing — label not generated from the real group name", label)
			continue
		}
		if got == "" {
			t.Errorf("knockout slot %q unresolved", label)
			continue
		}
		if got != wantPID {
			t.Errorf("knockout slot %q resolved to wrong pair", label)
		}
	}
	if byes != 1 {
		t.Errorf("expected exactly 1 bye pad slot in round 1, found %d", byes)
	}
}

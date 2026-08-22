//go:build integration

package inttest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestSchedulingConflicts covers Phase 3.5 end-to-end against the real DB:
// hard court/participant overlap 422s that write nothing and broadcast
// nothing, half-open adjacency, the rest-buffer warning + explicit override,
// audit records, the btree_gist exclusion constraint as concurrency backstop,
// and post-commit-only SSE.
func TestSchedulingConflicts(t *testing.T) {
	e := setup(t)
	tok := e.login(t, organizerEmail)

	// Fixture: published tournament, RR division, three pairs, two matches
	// sharing pair A (A-B and A-C), two courts.
	st, res := e.call(t, "POST", "/tournaments",
		map[string]any{"name": "IT sched", "slug": "itest-sched", "sport": "tennis"}, tok)
	if st != 201 {
		t.Fatalf("create tournament: %d %v", st, res)
	}
	tid := res["data"].(map[string]any)["id"].(string)
	st, res = e.call(t, "POST", "/tournaments/"+tid+"/events",
		map[string]any{"name": "IT Sched Div", "discipline": "doubles", "format": "round_robin"}, tok)
	if st != 201 {
		t.Fatalf("create event: %d %v", st, res)
	}
	evID := res["data"].(map[string]any)["id"].(string)

	var pids []string
	for _, n := range []string{"IT Sched A / 1", "IT Sched B / 2", "IT Sched C / 3"} {
		st, res = e.call(t, "POST", "/events/"+evID+"/participants", map[string]any{"display_name": n}, tok)
		if st != 201 {
			t.Fatalf("add pair: %d %v", st, res)
		}
		pids = append(pids, res["data"].(map[string]any)["id"].(string))
	}
	newMatch := func(a, b int) string {
		st, res = e.call(t, "POST", "/events/"+evID+"/matches",
			map[string]any{"team_a_id": pids[a], "team_b_id": pids[b]}, tok)
		if st != 201 {
			t.Fatalf("add match: %d %v", st, res)
		}
		return res["data"].(map[string]any)["id"].(string)
	}
	match1, match2 := newMatch(0, 1), newMatch(0, 2)

	var courts []string
	for _, n := range []string{"IT Court 1", "IT Court 2"} {
		st, res = e.call(t, "POST", "/tournaments/"+tid+"/courts", map[string]any{"name": n}, tok)
		if st != 201 {
			t.Fatalf("add court: %d %v", st, res)
		}
		courts = append(courts, res["data"].(map[string]any)["id"].(string))
	}
	if st, res = e.call(t, "POST", "/tournaments/"+tid+"/publish", nil, tok); st != 200 {
		t.Fatalf("publish: %d %v", st, res)
	}

	at := func(hhmm string) string { return "2027-03-01T" + hhmm + ":00Z" }
	slotBody := func(court, match, from, to string, override bool) map[string]any {
		b := map[string]any{
			"tournament_id": tid, "court_id": court,
			"starts_at": at(from), "ends_at": at(to),
			"override_rest_buffer": override,
		}
		if match != "" {
			b["match_id"] = match
		}
		return b
	}
	slotCount := func() int {
		var n int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM schedule_slots WHERE tournament_id = $1`, tid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, stStream := streamEvents(ctx, t, e, "itest-sched")
	if stStream != 200 {
		t.Fatalf("stream: %d", stStream)
	}
	expectEvent(t, ch, "connected", 2*time.Second)

	// 1. Baseline: match1 on court1 10:00-11:30 → created + broadcast.
	st, res = e.call(t, "POST", "/schedule/slots", slotBody(courts[0], match1, "10:00", "11:30", false), tok)
	if st != 201 {
		t.Fatalf("baseline slot: %d %v", st, res)
	}
	slot1 := res["data"].(map[string]any)["id"].(string)
	expectEvent(t, ch, "schedule.updated", 3*time.Second)

	// 2. Adjacent held slot on the same court (half-open '[)') → legal.
	st, res = e.call(t, "POST", "/schedule/slots", slotBody(courts[0], "", "11:30", "13:00", false), tok)
	if st != 201 {
		t.Fatalf("adjacent slot rejected: %d %v", st, res)
	}
	heldSlot := res["data"].(map[string]any)["id"].(string)
	expectEvent(t, ch, "schedule.updated", 3*time.Second)

	// 3. Court overlap → 422 schedule_conflict, nothing written, no SSE.
	before := slotCount()
	st, res = e.call(t, "POST", "/schedule/slots", slotBody(courts[0], "", "10:30", "11:30", false), tok)
	if st != 422 {
		t.Fatalf("court overlap = %d, want 422 (%v)", st, res)
	}
	errObj := res["error"].(map[string]any)
	if errObj["code"] != "schedule_conflict" {
		t.Errorf("court overlap code = %v", errObj["code"])
	}
	confs := errObj["details"].(map[string]any)["conflicts"].([]any)
	if len(confs) == 0 || confs[0].(map[string]any)["type"] != "court_overlap" {
		t.Errorf("conflict detail = %v", confs)
	}

	// 4. Participant overlap on ANOTHER court (pair A in both matches) → 422.
	st, res = e.call(t, "POST", "/schedule/slots", slotBody(courts[1], match2, "11:00", "12:30", false), tok)
	if st != 422 {
		t.Fatalf("participant overlap = %d, want 422 (%v)", st, res)
	}
	errObj = res["error"].(map[string]any)
	confs = errObj["details"].(map[string]any)["conflicts"].([]any)
	if len(confs) == 0 || confs[0].(map[string]any)["type"] != "participant_overlap" {
		t.Errorf("participant conflict detail = %v (code %v)", confs, errObj["code"])
	}
	if got := slotCount(); got != before {
		t.Errorf("rejected writes changed slot count: %d -> %d", before, got)
	}
	// match2 must not have been stamped by the rejected write.
	var stamped bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT scheduled_at IS NOT NULL FROM matches WHERE id = $1`, match2).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped {
		t.Error("rejected write stamped the match")
	}
	expectNoEvent(t, ch, "schedule.updated", 1200*time.Millisecond)

	// 5. Rest buffer: match2 starts 15 min after pair A finishes → warning 422;
	//    explicit override schedules it and the override is audited.
	st, res = e.call(t, "POST", "/schedule/slots", slotBody(courts[1], match2, "11:45", "13:15", false), tok)
	if st != 422 {
		t.Fatalf("rest buffer = %d, want 422 (%v)", st, res)
	}
	errObj = res["error"].(map[string]any)
	if errObj["code"] != "insufficient_rest" {
		t.Errorf("rest code = %v", errObj["code"])
	}
	warns := errObj["details"].(map[string]any)["warnings"].([]any)
	if len(warns) == 0 || warns[0].(map[string]any)["type"] != "rest_buffer" {
		t.Errorf("rest warning detail = %v", warns)
	}
	st, res = e.call(t, "POST", "/schedule/slots", slotBody(courts[1], match2, "11:45", "13:15", true), tok)
	if st != 201 {
		t.Fatalf("override rejected: %d %v", st, res)
	}
	overrideSlot := res["data"].(map[string]any)["id"].(string)
	expectEvent(t, ch, "schedule.updated", 3*time.Second)

	var overrideAudits int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_logs
		WHERE action = 'schedule.slot.create' AND target_id = $1
		  AND (diff->>'rest_override')::bool`, overrideSlot).Scan(&overrideAudits); err != nil {
		t.Fatal(err)
	}
	if overrideAudits != 1 {
		t.Errorf("override audit rows = %d, want 1", overrideAudits)
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT scheduled_at IS NOT NULL AND status = 'scheduled' FROM matches WHERE id = $1`,
		match2).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if !stamped {
		t.Error("override create did not stamp the match")
	}

	// 6. Update into a conflict → 422; the slot keeps its stored time.
	st, res = e.call(t, "PATCH", "/schedule/slots/"+heldSlot, map[string]any{
		"court_id": courts[0], "starts_at": at("10:00"), "ends_at": at("11:00"),
	}, tok)
	if st != 422 {
		t.Fatalf("conflicting update = %d, want 422 (%v)", st, res)
	}
	var storedStart time.Time
	if err := e.pool.QueryRow(context.Background(),
		`SELECT starts_at FROM schedule_slots WHERE id = $1`, heldSlot).Scan(&storedStart); err != nil {
		t.Fatal(err)
	}
	if !storedStart.Equal(mustTime(t, at("11:30"))) {
		t.Errorf("rejected update mutated the slot: %v", storedStart)
	}

	// 7. Delete un-stamps the match once no slot references it.
	if st, res = e.call(t, "DELETE", "/schedule/slots/"+overrideSlot, nil, tok); st != 204 {
		t.Fatalf("delete: %d %v", st, res)
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT scheduled_at IS NULL AND status = 'pending' FROM matches WHERE id = $1`,
		match2).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if !stamped {
		t.Error("delete did not un-stamp the match")
	}
	var delAudits int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = 'schedule.slot.delete' AND target_id = $1`,
		overrideSlot).Scan(&delAudits); err != nil {
		t.Fatal(err)
	}
	if delAudits != 1 {
		t.Errorf("delete audit rows = %d, want 1", delAudits)
	}

	// 8. DB backstop: a direct overlapping insert trips the exclusion
	//    constraint (23P01); an adjacent one passes.
	_, err := e.pool.Exec(context.Background(), `
		INSERT INTO schedule_slots (tournament_id, court_id, starts_at, ends_at)
		VALUES ($1, $2, $3, $4)`, tid, courts[0], mustTime(t, at("10:15")), mustTime(t, at("10:45")))
	if err == nil {
		t.Error("exclusion constraint did not block a direct overlapping insert")
	}
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO schedule_slots (tournament_id, court_id, starts_at, ends_at)
		VALUES ($1, $2, $3, $4)`, tid, courts[0], mustTime(t, at("13:00")), mustTime(t, at("13:30"))); err != nil {
		t.Errorf("adjacent direct insert blocked: %v", err)
	}
	// Inverted range trips the CHECK.
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO schedule_slots (tournament_id, court_id, starts_at, ends_at)
		VALUES ($1, $2, $3, $4)`, tid, courts[1], mustTime(t, at("15:00")), mustTime(t, at("14:00"))); err == nil {
		t.Error("time_valid CHECK did not block an inverted range")
	}

	// 9. Concurrency: two simultaneous submissions for the same free window on
	//    one court → exactly one succeeds.
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = rawPost(e, "/schedule/slots", slotBody(courts[1], "", "16:00", "17:00", false), tok)
		}(i)
	}
	wg.Wait()
	ok, rejected := 0, 0
	for _, s := range statuses {
		switch {
		case s == 201:
			ok++
		case s == 422 || s == 409:
			rejected++
		}
	}
	if ok != 1 || rejected != 1 {
		t.Errorf("concurrent submissions = %v, want exactly one 201 and one 422/409", statuses)
	}

	_ = slot1
}

func mustTime(t *testing.T, iso string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// rawPost is e.call without *testing.T so it is safe inside goroutines
// (t.Fatal must not be called off the test goroutine).
func rawPost(e *env, path string, body map[string]any, token string) int {
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", e.url(path), bytes.NewReader(payload))
	if err != nil {
		return -1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer res.Body.Close()
	return res.StatusCode
}

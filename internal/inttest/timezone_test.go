//go:build integration

package inttest

import "testing"

// TestTournamentTimezone covers Phase 3.6's API contract: default on create,
// valid IANA zones accepted and round-tripped, invalid zones rejected with the
// structured 422, and the zone present in both organizer and public payloads.
func TestTournamentTimezone(t *testing.T) {
	e := setup(t)
	tok := e.login(t, organizerEmail)

	// Default: no timezone on create -> Asia/Makassar.
	st, res := e.call(t, "POST", "/tournaments",
		map[string]any{"name": "IT tz default", "slug": "itest-tz-default", "sport": "tennis"}, tok)
	if st != 201 {
		t.Fatalf("create default: %d %v", st, res)
	}
	data := res["data"].(map[string]any)
	if data["timezone"] != "Asia/Makassar" {
		t.Errorf("default timezone = %v, want Asia/Makassar", data["timezone"])
	}
	defaultID := data["id"].(string)

	// Valid explicit zone round-trips through create and organizer read.
	st, res = e.call(t, "POST", "/tournaments", map[string]any{
		"name": "IT tz sydney", "slug": "itest-tz-sydney", "sport": "tennis",
		"timezone": "Australia/Sydney",
	}, tok)
	if st != 201 {
		t.Fatalf("create sydney: %d %v", st, res)
	}
	sydneyID := res["data"].(map[string]any)["id"].(string)
	st, res = e.call(t, "GET", "/tournaments/"+sydneyID, nil, tok)
	if st != 200 || res["data"].(map[string]any)["timezone"] != "Australia/Sydney" {
		t.Errorf("organizer read timezone = %v (%d)", res["data"], st)
	}

	// Invalid zone on create -> structured 422.
	st, res = e.call(t, "POST", "/tournaments", map[string]any{
		"name": "IT tz bad", "slug": "itest-tz-bad", "sport": "tennis",
		"timezone": "Bali/Kuta",
	}, tok)
	if st != 422 {
		t.Fatalf("invalid tz create = %d, want 422 (%v)", st, res)
	}
	errObj := res["error"].(map[string]any)
	if errObj["code"] != "invalid_timezone" {
		t.Errorf("code = %v", errObj["code"])
	}
	det := errObj["details"].(map[string]any)
	if det["field"] != "timezone" || det["value"] != "Bali/Kuta" {
		t.Errorf("details = %v", det)
	}

	// PATCH: valid update applies, invalid rejects and leaves the row alone.
	st, res = e.call(t, "PATCH", "/tournaments/"+defaultID,
		map[string]any{"timezone": "Asia/Jakarta"}, tok)
	if st != 200 || res["data"].(map[string]any)["timezone"] != "Asia/Jakarta" {
		t.Errorf("tz update = %d %v", st, res)
	}
	st, res = e.call(t, "PATCH", "/tournaments/"+defaultID,
		map[string]any{"timezone": "Local"}, tok)
	if st != 422 || res["error"].(map[string]any)["code"] != "invalid_timezone" {
		t.Errorf("Local tz update = %d %v, want 422 invalid_timezone", st, res)
	}
	st, res = e.call(t, "GET", "/tournaments/"+defaultID, nil, tok)
	if st != 200 || res["data"].(map[string]any)["timezone"] != "Asia/Jakarta" {
		t.Errorf("rejected update mutated timezone: %v", res["data"])
	}

	// Public payload carries the zone.
	if st, res = e.call(t, "POST", "/tournaments/"+sydneyID+"/publish", nil, tok); st != 200 {
		t.Fatalf("publish: %d %v", st, res)
	}
	st, res = e.call(t, "GET", "/public/tournaments/itest-tz-sydney", nil, "")
	if st != 200 {
		t.Fatalf("public read: %d %v", st, res)
	}
	if res["data"].(map[string]any)["timezone"] != "Australia/Sydney" {
		t.Errorf("public timezone = %v", res["data"].(map[string]any)["timezone"])
	}
}

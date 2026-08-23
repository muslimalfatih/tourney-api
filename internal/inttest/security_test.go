//go:build integration

// Package inttest holds DB-backed integration tests. They run the REAL gin
// engine (same wiring as cmd/api/main.go) against a real Postgres and assert
// the security matrix from Refactor Phase 1 stays true:
//
//	go test -tags integration ./internal/inttest/... -count=1
//
// Connection comes from TEST_DATABASE_URL, falling back to DATABASE_URL. The
// suite is self-contained: it creates (and reuses) its own `itest-*` org,
// users and tournaments, and wipes its own tournaments at startup — it never
// touches seed or user data. Requires migrations to be applied.
package inttest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/tourney-api/internal/audit"
	"github.com/muslimalfatih/tourney-api/internal/auth"
	"github.com/muslimalfatih/tourney-api/internal/config"
	"github.com/muslimalfatih/tourney-api/internal/draw"
	"github.com/muslimalfatih/tourney-api/internal/event"
	"github.com/muslimalfatih/tourney-api/internal/match"
	"github.com/muslimalfatih/tourney-api/internal/participant"
	"github.com/muslimalfatih/tourney-api/internal/platform"
	"github.com/muslimalfatih/tourney-api/internal/realtime"
	"github.com/muslimalfatih/tourney-api/internal/schedule"
	"github.com/muslimalfatih/tourney-api/internal/server"
	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
	"github.com/muslimalfatih/tourney-api/internal/storage/postgres"
	"github.com/muslimalfatih/tourney-api/internal/tournament"
)

const (
	orgSlug        = "itest-org"
	organizerEmail = "itest-organizer@tourney.test"
	adminEmail     = "itest-admin@tourney.test"
	password       = "itest-password-2026"
)

type env struct {
	ts   *httptest.Server
	pool *pgxpool.Pool
}

func (e *env) url(path string) string { return e.ts.URL + "/api/v1" + path }

// call is the one HTTP helper every test uses.
func (e *env) call(t *testing.T, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, e.url(path), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (e *env) login(t *testing.T, email string) string {
	t.Helper()
	st, res := e.call(t, "POST", "/auth/login", map[string]string{"email": email, "password": password}, "")
	if st != 200 {
		t.Fatalf("login %s: %d %v", email, st, res)
	}
	return res["data"].(map[string]any)["access_token"].(string)
}

// setup builds the real engine, provisions the itest org/users idempotently,
// and wipes any itest tournaments from a previous run so every run starts from
// the same state.
func setup(t *testing.T) *env {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL / DATABASE_URL not set — skipping integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := postgres.Connect(ctx, dbURL, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Idempotent test principals. Passwords hashed with the app's own helper.
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	var orgID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO organizations (name, slug) VALUES ('Integration Test Org', $1)
		ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, orgSlug).Scan(&orgID); err != nil {
		t.Fatalf("itest org: %v", err)
	}
	for _, u := range []struct{ email, role string }{
		{organizerEmail, "organizer"},
		{adminEmail, "super_admin"},
	} {
		org := any(orgID)
		if u.role == "super_admin" {
			org = nil // users_org_scope CHECK: super_admin must have NULL org
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO users (email, password_hash, name, role, org_id)
			VALUES ($1, $2, $3, $4::user_role, $5)
			ON CONFLICT (email) DO UPDATE SET password_hash = EXCLUDED.password_hash`,
			u.email, hash, "Integration "+u.role, u.role, org); err != nil {
			t.Fatalf("itest user %s: %v", u.email, err)
		}
	}
	// Fresh slate: cascade wipes events/matches/participants/slots under them.
	if _, err := pool.Exec(ctx, `DELETE FROM tournaments WHERE slug LIKE 'itest-%'`); err != nil {
		t.Fatalf("wipe itest tournaments: %v", err)
	}

	// Real engine, wired exactly like cmd/api/main.go.
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Env: "test", CORSOrigins: []string{"http://localhost"}}
	tokens := auth.NewTokenService("itest-secret-0123456789abcdef", 15*time.Minute, time.Hour)
	hub := realtime.NewHub()
	drawService := draw.NewService(pool)
	tournamentService := tournament.NewService(pool)
	authHandler := auth.NewHandler(auth.NewService(auth.NewRepository(pool), tokens))
	realtimeHandler := realtime.NewHandler(hub, tournamentService.IsPublishedSlug)
	tournamentHandler := tournament.NewHandler(tournamentService)
	eventHandler := event.NewHandler(event.NewService(pool), drawService)
	participantHandler := participant.NewHandler(participant.NewService(pool))
	matchHandler := match.NewHandler(match.NewService(pool), hub)
	scheduleHandler := schedule.NewHandler(schedule.NewService(pool), hub)
	platformHandler := platform.NewHandler(platform.NewService(pool))
	auditHandler := audit.NewHandler(audit.NewService(pool))

	engine := server.New(server.Deps{
		Config: cfg, Log: slog.New(slog.DiscardHandler), Pool: pool, Verifier: tokens,
		RegisterAuthRoutes: func(rg *gin.RouterGroup, v middleware.TokenVerifier) { authHandler.Register(rg, v) },
		RegisterPublicRoutes: func(rg *gin.RouterGroup) {
			tournamentHandler.RegisterPublic(rg)
			eventHandler.RegisterPublic(rg)
			participantHandler.RegisterPublic(rg)
			matchHandler.RegisterPublic(rg)
			scheduleHandler.RegisterPublic(rg)
			realtimeHandler.Register(rg)
		},
		RegisterOrganizerRoutes: func(rg *gin.RouterGroup) {
			tournamentHandler.RegisterOrganizer(rg)
			eventHandler.RegisterOrganizer(rg)
			participantHandler.RegisterOrganizer(rg)
			matchHandler.RegisterOrganizer(rg)
			scheduleHandler.RegisterOrganizer(rg)
		},
		RegisterAdminRoutes: func(rg *gin.RouterGroup) {
			platformHandler.RegisterAdmin(rg)
			auditHandler.RegisterAdmin(rg)
		},
	})
	ts := httptest.NewServer(engine)
	t.Cleanup(ts.Close)

	return &env{ts: ts, pool: pool}
}

// fixture creates a round_robin tournament with one division, two pairs and
// one manual match, returning ids. Draft unless published afterwards.
type fixture struct {
	tournamentID, slug, eventID, matchID string
}

func makeFixture(t *testing.T, e *env, token, slug string) fixture {
	t.Helper()
	st, res := e.call(t, "POST", "/tournaments",
		map[string]any{"name": "IT " + slug, "slug": slug, "sport": "tennis"}, token)
	if st != 201 {
		t.Fatalf("create tournament: %d %v", st, res)
	}
	tid := res["data"].(map[string]any)["id"].(string)

	st, res = e.call(t, "POST", "/tournaments/"+tid+"/events",
		map[string]any{"name": "IT Division", "discipline": "doubles", "format": "round_robin"}, token)
	if st != 201 {
		t.Fatalf("create event: %d %v", st, res)
	}
	evID := res["data"].(map[string]any)["id"].(string)

	var pids []string
	for _, name := range []string{"IT Pair One / A", "IT Pair Two / B"} {
		st, res = e.call(t, "POST", "/events/"+evID+"/participants",
			map[string]any{"display_name": name}, token)
		if st != 201 {
			t.Fatalf("add participant: %d %v", st, res)
		}
		pids = append(pids, res["data"].(map[string]any)["id"].(string))
	}
	st, res = e.call(t, "POST", "/events/"+evID+"/matches",
		map[string]any{"team_a_id": pids[0], "team_b_id": pids[1]}, token)
	if st != 201 {
		t.Fatalf("add match: %d %v", st, res)
	}
	mid := res["data"].(map[string]any)["id"].(string)
	return fixture{tournamentID: tid, slug: slug, eventID: evID, matchID: mid}
}

// streamEvents opens the SSE stream and forwards each `event:` line's name on
// a channel until the context ends. A nil channel result means connect failed
// with the returned status.
func streamEvents(ctx context.Context, t *testing.T, e *env, slug string) (<-chan string, int) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.url("/public/tournaments/"+slug+"/stream"), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream connect: %v", err)
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, res.StatusCode
	}
	ch := make(chan string, 16)
	go func() {
		defer res.Body.Close()
		defer close(ch)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event:") {
				ch <- strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
		}
	}()
	return ch, 200
}

func expectEvent(t *testing.T, ch <-chan string, name string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before %q arrived", name)
			}
			if ev == name {
				return
			}
		case <-deadline:
			t.Fatalf("no %q event within %s", name, within)
		}
	}
}

func expectNoEvent(t *testing.T, ch <-chan string, name string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return // closed stream cannot deliver anything — fine
			}
			if ev == name {
				t.Fatalf("received %q event that must have been suppressed", name)
			}
		case <-deadline:
			return
		}
	}
}

// TestSecurityMatrix is the committed form of the Phase 1 live verification:
// draft / published / hidden / archived visibility across the match read, the
// SSE stream, and broadcast filtering.
func TestSecurityMatrix(t *testing.T) {
	e := setup(t)
	orgTok := e.login(t, organizerEmail)
	adminTok := e.login(t, adminEmail)

	// --- DRAFT ---------------------------------------------------------
	f := makeFixture(t, e, orgTok, "itest-matrix")

	if st, _ := e.call(t, "GET", "/public/matches/"+f.matchID, nil, ""); st != 404 {
		t.Errorf("draft match read = %d, want 404", st)
	}
	if _, st := streamEvents(t.Context(), t, e, f.slug); st != 404 {
		t.Errorf("draft stream = %d, want 404", st)
	}
	if st, _ := e.call(t, "GET", "/public/tournaments/"+f.slug, nil, ""); st != 404 {
		t.Errorf("draft tournament read = %d, want 404", st)
	}

	// Indistinguishability: a draft match and a nonexistent match answer alike.
	stKnown, bodyKnown := e.call(t, "GET", "/public/matches/"+f.matchID, nil, "")
	stGhost, bodyGhost := e.call(t, "GET", "/public/matches/00000000-0000-0000-0000-000000000000", nil, "")
	if stKnown != stGhost || fmt.Sprint(bodyKnown) != fmt.Sprint(bodyGhost) {
		t.Errorf("draft match (%d %v) distinguishable from unknown match (%d %v)",
			stKnown, bodyKnown, stGhost, bodyGhost)
	}

	// --- PUBLISHED -----------------------------------------------------
	if st, res := e.call(t, "POST", "/tournaments/"+f.tournamentID+"/publish", nil, orgTok); st != 200 {
		t.Fatalf("publish: %d %v", st, res)
	}
	if st, _ := e.call(t, "GET", "/public/matches/"+f.matchID, nil, ""); st != 200 {
		t.Errorf("published match read = %d, want 200", st)
	}

	// Public payload never carries organizer internals.
	_, pub := e.call(t, "GET", "/public/tournaments/"+f.slug, nil, "")
	raw, _ := json.Marshal(pub)
	if strings.Contains(string(raw), "org_id") {
		t.Errorf("public tournament payload leaks org_id: %s", raw)
	}

	// Stream connects; a status change on a public division broadcasts.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, st := streamEvents(ctx, t, e, f.slug)
	if st != 200 {
		t.Fatalf("published stream = %d, want 200", st)
	}
	expectEvent(t, ch, "connected", 2*time.Second)
	if st, res := e.call(t, "PATCH", "/matches/"+f.matchID+"/status",
		map[string]string{"status": "live"}, orgTok); st != 200 {
		t.Fatalf("set live: %d %v", st, res)
	}
	expectEvent(t, ch, "match.status", 3*time.Second)

	// --- HIDDEN DIVISION ----------------------------------------------
	hide := false
	if st, res := e.call(t, "PATCH", "/events/"+f.eventID,
		map[string]any{"is_public": hide}, orgTok); st != 200 {
		t.Fatalf("hide division: %d %v", st, res)
	}
	if st, _ := e.call(t, "GET", "/public/matches/"+f.matchID, nil, ""); st != 404 {
		t.Errorf("hidden-division match read = %d, want 404", st)
	}
	if st, res := e.call(t, "PATCH", "/matches/"+f.matchID+"/status",
		map[string]string{"status": "live"}, orgTok); st != 200 {
		t.Fatalf("set live (hidden): %d %v", st, res)
	}
	expectNoEvent(t, ch, "match.status", 1500*time.Millisecond)

	// --- RESTORED ------------------------------------------------------
	show := true
	if st, _ := e.call(t, "PATCH", "/events/"+f.eventID, map[string]any{"is_public": show}, orgTok); st != 200 {
		t.Fatal("restore division")
	}
	if st, _ := e.call(t, "GET", "/public/matches/"+f.matchID, nil, ""); st != 200 {
		t.Errorf("restored match read != 200")
	}
	if st, _ := e.call(t, "PATCH", "/matches/"+f.matchID+"/status",
		map[string]string{"status": "scheduled"}, orgTok); st != 200 {
		t.Fatal("set scheduled (restored)")
	}
	expectEvent(t, ch, "match.status", 3*time.Second)

	// --- ARCHIVED ------------------------------------------------------
	if st, res := e.call(t, "POST", "/admin/tournaments/"+f.tournamentID+"/status",
		map[string]string{"action": "archive"}, adminTok); st != 200 {
		t.Fatalf("archive: %d %v", st, res)
	}
	if st, _ := e.call(t, "GET", "/public/matches/"+f.matchID, nil, ""); st != 404 {
		t.Errorf("archived match read = %d, want 404", st)
	}
	if _, st := streamEvents(t.Context(), t, e, f.slug); st != 404 {
		t.Errorf("archived stream = %d, want 404", st)
	}
}

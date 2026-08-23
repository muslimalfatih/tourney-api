// Package draw orchestrates draw generation: it loads an event's participants,
// runs the pure format generator (single_elim.go et al.), and persists the
// resulting matches + progression graph. The generators stay HTTP/DB-free; this
// service is the only part that touches the database.
package draw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/tourney-api/internal/audit"
)

var (
	ErrNotFound        = errors.New("event not found")
	ErrForbidden       = errors.New("not permitted")
	ErrNotEnough       = errors.New("need at least 2 participants")
	ErrUnsupportedForm = errors.New("format not yet supported")
	ErrInvalidPairs    = errors.New("invalid manual pairings")
)

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Service
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, audit: audit.NewService(pool)}
}

// BuildInput is a Match-builder request. Pairs are the organizer's explicit
// round-1 matchups (Pair, nil side = bye). Draws are always arranged manually —
// there is no auto/random pairing.
type BuildInput struct {
	Pairs []Pair
}

// Build creates and persists a single-elimination bracket from the Match
// builder's explicit round-1 pairs. It validates the pairs and uses them
// verbatim (nil side = bye). Only single-elimination events are supported.
func (s *Service) Build(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID, in BuildInput) (int, error) {
	// Authorize + read format.
	var ownerOrg uuid.UUID
	var format string
	err := s.pool.QueryRow(ctx, `
		SELECT t.org_id, e.format
		FROM events e JOIN tournaments t ON t.id = e.tournament_id
		WHERE e.id = $1`, eventID).Scan(&ownerOrg, &format)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if orgID != nil && ownerOrg != *orgID {
		return 0, ErrForbidden
	}
	if format != "single_elim" {
		return 0, ErrUnsupportedForm
	}

	// The set of participant ids that legitimately belong to this event; every
	// manual pairing must reference these and nothing else.
	valid := map[uuid.UUID]bool{}
	rows, err := s.pool.Query(ctx, `SELECT id FROM participants WHERE event_id = $1`, eventID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		valid[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	pairs, err := validatePairs(in.Pairs, valid)
	if err != nil {
		return 0, err
	}
	matches := GenerateFromPairs(pairs)
	if len(matches) == 0 {
		return 0, ErrNotEnough
	}

	if err := s.persist(ctx, eventID, matches); err != nil {
		return 0, err
	}
	// Draws are always arranged manually now.
	if _, err := s.pool.Exec(ctx,
		`UPDATE events SET pairing_mode = 'manual'::pairing_mode WHERE id = $1`, eventID); err != nil {
		return 0, err
	}
	return len(matches), nil
}

// validatePairs enforces the Match-builder rules: no half-filled match (exactly
// one side set), no team used twice, and every id must belong to the event.
// Fully-empty pairs are dropped. Requires at least one real matchup.
func validatePairs(in []Pair, valid map[uuid.UUID]bool) ([]Pair, error) {
	seen := map[uuid.UUID]bool{}
	out := make([]Pair, 0, len(in))
	filled := 0
	for _, p := range in {
		a, b := p.A, p.B
		// A half-filled match (one team, no opponent) is not allowed.
		if (a == nil) != (b == nil) {
			return nil, ErrInvalidPairs
		}
		if a == nil && b == nil {
			continue // empty row — skip
		}
		for _, id := range []*uuid.UUID{a, b} {
			if !valid[*id] {
				return nil, ErrInvalidPairs // unknown / not in this event
			}
			if seen[*id] {
				return nil, ErrInvalidPairs // a team can appear at most once
			}
			seen[*id] = true
		}
		out = append(out, Pair{A: a, B: b})
		filled++
	}
	if filled == 0 {
		return nil, ErrNotEnough
	}
	return out, nil
}

// persist writes the generated matches and their progression + slot entries in
// a single transaction. It maps the generator's flat indices to DB uuids, then
// wires next_match_id in a second pass. When any match carries a GroupIndex >= 0
// (group-knockout), it also creates a group stage + group rows and a knockout
// stage, stamping matches.stage_id / group_id.
func (s *Service) persist(ctx context.Context, eventID uuid.UUID, matches []Match) error {
	return s.persistWithGroups(ctx, eventID, matches, nil, nil)
}

// persistWithGroups is persist with optional per-group display names + advance
// counts (indexed by GroupIndex). nil slices fall back to "Group A/B/…" names
// and the schema default advance_count. Used by the manual group_knockout build.
func (s *Service) persistWithGroups(ctx context.Context, eventID uuid.UUID, matches []Match, groupNames []string, groupAdvance []int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Fresh draw: clear existing matches AND stages for this event.
	if _, err := tx.Exec(ctx, `DELETE FROM matches WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM stages WHERE event_id = $1`, eventID); err != nil {
		return err
	}

	// Detect group-knockout (any match with GroupIndex >= 0) and prepare
	// stage/group ids up front.
	hasGroups := false
	maxGroup := -1
	for _, m := range matches {
		if m.GroupIndex >= 0 {
			hasGroups = true
			if m.GroupIndex > maxGroup {
				maxGroup = m.GroupIndex
			}
		}
	}

	var groupStageID, knockoutStageID uuid.UUID
	groupIDs := map[int]uuid.UUID{}
	if hasGroups {
		if err := tx.QueryRow(ctx,
			`INSERT INTO stages (event_id, kind, sort_order) VALUES ($1,'group',0) RETURNING id`,
			eventID).Scan(&groupStageID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO stages (event_id, kind, sort_order) VALUES ($1,'knockout',1) RETURNING id`,
			eventID).Scan(&knockoutStageID); err != nil {
			return err
		}
		for g := 0; g <= maxGroup; g++ {
			name := "Group " + string(rune('A'+g))
			if g < len(groupNames) && groupNames[g] != "" {
				name = groupNames[g]
			}
			advance := 2 // schema default
			if g < len(groupAdvance) && groupAdvance[g] > 0 {
				advance = groupAdvance[g]
			}
			var gid uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO groups (stage_id, name, advance_count) VALUES ($1,$2,$3) RETURNING id`,
				groupStageID, name, advance).Scan(&gid); err != nil {
				return err
			}
			groupIDs[g] = gid
		}
	}

	ids := make([]uuid.UUID, len(matches))

	// Pass 1: insert matches, capturing ids + stage/group assignment.
	for i, m := range matches {
		status := "pending"
		if m.Slot1.IsBye || m.Slot2.IsBye {
			status = "bye"
		}
		var stageID, groupID *uuid.UUID
		if hasGroups {
			if m.GroupIndex >= 0 {
				stageID = &groupStageID
				gid := groupIDs[m.GroupIndex]
				groupID = &gid
			} else {
				stageID = &knockoutStageID
			}
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO matches (event_id, stage_id, group_id, match_no, status)
			VALUES ($1, $2, $3, $4, $5::match_status)
			RETURNING id`, eventID, stageID, groupID, i+1, status).Scan(&id); err != nil {
			return fmt.Errorf("insert match %d: %w", i, err)
		}
		ids[i] = id
	}

	// Pass 2: wire progression + slot participants (or source labels).
	for i, m := range matches {
		if m.NextMatchIdx >= 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE matches SET next_match_id = $1, next_slot = $2 WHERE id = $3`,
				ids[m.NextMatchIdx], m.NextSlot, ids[i]); err != nil {
				return err
			}
		}
		if err := insertSlot(ctx, tx, ids[i], 1, m.Slot1, groupIDs); err != nil {
			return err
		}
		if err := insertSlot(ctx, tx, ids[i], 2, m.Slot2, groupIDs); err != nil {
			return err
		}
	}

	// Pass 3: type the winner feeds. Every fed slot was inserted as 'empty'
	// above; now that all match rows exist, stamp it with its feeder. Only
	// 'empty' slots are upgraded — byes and group placements keep their type.
	for i, m := range matches {
		if m.NextMatchIdx < 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE match_participants
			SET source_type = 'match_winner', source_match_id = $1
			WHERE match_id = $2 AND slot = $3 AND source_type = 'empty'`,
			ids[i], ids[m.NextMatchIdx], m.NextSlot); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// --- Bracket read model (consumed by the custom Svelte renderer) ---

type BracketSlot struct {
	Slot          int        `json:"slot"`
	ParticipantID *uuid.UUID `json:"participant_id"`
	DisplayName   *string    `json:"display_name"`
	Seed          *int       `json:"seed"`
	// SourceLabel is a placeholder like "Winner Group A" for unresolved
	// group-knockout feeds (nil once a participant is assigned).
	SourceLabel *string `json:"source_label"`
}

// BracketSet is one set's games for both sides, for compact score display,
// plus the tiebreak points when the set had one — without them the organizer
// panel could not seed a correction of a 7-6 result (which requires tiebreak
// metadata to resubmit legally).
type BracketSet struct {
	P1  int  `json:"p1"`
	P2  int  `json:"p2"`
	TB1 *int `json:"tb1"`
	TB2 *int `json:"tb2"`
}

type BracketMatch struct {
	ID                  uuid.UUID     `json:"id"`
	MatchNo             int           `json:"match_no"`
	Status              string        `json:"status"`
	WinnerParticipantID *uuid.UUID    `json:"winner_participant_id"`
	CourtID             *uuid.UUID    `json:"court_id"`
	CourtName           *string       `json:"court_name"`
	ScheduledAt         *time.Time    `json:"scheduled_at"`
	Participants        []BracketSlot `json:"participants"`
	Sets                []BracketSet  `json:"sets"`
}

type BracketRound struct {
	RoundNumber int            `json:"round_number"`
	Name        string         `json:"name"`
	Matches     []BracketMatch `json:"matches"`
}

type Bracket struct {
	EventID uuid.UUID      `json:"event_id"`
	Format  string         `json:"format"`
	Rounds  []BracketRound `json:"rounds"`
}

// GetBracket returns the event's persisted draw as a rounds→matches tree. Rounds
// RequirePublic gates the public read-model endpoints (bracket / standings /
// groups): the event must be is_public AND its tournament published. Returns
// ErrNotFound otherwise so hidden or unpublished events 404 identically to a
// nonexistent one — no existence oracle. Mirrors participant.ListPublic +
// tournament.EventsFor gating so the public reads are consistent. Organizer
// reads use org-scoped routes that skip this gate.
func (s *Service) RequirePublic(ctx context.Context, eventID uuid.UUID) error {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM events e
			JOIN tournaments t ON t.id = e.tournament_id
			WHERE e.id = $1 AND e.is_public = true AND t.status = 'published'
		)`, eventID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// roundMatches reads roundsMap[rn], guaranteed non-nil. A map[int][]T lookup
// on a missing key returns the zero value of []T, which is nil — and
// json.Marshal renders a nil slice as `null`, not `[]`. Every consumer of
// this API (public site, organizer dashboard) assumes Matches is always an
// array; a round with genuinely zero matches (no draw generated yet, or a
// later round not yet seeded from earlier winners) must still serialize as
// `"matches": []`. Shared by GetBracket and knockoutRounds below — both build
// a BracketRound the same way, so the guard lives once here rather than
// twice.
func roundMatches(roundsMap map[int][]BracketMatch, rn int) []BracketMatch {
	if m := roundsMap[rn]; m != nil {
		return m
	}
	return []BracketMatch{}
}

// are derived from the progression depth: matches with no next feed the final
// rounds. For M1 we compute a match's round by walking next_match_id chains.
func (s *Service) GetBracket(ctx context.Context, eventID uuid.UUID) (*Bracket, error) {
	var format string
	err := s.pool.QueryRow(ctx, `SELECT format FROM events WHERE id = $1`, eventID).Scan(&format)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// Load matches with their next pointer + winner, plus each slot's participant.
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.match_no, m.status, m.next_match_id, m.winner_participant_id,
		       m.court_id, m.scheduled_at, c.name,
		       mp.slot, mp.participant_id, p.display_name, p.seed
		FROM matches m
		LEFT JOIN courts c ON c.id = m.court_id
		LEFT JOIN match_participants mp ON mp.match_id = m.id
		LEFT JOIN participants p ON p.id = mp.participant_id
		WHERE m.event_id = $1
		ORDER BY m.match_no, mp.slot`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type mrec struct {
		matchNo     int
		status      string
		next        *uuid.UUID
		winner      *uuid.UUID
		courtID     *uuid.UUID
		courtName   *string
		scheduledAt *time.Time
		slots       []BracketSlot
		sets        []BracketSet
	}
	order := []uuid.UUID{}
	byID := map[uuid.UUID]*mrec{}
	for rows.Next() {
		var mid uuid.UUID
		var matchNo int
		var status string
		var next, winner, courtID *uuid.UUID
		var scheduledAt *time.Time
		var courtName *string
		var slot *int
		var pid *uuid.UUID
		var name *string
		var seed *int
		if err := rows.Scan(&mid, &matchNo, &status, &next, &winner, &courtID, &scheduledAt, &courtName, &slot, &pid, &name, &seed); err != nil {
			return nil, err
		}
		rec, ok := byID[mid]
		if !ok {
			rec = &mrec{matchNo: matchNo, status: status, next: next, winner: winner, courtID: courtID, courtName: courtName, scheduledAt: scheduledAt}
			byID[mid] = rec
			order = append(order, mid)
		}
		if slot != nil {
			rec.slots = append(rec.slots, BracketSlot{Slot: *slot, ParticipantID: pid, DisplayName: name, Seed: seed})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load set scores for all matches in this event in one pass.
	srows, err := s.pool.Query(ctx, `
		SELECT ms.match_id, ms.p1_games, ms.p2_games, ms.p1_tiebreak, ms.p2_tiebreak
		FROM match_scores ms
		JOIN matches m ON m.id = ms.match_id
		WHERE m.event_id = $1
		ORDER BY ms.match_id, ms.set_number`, eventID)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var mid uuid.UUID
		var p1, p2 int
		var tb1, tb2 *int
		if err := srows.Scan(&mid, &p1, &p2, &tb1, &tb2); err != nil {
			return nil, err
		}
		if rec, ok := byID[mid]; ok {
			rec.sets = append(rec.sets, BracketSet{P1: p1, P2: p2, TB1: tb1, TB2: tb2})
		}
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}

	// Compute round number = distance to the final (match with next == nil).
	// depthFromFinal: final = highest round. We assign rounds by counting how
	// many hops forward a match is from the final, then invert.
	roundOf := map[uuid.UUID]int{}
	var maxHops int
	var hops func(id uuid.UUID) int
	hops = func(id uuid.UUID) int {
		if v, ok := roundOf[id]; ok {
			return v
		}
		rec := byID[id]
		if rec == nil || rec.next == nil {
			roundOf[id] = 0
			return 0
		}
		v := hops(*rec.next) + 1
		roundOf[id] = v
		return v
	}
	for _, id := range order {
		if h := hops(id); h > maxHops {
			maxHops = h
		}
	}

	// Round number: earliest round = 1. hops(final)=0 → round = maxHops+1.
	roundName := func(fromFinal int) string {
		switch fromFinal {
		case 0:
			return "Final"
		case 1:
			return "Semifinal"
		case 2:
			return "Quarterfinal"
		default:
			return fmt.Sprintf("Round of %d", 1<<(fromFinal+1))
		}
	}

	roundsMap := map[int][]BracketMatch{}
	for _, id := range order {
		rec := byID[id]
		fromFinal := roundOf[id]
		rn := maxHops - fromFinal + 1
		roundsMap[rn] = append(roundsMap[rn], BracketMatch{
			ID: id, MatchNo: rec.matchNo, Status: rec.status,
			WinnerParticipantID: rec.winner, CourtID: rec.courtID, CourtName: rec.courtName,
			ScheduledAt: rec.scheduledAt, Participants: rec.slots, Sets: rec.sets,
		})
	}

	b := &Bracket{EventID: eventID, Format: format}
	for rn := 1; rn <= maxHops+1; rn++ {
		fromFinal := maxHops - rn + 1
		b.Rounds = append(b.Rounds, BracketRound{
			RoundNumber: rn, Name: roundName(fromFinal), Matches: roundMatches(roundsMap, rn),
		})
	}
	return b, nil
}

// --- Round-robin standings ---

type Standing struct {
	ParticipantID uuid.UUID `json:"participant_id"`
	DisplayName   string    `json:"display_name"`
	Seed          *int      `json:"seed"`
	Played        int       `json:"played"`
	Won           int       `json:"won"`
	Lost          int       `json:"lost"`
	SetsFor       int       `json:"sets_for"`
	SetsAgainst   int       `json:"sets_against"`
}

// GetStandings computes a round-robin table from completed matches: matches
// played, won/lost, and sets for/against (game-based set count). Ordered by
// wins desc, then set difference.
func (s *Service) GetStandings(ctx context.Context, eventID uuid.UUID) ([]Standing, error) {
	// One row per participant with aggregates over their completed matches.
	const q = `
		WITH parts AS (
			SELECT p.id, p.display_name, p.seed FROM participants p WHERE p.event_id = $1
		),
		played AS (
			SELECT mp.participant_id AS pid,
			       m.id AS match_id, m.winner_participant_id, mp.slot
			FROM matches m
			JOIN match_participants mp ON mp.match_id = m.id
			WHERE m.event_id = $1 AND m.status IN ('completed','walkover','retired') AND mp.participant_id IS NOT NULL
		),
		setcounts AS (
			SELECT pl.pid,
			       COALESCE(SUM(CASE WHEN pl.slot = 1 THEN ms.p1_games ELSE ms.p2_games END),0) AS sf,
			       COALESCE(SUM(CASE WHEN pl.slot = 1 THEN ms.p2_games ELSE ms.p1_games END),0) AS sa
			FROM played pl
			LEFT JOIN match_scores ms ON ms.match_id = pl.match_id
			GROUP BY pl.pid
		)
		SELECT pr.id, pr.display_name, pr.seed,
		       COUNT(pl.match_id) AS played,
		       COUNT(*) FILTER (WHERE pl.winner_participant_id = pr.id) AS won,
		       COUNT(*) FILTER (WHERE pl.winner_participant_id IS NOT NULL AND pl.winner_participant_id <> pr.id) AS lost,
		       COALESCE(sc.sf,0) AS sets_for, COALESCE(sc.sa,0) AS sets_against
		FROM parts pr
		LEFT JOIN played pl ON pl.pid = pr.id
		LEFT JOIN setcounts sc ON sc.pid = pr.id
		GROUP BY pr.id, pr.display_name, pr.seed, sc.sf, sc.sa
		ORDER BY won DESC, (COALESCE(sc.sf,0) - COALESCE(sc.sa,0)) DESC, pr.display_name`
	rows, err := s.pool.Query(ctx, q, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Standing{}
	for rows.Next() {
		var st Standing
		if err := rows.Scan(&st.ParticipantID, &st.DisplayName, &st.Seed,
			&st.Played, &st.Won, &st.Lost, &st.SetsFor, &st.SetsAgainst); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// MaybeResolveGroups checks whether a just-completed match belongs to a
// group-knockout group stage and, if the whole group stage is now complete,
// fills the knockout placeholders. It is a no-op for other formats or while any
// group match is still outstanding. Returns true if it resolved. Safe to call
// after every score; errors are returned so the caller can log but not fail.
func (s *Service) MaybeResolveGroups(ctx context.Context, matchID uuid.UUID) (bool, error) {
	// Is this match part of a group stage? (has a group_id)
	var eventID uuid.UUID
	var groupID *uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT event_id, group_id FROM matches WHERE id = $1`, matchID).Scan(&eventID, &groupID)
	if err != nil || groupID == nil {
		return false, err
	}

	// Are all group-stage matches for this event completed?
	var remaining int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM matches m JOIN stages st ON st.id = m.stage_id AND st.kind = 'group'
		WHERE m.event_id = $1 AND m.status NOT IN ('completed','bye','walkover','retired','cancelled')`,
		eventID).Scan(&remaining); err != nil {
		return false, err
	}
	if remaining > 0 {
		return false, nil
	}

	// All groups done → resolve (nil org = system, already authorized upstream).
	_, err = s.ResolveGroups(ctx, eventID, nil)
	return err == nil, err
}

// ResolveGroups fills the knockout group-placement slots with the actual group
// finishers from current standings. Resolution is TYPED — each slot names its
// (group, rank) directly — so any rank resolves (not just 1-2, the old
// label-matching bug) and organizer-named groups resolve too (labels were
// letter-based and never matched custom names). Idempotent: re-running
// re-resolves from current standings, including overwriting a previously
// resolved slot whose standings changed. The whole resolution is one
// transaction. Returns the number of slots filled.
func (s *Service) ResolveGroups(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID) (int, error) {
	// Authorize via the event's tournament org.
	var ownerOrg uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT t.org_id FROM events e JOIN tournaments t ON t.id = e.tournament_id WHERE e.id = $1`,
		eventID).Scan(&ownerOrg)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if orgID != nil && ownerOrg != *orgID {
		return 0, ErrForbidden
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Every knockout slot that sources a group placement, resolved or not.
	rows, err := tx.Query(ctx, `
		SELECT mp.id, mp.source_group_id, mp.source_rank
		FROM match_participants mp
		JOIN matches m ON m.id = mp.match_id
		JOIN stages st ON st.id = m.stage_id AND st.kind = 'knockout'
		WHERE m.event_id = $1 AND mp.source_type = 'group_placement'`, eventID)
	if err != nil {
		return 0, err
	}
	type slotRef struct {
		slotRowID uuid.UUID
		groupID   uuid.UUID
		rank      int
	}
	var refs []slotRef
	for rows.Next() {
		var r slotRef
		if err := rows.Scan(&r.slotRowID, &r.groupID, &r.rank); err != nil {
			rows.Close()
			return 0, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Standings per referenced group, computed once inside the transaction.
	placements := map[uuid.UUID][]Standing{}
	for _, r := range refs {
		if _, done := placements[r.groupID]; done {
			continue
		}
		st, err := s.standingsForGroup(ctx, tx, r.groupID)
		if err != nil {
			return 0, err
		}
		placements[r.groupID] = st
	}

	filled := 0
	for _, r := range refs {
		st := placements[r.groupID]
		if r.rank > len(st) {
			continue // rank beyond the group's field — leave unresolved
		}
		if _, err := tx.Exec(ctx,
			`UPDATE match_participants SET participant_id = $1 WHERE id = $2`,
			st[r.rank-1].ParticipantID, r.slotRowID); err != nil {
			return 0, err
		}
		filled++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return filled, nil
}

// SetGroupAdvance updates how many top teams advance from one group. Authorized
// via the group's event → tournament → org. advance must be >= 1. Returns the
// group's new advance_count (and event_id for cache-busting the read model).
func (s *Service) SetGroupAdvance(ctx context.Context, groupID uuid.UUID, orgID *uuid.UUID, advance int) (uuid.UUID, int, error) {
	if advance < 1 {
		return uuid.Nil, 0, ErrInvalidPairs
	}
	var ownerOrg, eventID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT t.org_id, e.id
		FROM groups g
		JOIN stages st ON st.id = g.stage_id
		JOIN events e ON e.id = st.event_id
		JOIN tournaments t ON t.id = e.tournament_id
		WHERE g.id = $1`, groupID).Scan(&ownerOrg, &eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, 0, err
	}
	if orgID != nil && ownerOrg != *orgID {
		return uuid.Nil, 0, ErrForbidden
	}
	var newAdvance int
	if err := s.pool.QueryRow(ctx,
		`UPDATE groups SET advance_count = $2 WHERE id = $1 RETURNING advance_count`,
		groupID, advance).Scan(&newAdvance); err != nil {
		return uuid.Nil, 0, err
	}
	return eventID, newAdvance, nil
}

// --- Group-knockout read model ---

type GroupTable struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	AdvanceCount int        `json:"advance_count"`
	Standings    []Standing `json:"standings"`
}

type GroupKnockout struct {
	EventID  uuid.UUID      `json:"event_id"`
	Groups   []GroupTable   `json:"groups"`
	Knockout []BracketRound `json:"knockout"`
}

// GetGroupKnockout returns per-group standings plus the knockout bracket for a
// group_knockout event.
func (s *Service) GetGroupKnockout(ctx context.Context, eventID uuid.UUID) (*GroupKnockout, error) {
	out := &GroupKnockout{EventID: eventID, Groups: []GroupTable{}, Knockout: []BracketRound{}}

	// Groups + their standings (computed per group_id).
	grows, err := s.pool.Query(ctx, `
		SELECT g.id, g.name, g.advance_count
		FROM groups g
		JOIN stages st ON st.id = g.stage_id
		WHERE st.event_id = $1 AND st.kind = 'group'
		ORDER BY g.name`, eventID)
	if err != nil {
		return nil, err
	}
	type grp struct {
		id      uuid.UUID
		name    string
		advance int
	}
	var groups []grp
	for grows.Next() {
		var g grp
		if err := grows.Scan(&g.id, &g.name, &g.advance); err != nil {
			grows.Close()
			return nil, err
		}
		groups = append(groups, g)
	}
	grows.Close()

	for _, g := range groups {
		st, err := s.standingsForGroup(ctx, s.pool, g.id)
		if err != nil {
			return nil, err
		}
		out.Groups = append(out.Groups, GroupTable{ID: g.id, Name: g.name, AdvanceCount: g.advance, Standings: st})
	}

	// Knockout bracket: reuse the bracket builder but scope to knockout-stage
	// matches. We fetch and build a rounds tree from those matches only.
	ko, err := s.knockoutRounds(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out.Knockout = ko
	return out, nil
}

// standingsForGroup computes a standings table for one group's participants.
// Querier is satisfied by both *pgxpool.Pool and pgx.Tx, so draw reads and
// slot rebuilds can run inside a caller's transaction (the score-correction
// flow in internal/match) or standalone.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// LockedSlot identifies a downstream slot whose match has already started, so
// a correction that would change it must be refused rather than applied or
// silently skipped.
type LockedSlot struct {
	MatchID uuid.UUID `json:"match_id"`
	MatchNo int       `json:"match_no"`
	Slot    int       `json:"slot"`
	Status  string    `json:"status"`
}

// RebuildGroupPlacements recomputes the knockout group_placement slots that
// reference one group, inside the caller's transaction.
//
// groupComplete=true resolves each slot to the group's current standings;
// false clears them back to unresolved (the group was re-opened by a
// correction). Slots whose value would NOT change are untouched and never
// count as locked. Slots whose value WOULD change but whose knockout match is
// live/completed/scored are returned as locked — the caller must abort the
// transaction; nothing is silently skipped.
func (s *Service) RebuildGroupPlacements(ctx context.Context, q Querier, groupID uuid.UUID, groupComplete bool) (changed int, locked []LockedSlot, err error) {
	rows, err := q.Query(ctx, `
		SELECT mp.id, mp.source_rank, mp.participant_id,
		       m.id, m.match_no, m.status,
		       EXISTS(SELECT 1 FROM match_scores ms WHERE ms.match_id = m.id) AS has_scores,
		       mp.slot
		FROM match_participants mp
		JOIN matches m ON m.id = mp.match_id
		WHERE mp.source_type = 'group_placement' AND mp.source_group_id = $1
		FOR UPDATE OF m, mp`, groupID)
	if err != nil {
		return 0, nil, err
	}
	type ref struct {
		slotRowID uuid.UUID
		rank      int
		current   *uuid.UUID
		matchID   uuid.UUID
		matchNo   int
		status    string
		hasScores bool
		slot      int
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.slotRowID, &r.rank, &r.current,
			&r.matchID, &r.matchNo, &r.status, &r.hasScores, &r.slot); err != nil {
			rows.Close()
			return 0, nil, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(refs) == 0 {
		return 0, nil, nil
	}

	var placements []Standing
	if groupComplete {
		if placements, err = s.standingsForGroup(ctx, q, groupID); err != nil {
			return 0, nil, err
		}
	}
	desired := func(r ref) *uuid.UUID {
		if !groupComplete || r.rank > len(placements) {
			return nil
		}
		pid := placements[r.rank-1].ParticipantID
		return &pid
	}
	same := func(a, b *uuid.UUID) bool {
		if a == nil || b == nil {
			return a == b
		}
		return *a == *b
	}

	for _, r := range refs {
		want := desired(r)
		if same(r.current, want) {
			continue
		}
		// "bye" is structural (pad match), not a started consumer — stays writable.
		if (r.status != "pending" && r.status != "scheduled" && r.status != "bye") || r.hasScores {
			locked = append(locked, LockedSlot{MatchID: r.matchID, MatchNo: r.matchNo, Slot: r.slot, Status: r.status})
		}
	}
	if len(locked) > 0 {
		return 0, locked, nil
	}
	for _, r := range refs {
		want := desired(r)
		if same(r.current, want) {
			continue
		}
		if _, err := q.Exec(ctx,
			`UPDATE match_participants SET participant_id = $1 WHERE id = $2`, want, r.slotRowID); err != nil {
			return 0, nil, err
		}
		changed++
	}
	return changed, nil, nil
}

func (s *Service) standingsForGroup(ctx context.Context, q Querier, groupID uuid.UUID) ([]Standing, error) {
	const q2 = `
		WITH parts AS (
			SELECT DISTINCT p.id, p.display_name, p.seed
			FROM matches m
			JOIN match_participants mp ON mp.match_id = m.id
			JOIN participants p ON p.id = mp.participant_id
			WHERE m.group_id = $1
		),
		played AS (
			SELECT mp.participant_id AS pid, m.id AS match_id, m.winner_participant_id, mp.slot
			FROM matches m
			JOIN match_participants mp ON mp.match_id = m.id
			WHERE m.group_id = $1 AND m.status IN ('completed','walkover','retired') AND mp.participant_id IS NOT NULL
		),
		setcounts AS (
			SELECT pl.pid,
			       COALESCE(SUM(CASE WHEN pl.slot=1 THEN ms.p1_games ELSE ms.p2_games END),0) AS sf,
			       COALESCE(SUM(CASE WHEN pl.slot=1 THEN ms.p2_games ELSE ms.p1_games END),0) AS sa
			FROM played pl LEFT JOIN match_scores ms ON ms.match_id = pl.match_id
			GROUP BY pl.pid
		)
		SELECT pr.id, pr.display_name, pr.seed,
		       COUNT(pl.match_id) AS played,
		       COUNT(*) FILTER (WHERE pl.winner_participant_id = pr.id) AS won,
		       COUNT(*) FILTER (WHERE pl.winner_participant_id IS NOT NULL AND pl.winner_participant_id <> pr.id) AS lost,
		       COALESCE(sc.sf,0), COALESCE(sc.sa,0)
		FROM parts pr
		LEFT JOIN played pl ON pl.pid = pr.id
		LEFT JOIN setcounts sc ON sc.pid = pr.id
		GROUP BY pr.id, pr.display_name, pr.seed, sc.sf, sc.sa
		ORDER BY won DESC, (COALESCE(sc.sf,0)-COALESCE(sc.sa,0)) DESC, pr.display_name`
	rows, err := q.Query(ctx, q2, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Standing{}
	for rows.Next() {
		var st Standing
		if err := rows.Scan(&st.ParticipantID, &st.DisplayName, &st.Seed,
			&st.Played, &st.Won, &st.Lost, &st.SetsFor, &st.SetsAgainst); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// knockoutRounds builds the rounds tree for an event's knockout-stage matches,
// including source labels for unresolved feeds.
func (s *Service) knockoutRounds(ctx context.Context, eventID uuid.UUID) ([]BracketRound, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.match_no, m.status, m.next_match_id, m.winner_participant_id,
		       mp.slot, mp.participant_id, p.display_name, p.seed, mp.source->>'label'
		FROM matches m
		JOIN stages st ON st.id = m.stage_id AND st.kind = 'knockout'
		LEFT JOIN match_participants mp ON mp.match_id = m.id
		LEFT JOIN participants p ON p.id = mp.participant_id
		WHERE m.event_id = $1
		ORDER BY m.match_no, mp.slot`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type mrec struct {
		matchNo int
		status  string
		next    *uuid.UUID
		winner  *uuid.UUID
		slots   []BracketSlot
	}
	order := []uuid.UUID{}
	byID := map[uuid.UUID]*mrec{}
	for rows.Next() {
		var mid uuid.UUID
		var matchNo int
		var status string
		var next, winner, pid *uuid.UUID
		var slot *int
		var name, label *string
		var seed *int
		if err := rows.Scan(&mid, &matchNo, &status, &next, &winner, &slot, &pid, &name, &seed, &label); err != nil {
			return nil, err
		}
		rec, ok := byID[mid]
		if !ok {
			rec = &mrec{matchNo: matchNo, status: status, next: next, winner: winner}
			byID[mid] = rec
			order = append(order, mid)
		}
		if slot != nil {
			rec.slots = append(rec.slots, BracketSlot{Slot: *slot, ParticipantID: pid, DisplayName: name, Seed: seed, SourceLabel: label})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return []BracketRound{}, nil
	}

	// Round depth from the final (next == nil).
	roundOf := map[uuid.UUID]int{}
	var maxHops int
	var hops func(id uuid.UUID) int
	hops = func(id uuid.UUID) int {
		if v, ok := roundOf[id]; ok {
			return v
		}
		rec := byID[id]
		if rec == nil || rec.next == nil {
			roundOf[id] = 0
			return 0
		}
		v := hops(*rec.next) + 1
		roundOf[id] = v
		return v
	}
	for _, id := range order {
		if h := hops(id); h > maxHops {
			maxHops = h
		}
	}
	name := func(fromFinal int) string {
		switch fromFinal {
		case 0:
			return "Final"
		case 1:
			return "Semifinal"
		case 2:
			return "Quarterfinal"
		default:
			return fmt.Sprintf("Round of %d", 1<<(fromFinal+1))
		}
	}
	roundsMap := map[int][]BracketMatch{}
	for _, id := range order {
		rec := byID[id]
		rn := maxHops - roundOf[id] + 1
		roundsMap[rn] = append(roundsMap[rn], BracketMatch{
			ID: id, MatchNo: rec.matchNo, Status: rec.status,
			WinnerParticipantID: rec.winner, Participants: rec.slots,
		})
	}
	var out []BracketRound
	for rn := 1; rn <= maxHops+1; rn++ {
		out = append(out, BracketRound{RoundNumber: rn, Name: name(maxHops - rn + 1), Matches: roundMatches(roundsMap, rn)})
	}
	return out, nil
}

// insertSlot persists one slot with its TYPED source. groupIDs maps a
// generator group index to the just-created groups row (nil outside
// group-knockout). Winner-feed typing happens afterwards in persist's pass 3,
// once every match row exists — here a feed-to-be is written as 'empty'.
func insertSlot(ctx context.Context, tx pgx.Tx, matchID uuid.UUID, slot int, s Slot, groupIDs map[int]uuid.UUID) error {
	var pid *uuid.UUID
	if s.ParticipantID != nil {
		pid = s.ParticipantID
	}
	// The display label stays in source jsonb; it is never used for resolution.
	var source *string
	if s.SourceLabel != "" {
		j := `{"label":` + jsonString(s.SourceLabel) + `}`
		source = &j
	}

	sourceType := "empty"
	var groupID *uuid.UUID
	var rank *int
	switch {
	case s.ParticipantID != nil:
		sourceType = "fixed"
	case s.IsBye:
		sourceType = "bye"
	case s.SourceLabel != "":
		sourceType = "group_placement"
		gid, ok := groupIDs[s.SourceGroupIdx]
		if !ok {
			return fmt.Errorf("slot references unknown group index %d", s.SourceGroupIdx)
		}
		groupID = &gid
		r := s.SourceRank
		rank = &r
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO match_participants
			(match_id, participant_id, slot, source, source_type, source_group_id, source_rank)
		VALUES ($1, $2, $3, $4::jsonb, $5::slot_source, $6, $7)`,
		matchID, pid, slot, source, sourceType, groupID, rank)
	return err
}

// jsonString quotes a string for embedding in a small JSON literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

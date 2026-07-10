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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound        = errors.New("event not found")
	ErrForbidden       = errors.New("not permitted")
	ErrNotEnough       = errors.New("need at least 2 participants")
	ErrUnsupportedForm = errors.New("format not yet supported")
	ErrInvalidPairs    = errors.New("invalid manual pairings")
)

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

type participantRow struct {
	id   uuid.UUID
	name string
	seed *int
}

// Generate builds and persists the draw for an event. It is idempotent-ish:
// regenerating deletes the event's existing matches first (a fresh draw).
func (s *Service) Generate(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID) (int, error) {
	// Authorize + read the event's format.
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

	// Load participants (seeded first via the query order isn't required; the
	// generator handles seeding).
	rows, err := s.pool.Query(ctx,
		`SELECT id, display_name, seed FROM participants WHERE event_id = $1`, eventID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ps []participantRow
	for rows.Next() {
		var p participantRow
		if err := rows.Scan(&p.id, &p.name, &p.seed); err != nil {
			return 0, err
		}
		ps = append(ps, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ps) < 2 {
		return 0, ErrNotEnough
	}

	entries := make([]Entry, len(ps))
	for i, p := range ps {
		seed := 0
		if p.seed != nil {
			seed = *p.seed
		}
		entries[i] = Entry{ParticipantID: p.id, Seed: seed}
	}

	// Pick the generator by format.
	var matches []Match
	switch format {
	case "single_elim":
		matches = GenerateSingleElimination(entries)
	case "round_robin":
		matches = GenerateRoundRobin(entries)
	case "group_knockout":
		matches = GenerateGroupKnockout(entries, defaultGroupConfig(len(entries)))
	default:
		return 0, ErrUnsupportedForm
	}
	if len(matches) == 0 {
		return 0, ErrNotEnough
	}

	return len(matches), s.persist(ctx, eventID, matches)
}

// BuildInput is a Match-builder request. Mode is "auto" or "manual". For manual,
// Pairs are the organizer's explicit round-1 matchups (Pair, nil side = bye).
// For auto, Pairs is ignored and random pairs are generated.
type BuildInput struct {
	Mode  string
	Pairs []Pair
}

// Build creates and persists a single-elimination bracket from the Match
// builder. In "auto" mode it shuffles the event's participants into random pairs
// (seeding ignored); in "manual" mode it validates and uses the supplied
// round-1 pairs verbatim. It also records the chosen pairing_mode on the event.
// Only single-elimination events are supported by the builder for M1.
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

	var matches []Match
	mode := in.Mode
	switch mode {
	case "manual":
		pairs, err := validatePairs(in.Pairs, valid)
		if err != nil {
			return 0, err
		}
		matches = GenerateFromPairs(pairs)
	case "auto", "":
		mode = "auto"
		if len(valid) < 2 {
			return 0, ErrNotEnough
		}
		// Auto pairs = the seeded generator with every seed 0, so placeEntries
		// fills slots in participant order. Deterministic here; the frontend's
		// "auto-fill" already offers a shuffle when the organizer wants one.
		entries := make([]Entry, 0, len(valid))
		for id := range valid {
			entries = append(entries, Entry{ParticipantID: id})
		}
		matches = GenerateSingleElimination(entries)
	default:
		return 0, ErrInvalidPairs
	}
	if len(matches) == 0 {
		return 0, ErrNotEnough
	}

	if err := s.persist(ctx, eventID, matches); err != nil {
		return 0, err
	}
	// Record how this draw was built (a future re-open of the builder reads it).
	if _, err := s.pool.Exec(ctx,
		`UPDATE events SET pairing_mode = $1::pairing_mode WHERE id = $2`, mode, eventID); err != nil {
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

// defaultGroupConfig picks a sensible groups/advance split from the entry
// count: groups of ~4, top 2 advance, giving a power-of-two knockout.
func defaultGroupConfig(n int) GroupKnockoutConfig {
	groups := 2
	switch {
	case n >= 16:
		groups = 4
	case n >= 8:
		groups = 2
	}
	return GroupKnockoutConfig{Groups: groups, Advance: 2}
}

// persist writes the generated matches and their progression + slot entries in
// a single transaction. It maps the generator's flat indices to DB uuids, then
// wires next_match_id in a second pass. When any match carries a GroupIndex >= 0
// (group-knockout), it also creates a group stage + group rows and a knockout
// stage, stamping matches.stage_id / group_id.
func (s *Service) persist(ctx context.Context, eventID uuid.UUID, matches []Match) error {
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
			var gid uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO groups (stage_id, name) VALUES ($1,$2) RETURNING id`,
				groupStageID, name).Scan(&gid); err != nil {
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
		if err := insertSlot(ctx, tx, ids[i], 1, m.Slot1); err != nil {
			return err
		}
		if err := insertSlot(ctx, tx, ids[i], 2, m.Slot2); err != nil {
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

// BracketSet is one set's games for both sides, for compact score display.
type BracketSet struct {
	P1 int `json:"p1"`
	P2 int `json:"p2"`
}

type BracketMatch struct {
	ID                  uuid.UUID     `json:"id"`
	MatchNo             int           `json:"match_no"`
	Status              string        `json:"status"`
	WinnerParticipantID *uuid.UUID    `json:"winner_participant_id"`
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
		       mp.slot, mp.participant_id, p.display_name, p.seed
		FROM matches m
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
		sets    []BracketSet
	}
	order := []uuid.UUID{}
	byID := map[uuid.UUID]*mrec{}
	for rows.Next() {
		var mid uuid.UUID
		var matchNo int
		var status string
		var next, winner *uuid.UUID
		var slot *int
		var pid *uuid.UUID
		var name *string
		var seed *int
		if err := rows.Scan(&mid, &matchNo, &status, &next, &winner, &slot, &pid, &name, &seed); err != nil {
			return nil, err
		}
		rec, ok := byID[mid]
		if !ok {
			rec = &mrec{matchNo: matchNo, status: status, next: next, winner: winner}
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
		SELECT ms.match_id, ms.p1_games, ms.p2_games
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
		if err := srows.Scan(&mid, &p1, &p2); err != nil {
			return nil, err
		}
		if rec, ok := byID[mid]; ok {
			rec.sets = append(rec.sets, BracketSet{P1: p1, P2: p2})
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
			WinnerParticipantID: rec.winner, Participants: rec.slots, Sets: rec.sets,
		})
	}

	b := &Bracket{EventID: eventID, Format: format}
	for rn := 1; rn <= maxHops+1; rn++ {
		fromFinal := maxHops - rn + 1
		b.Rounds = append(b.Rounds, BracketRound{
			RoundNumber: rn, Name: roundName(fromFinal), Matches: roundsMap[rn],
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
			WHERE m.event_id = $1 AND m.status = 'completed' AND mp.participant_id IS NOT NULL
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
		WHERE m.event_id = $1 AND m.status NOT IN ('completed','bye','walkover')`,
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

// ResolveGroups fills the knockout placeholders (Winner/Runner-up Group X) with
// the actual group finishers, once the group stage is complete. It is
// idempotent: re-running re-resolves from current standings. Returns the number
// of slots filled. Authorization is the caller's responsibility.
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

	// Build a map: label ("Winner Group A") → participant id, from standings.
	grows, err := s.pool.Query(ctx, `
		SELECT g.id, g.name
		FROM groups g JOIN stages st ON st.id = g.stage_id
		WHERE st.event_id = $1 AND st.kind = 'group'`, eventID)
	if err != nil {
		return 0, err
	}
	type grp struct {
		id   uuid.UUID
		name string
	}
	var groups []grp
	for grows.Next() {
		var g grp
		if err := grows.Scan(&g.id, &g.name); err != nil {
			grows.Close()
			return 0, err
		}
		groups = append(groups, g)
	}
	grows.Close()

	labelToPID := map[string]uuid.UUID{}
	for _, g := range groups {
		st, err := s.standingsForGroup(ctx, g.id)
		if err != nil {
			return 0, err
		}
		// g.name is like "Group A"; labels are "Winner Group A", "Runner-up Group A".
		if len(st) >= 1 {
			labelToPID["Winner "+g.name] = st[0].ParticipantID
		}
		if len(st) >= 2 {
			labelToPID["Runner-up "+g.name] = st[1].ParticipantID
		}
	}

	// For each knockout match slot with a source label, fill the participant.
	rows, err := s.pool.Query(ctx, `
		SELECT mp.id, mp.source->>'label'
		FROM match_participants mp
		JOIN matches m ON m.id = mp.match_id
		JOIN stages st ON st.id = m.stage_id AND st.kind = 'knockout'
		WHERE m.event_id = $1 AND mp.source->>'label' IS NOT NULL`, eventID)
	if err != nil {
		return 0, err
	}
	type upd struct {
		slotRowID uuid.UUID
		pid       uuid.UUID
	}
	var updates []upd
	for rows.Next() {
		var slotRowID uuid.UUID
		var label string
		if err := rows.Scan(&slotRowID, &label); err != nil {
			rows.Close()
			return 0, err
		}
		if pid, ok := labelToPID[label]; ok {
			updates = append(updates, upd{slotRowID, pid})
		}
	}
	rows.Close()

	for _, u := range updates {
		if _, err := s.pool.Exec(ctx,
			`UPDATE match_participants SET participant_id = $1 WHERE id = $2`, u.pid, u.slotRowID); err != nil {
			return 0, err
		}
	}
	return len(updates), nil
}

// --- Group-knockout read model ---

type GroupTable struct {
	Name      string     `json:"name"`
	Standings []Standing `json:"standings"`
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
		SELECT g.id, g.name
		FROM groups g
		JOIN stages st ON st.id = g.stage_id
		WHERE st.event_id = $1 AND st.kind = 'group'
		ORDER BY g.name`, eventID)
	if err != nil {
		return nil, err
	}
	type grp struct {
		id   uuid.UUID
		name string
	}
	var groups []grp
	for grows.Next() {
		var g grp
		if err := grows.Scan(&g.id, &g.name); err != nil {
			grows.Close()
			return nil, err
		}
		groups = append(groups, g)
	}
	grows.Close()

	for _, g := range groups {
		st, err := s.standingsForGroup(ctx, g.id)
		if err != nil {
			return nil, err
		}
		out.Groups = append(out.Groups, GroupTable{Name: g.name, Standings: st})
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
func (s *Service) standingsForGroup(ctx context.Context, groupID uuid.UUID) ([]Standing, error) {
	const q = `
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
			WHERE m.group_id = $1 AND m.status = 'completed' AND mp.participant_id IS NOT NULL
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
	rows, err := s.pool.Query(ctx, q, groupID)
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
		out = append(out, BracketRound{RoundNumber: rn, Name: name(maxHops - rn + 1), Matches: roundsMap[rn]})
	}
	return out, nil
}

func insertSlot(ctx context.Context, tx pgx.Tx, matchID uuid.UUID, slot int, s Slot) error {
	// A bye or a fed (winner-of / group-position) slot has no participant yet.
	var pid *uuid.UUID
	if s.ParticipantID != nil {
		pid = s.ParticipantID
	}
	// A group-knockout placeholder carries a display label in source jsonb.
	var source *string
	if s.SourceLabel != "" {
		j := `{"label":` + jsonString(s.SourceLabel) + `}`
		source = &j
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO match_participants (match_id, participant_id, slot, source)
		VALUES ($1, $2, $3, $4::jsonb)`, matchID, pid, slot, source)
	return err
}

// jsonString quotes a string for embedding in a small JSON literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

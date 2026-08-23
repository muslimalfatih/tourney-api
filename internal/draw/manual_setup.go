package draw

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/muslimalfatih/tourney-api/internal/audit"
)

var ErrBadRequest = errors.New("invalid request")

// DuplicateFixtureError reports that the requested pairing already has a
// fixture in this event. Rematchable says whether allow_rematch would let the
// create through: false while the existing fixture is still unplayed
// (pending/scheduled/live — a second unplayed fixture for the same pair is
// never legitimate), true when it was decided (completed/walkover/retired).
type DuplicateFixtureError struct {
	MatchID     uuid.UUID
	MatchNo     int
	Status      string
	Rematchable bool
}

func (e *DuplicateFixtureError) Error() string {
	return fmt.Sprintf("pair already has fixture %d (%s)", e.MatchNo, e.Status)
}

// eventAuth is what authorizeEvent resolves for the manual-setup ops.
type eventAuth struct {
	orgID        uuid.UUID // owning org (for audit)
	tournamentID uuid.UUID
	format       string
}

// authorizeEvent verifies the caller's org owns the event's tournament (nil =
// super admin) and returns its context. Shared by the manual-setup ops.
func (s *Service) authorizeEvent(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID) (eventAuth, error) {
	var a eventAuth
	err := s.pool.QueryRow(ctx, `
		SELECT t.org_id, t.id, e.format
		FROM events e JOIN tournaments t ON t.id = e.tournament_id
		WHERE e.id = $1`, eventID).Scan(&a.orgID, &a.tournamentID, &a.format)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if orgID != nil && a.orgID != *orgID {
		return a, ErrForbidden
	}
	return a, nil
}

// dupRow is one existing fixture for a normalized pair (see findDuplicates).
type dupRow struct {
	id      uuid.UUID
	matchNo int
	status  string
}

// classifyDuplicate decides what an existing-fixture set means for a create:
// returns the error to surface (nil = create may proceed) and whether the
// create is a rematch that must be audited. Byes and cancelled fixtures never
// appear in rows (excluded by the query): a cancelled fixture was voided, so
// re-creating its pairing is the normal recovery path, not a rematch.
func classifyDuplicate(rows []dupRow, allowRematch bool) (*DuplicateFixtureError, bool) {
	var decided *dupRow
	for i := range rows {
		r := rows[i]
		switch r.status {
		case "pending", "scheduled", "live":
			// An unplayed fixture for this pair already exists — hard block,
			// override cannot help.
			return &DuplicateFixtureError{MatchID: r.id, MatchNo: r.matchNo, Status: r.status}, false
		case "completed", "walkover", "retired":
			if decided == nil {
				decided = &r
			}
		}
	}
	if decided == nil {
		return nil, false
	}
	if !allowRematch {
		return &DuplicateFixtureError{
			MatchID: decided.id, MatchNo: decided.matchNo, Status: decided.status, Rematchable: true,
		}, false
	}
	return nil, true
}

// findDuplicates returns the existing fixtures for the UNORDERED pair (a, b)
// in this event (ungrouped scope — manual RR fixtures carry no group), newest
// first. Cancelled and bye fixtures are excluded by definition.
func findDuplicates(ctx context.Context, tx pgx.Tx, eventID, a, b uuid.UUID) ([]dupRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.id, m.match_no, m.status
		FROM matches m
		JOIN match_participants p1 ON p1.match_id = m.id AND p1.slot = 1
		JOIN match_participants p2 ON p2.match_id = m.id AND p2.slot = 2
		WHERE m.event_id = $1 AND m.group_id IS NULL
		  AND m.status NOT IN ('cancelled', 'bye')
		  AND least(p1.participant_id, p2.participant_id) = least($2::uuid, $3::uuid)
		  AND greatest(p1.participant_id, p2.participant_id) = greatest($2::uuid, $3::uuid)
		ORDER BY m.match_no DESC`, eventID, a, b)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dupRow
	for rows.Next() {
		var r dupRow
		if err := rows.Scan(&r.id, &r.matchNo, &r.status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lockEventFixtures serializes fixture writes per event for the transaction's
// lifetime, so two concurrent creates (or a create racing the generator) can't
// both pass the duplicate check. Deliberately a lock, not a DB uniqueness
// constraint — legitimate rematches make the pairing non-unique by design.
func lockEventFixtures(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, eventID.String())
	return err
}

// validParticipants returns the set of participant ids that belong to the event.
func (s *Service) validParticipants(ctx context.Context, eventID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM participants WHERE event_id = $1`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	valid := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		valid[id] = true
	}
	return valid, rows.Err()
}

// AddManualMatch inserts a single organizer-created match (round_robin manual
// setup — the organizer builds each fixture by hand). Both participants must
// belong to the event and differ. The pair is treated as UNORDERED: an
// existing non-cancelled fixture for the same two participants blocks the
// create with DuplicateFixtureError — decided fixtures can be re-created with
// allowRematch (audited), unplayed ones never. Returns id + match_no.
func (s *Service) AddManualMatch(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID, actor, teamA, teamB uuid.UUID, allowRematch bool) (uuid.UUID, int, error) {
	auth, err := s.authorizeEvent(ctx, eventID, orgID)
	if err != nil {
		return uuid.Nil, 0, err
	}
	if auth.format != "round_robin" {
		return uuid.Nil, 0, ErrUnsupportedForm
	}
	if teamA == teamB {
		return uuid.Nil, 0, ErrBadRequest
	}
	valid, err := s.validParticipants(ctx, eventID)
	if err != nil {
		return uuid.Nil, 0, err
	}
	if !valid[teamA] || !valid[teamB] {
		return uuid.Nil, 0, ErrBadRequest
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockEventFixtures(ctx, tx, eventID); err != nil {
		return uuid.Nil, 0, err
	}

	dups, err := findDuplicates(ctx, tx, eventID, teamA, teamB)
	if err != nil {
		return uuid.Nil, 0, err
	}
	dupErr, isRematch := classifyDuplicate(dups, allowRematch)
	if dupErr != nil {
		return uuid.Nil, 0, dupErr
	}

	var nextNo int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(match_no), 0) + 1 FROM matches WHERE event_id = $1`, eventID).Scan(&nextNo); err != nil {
		return uuid.Nil, 0, err
	}

	var matchID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO matches (event_id, match_no, status) VALUES ($1, $2, 'pending') RETURNING id`,
		eventID, nextNo).Scan(&matchID); err != nil {
		return uuid.Nil, 0, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO match_participants (match_id, participant_id, slot, source_type)
		 VALUES ($1,$2,1,'fixed'),($1,$3,2,'fixed')`,
		matchID, teamA, teamB); err != nil {
		return uuid.Nil, 0, err
	}

	// An explicit rematch is an accountability event: record which decided
	// fixture it re-runs and who authorized it.
	if isRematch {
		prior := dups[0]
		if err := s.audit.RecordTx(ctx, tx, audit.Entry{
			ActorUserID:  actor,
			OrgID:        &auth.orgID,
			TournamentID: &auth.tournamentID,
			Action:       "match.rematch_override",
			TargetType:   "match",
			TargetID:     matchID.String(),
			Diff: map[string]any{
				"prior_match_id": prior.id.String(),
				"prior_match_no": prior.matchNo,
				"prior_status":   prior.status,
				"participants":   []string{teamA.String(), teamB.String()},
			},
		}); err != nil {
			return uuid.Nil, 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, 0, err
	}
	return matchID, nextNo, nil
}

// GenerateRoundRobinFixtures creates every missing pairing of a round_robin
// event's participants — idempotent by construction: existing non-cancelled
// fixtures for a pair (any status, played or not) satisfy that pairing, so
// repeated generation creates zero duplicates. Returns (created, existing).
func (s *Service) GenerateRoundRobinFixtures(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID) (int, int, error) {
	auth, err := s.authorizeEvent(ctx, eventID, orgID)
	if err != nil {
		return 0, 0, err
	}
	if auth.format != "round_robin" {
		return 0, 0, ErrUnsupportedForm
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockEventFixtures(ctx, tx, eventID); err != nil {
		return 0, 0, err
	}

	// Deterministic participant order (seed, then id) so repeated runs number
	// missing fixtures the same way.
	pRows, err := tx.Query(ctx,
		`SELECT id FROM participants WHERE event_id = $1 ORDER BY seed NULLS LAST, id`, eventID)
	if err != nil {
		return 0, 0, err
	}
	var pids []uuid.UUID
	for pRows.Next() {
		var id uuid.UUID
		if err := pRows.Scan(&id); err != nil {
			pRows.Close()
			return 0, 0, err
		}
		pids = append(pids, id)
	}
	pRows.Close()
	if err := pRows.Err(); err != nil {
		return 0, 0, err
	}
	if len(pids) < 2 {
		return 0, 0, ErrBadRequest
	}

	// Existing pairings (normalized, cancelled excluded — a voided fixture
	// doesn't satisfy the pairing).
	eRows, err := tx.Query(ctx, `
		SELECT least(p1.participant_id, p2.participant_id),
		       greatest(p1.participant_id, p2.participant_id)
		FROM matches m
		JOIN match_participants p1 ON p1.match_id = m.id AND p1.slot = 1
		JOIN match_participants p2 ON p2.match_id = m.id AND p2.slot = 2
		WHERE m.event_id = $1 AND m.group_id IS NULL
		  AND m.status NOT IN ('cancelled', 'bye')
		  AND p1.participant_id IS NOT NULL AND p2.participant_id IS NOT NULL`, eventID)
	if err != nil {
		return 0, 0, err
	}
	have := map[string]bool{}
	for eRows.Next() {
		var lo, hi uuid.UUID
		if err := eRows.Scan(&lo, &hi); err != nil {
			eRows.Close()
			return 0, 0, err
		}
		have[lo.String()+"|"+hi.String()] = true
	}
	eRows.Close()
	if err := eRows.Err(); err != nil {
		return 0, 0, err
	}

	var nextNo int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(match_no), 0) + 1 FROM matches WHERE event_id = $1`, eventID).Scan(&nextNo); err != nil {
		return 0, 0, err
	}

	created := 0
	for i := 0; i < len(pids); i++ {
		for j := i + 1; j < len(pids); j++ {
			lo, hi := pids[i], pids[j]
			if hi.String() < lo.String() {
				lo, hi = hi, lo
			}
			if have[lo.String()+"|"+hi.String()] {
				continue
			}
			var matchID uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO matches (event_id, match_no, status) VALUES ($1, $2, 'pending') RETURNING id`,
				eventID, nextNo).Scan(&matchID); err != nil {
				return 0, 0, err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO match_participants (match_id, participant_id, slot, source_type)
				 VALUES ($1,$2,1,'fixed'),($1,$3,2,'fixed')`,
				matchID, pids[i], pids[j]); err != nil {
				return 0, 0, err
			}
			nextNo++
			created++
		}
	}
	return created, len(have), tx.Commit(ctx)
}

// DeleteManualMatch removes a single match the organizer created (round_robin).
// Only allowed while the match is still pending so a played fixture can't be
// silently dropped.
func (s *Service) DeleteManualMatch(ctx context.Context, matchID uuid.UUID, orgID *uuid.UUID) error {
	var ownerOrg uuid.UUID
	var status, format string
	err := s.pool.QueryRow(ctx, `
		SELECT t.org_id, m.status, e.format
		FROM matches m
		JOIN events e ON e.id = m.event_id
		JOIN tournaments t ON t.id = e.tournament_id
		WHERE m.id = $1`, matchID).Scan(&ownerOrg, &status, &format)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if orgID != nil && ownerOrg != *orgID {
		return ErrForbidden
	}
	if format != "round_robin" {
		return ErrUnsupportedForm
	}
	if status != "pending" {
		return ErrBadRequest // a started/completed fixture can't be deleted
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM matches WHERE id = $1`, matchID)
	return err
}

// GroupSpec is one organizer-defined group: display name, advance count, and the
// participant ids assigned to it. Used by SetGroups (manual group_knockout).
type GroupSpec struct {
	Name         string      `json:"name"`
	AdvanceCount int         `json:"advance_count"`
	TeamIDs      []uuid.UUID `json:"team_ids"`
}

// SetGroups replaces a group_knockout event's structure with the organizer's
// manual assignment, then builds the fixtures deterministically: a round-robin
// within each group + a knockout skeleton fed by the group finishers (resolved
// later by ResolveGroups). No randomness — the organizer's assignment IS the
// draw. Validates: every team belongs to the event, a team is in at most one
// group, each group has >= 2 teams and advance_count in [1, group size], and at
// least two teams advance overall.
func (s *Service) SetGroups(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID, specs []GroupSpec) (int, error) {
	auth, err := s.authorizeEvent(ctx, eventID, orgID)
	if err != nil {
		return 0, err
	}
	if auth.format != "group_knockout" {
		return 0, ErrUnsupportedForm
	}
	if len(specs) < 1 {
		return 0, ErrBadRequest
	}
	valid, err := s.validParticipants(ctx, eventID)
	if err != nil {
		return 0, err
	}

	seen := map[uuid.UUID]bool{}
	totalAdvance := 0
	for _, g := range specs {
		if len(g.TeamIDs) < 2 || g.AdvanceCount < 1 || g.AdvanceCount > len(g.TeamIDs) {
			return 0, ErrBadRequest
		}
		for _, id := range g.TeamIDs {
			if !valid[id] || seen[id] {
				return 0, ErrBadRequest
			}
			seen[id] = true
		}
		totalAdvance += g.AdvanceCount
	}
	if totalAdvance < 2 {
		return 0, ErrBadRequest
	}

	// Build the full match set (group RR + knockout skeleton) from the explicit
	// assignment, then reuse the shared persister.
	matches := groupKnockoutFromAssignment(specs)
	if len(matches) == 0 {
		return 0, ErrBadRequest
	}
	names := make([]string, len(specs))
	advance := make([]int, len(specs))
	for i, g := range specs {
		names[i] = g.Name
		advance[i] = g.AdvanceCount
	}
	if err := s.persistWithGroups(ctx, eventID, matches, names, advance); err != nil {
		return 0, err
	}
	return len(matches), nil
}

// groupKnockoutFromAssignment produces the flat match set for a manually-assigned
// group-knockout draw: a round-robin inside each group (GroupIndex = group
// index) followed by a source-labelled knockout skeleton (GroupIndex = -1),
// mirroring GenerateGroupKnockout but with the groups given explicitly.
func groupKnockoutFromAssignment(specs []GroupSpec) []Match {
	var matches []Match

	// Round-robin within each group. Group entries are the assigned teams in the
	// order given (no seeding — assignment is the draw).
	for gi, g := range specs {
		entries := make([]Entry, len(g.TeamIDs))
		for i, id := range g.TeamIDs {
			id := id
			entries[i] = Entry{ParticipantID: id}
		}
		for _, m := range GenerateRoundRobin(entries) {
			m.GroupIndex = gi
			m.NextMatchIdx = -1
			matches = append(matches, m)
		}
	}

	// Knockout skeleton over the advancers (labelled feeds), wired with absolute
	// indices offset past the group matches.
	advance := make([]int, len(specs))
	for i, g := range specs {
		advance[i] = g.AdvanceCount
	}
	koStart := len(matches)
	ko := buildKnockoutFromSlots(advancerSlots(specs))
	for i := range ko {
		if ko[i].NextMatchIdx >= 0 {
			ko[i].NextMatchIdx += koStart
		}
		ko[i].GroupIndex = -1
	}
	return append(matches, ko...)
}

// advancerSlots builds the ordered, TYPED knockout-entry slots for a manual
// assignment, cross-pairing group winners with other groups' runners-up (same
// scheme as the auto path, generalised to per-group advance counts).
//
// Each slot carries (group index, rank) as machine truth plus a display label
// built from the group's ACTUAL name — the legacy version labelled by
// synthetic letters ("Group A") while resolution matched against real group
// names, so a group named anything else silently never resolved. With typed
// slots the label is cosmetic and that whole failure class is gone.
func advancerSlots(specs []GroupSpec) []Slot {
	pos := func(p int) string {
		switch p {
		case 0:
			return "Winner"
		case 1:
			return "Runner-up"
		default:
			return fmt.Sprintf("#%d", p+1)
		}
	}
	placement := func(g, rank int) Slot {
		return Slot{
			SourceLabel:    pos(rank-1) + " " + specs[g].Name,
			SourceGroupIdx: g,
			SourceRank:     rank,
		}
	}
	var slots []Slot
	n := len(specs)
	for g := 0; g < n; g++ {
		slots = append(slots, placement(g, 1))
		if specs[g].AdvanceCount > 1 {
			other := (g + 1) % n
			slots = append(slots, placement(other, 2))
		}
		for p := 2; p < specs[g].AdvanceCount; p++ {
			slots = append(slots, placement(g, p+1))
		}
	}
	return slots
}

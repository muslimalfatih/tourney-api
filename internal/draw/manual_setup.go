package draw

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrBadRequest = errors.New("invalid request")

// authorizeEvent verifies the caller's org owns the event's tournament (nil =
// super admin) and returns the event's format. Shared by the manual-setup ops.
func (s *Service) authorizeEvent(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID) (string, error) {
	var ownerOrg uuid.UUID
	var format string
	err := s.pool.QueryRow(ctx, `
		SELECT t.org_id, e.format
		FROM events e JOIN tournaments t ON t.id = e.tournament_id
		WHERE e.id = $1`, eventID).Scan(&ownerOrg, &format)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if orgID != nil && ownerOrg != *orgID {
		return "", ErrForbidden
	}
	return format, nil
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
// belong to the event and differ. Returns the new match id + its match_no.
func (s *Service) AddManualMatch(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID, teamA, teamB uuid.UUID) (uuid.UUID, int, error) {
	format, err := s.authorizeEvent(ctx, eventID, orgID)
	if err != nil {
		return uuid.Nil, 0, err
	}
	if format != "round_robin" {
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
		`INSERT INTO match_participants (match_id, participant_id, slot) VALUES ($1,$2,1),($1,$3,2)`,
		matchID, teamA, teamB); err != nil {
		return uuid.Nil, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, 0, err
	}
	return matchID, nextNo, nil
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
	format, err := s.authorizeEvent(ctx, eventID, orgID)
	if err != nil {
		return 0, err
	}
	if format != "group_knockout" {
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
	labels := advancerLabels(specs)
	koStart := len(matches)
	ko := buildKnockoutFromLabels(labels)
	for i := range ko {
		if ko[i].NextMatchIdx >= 0 {
			ko[i].NextMatchIdx += koStart
		}
		ko[i].GroupIndex = -1
	}
	return append(matches, ko...)
}

// advancerLabels builds the ordered knockout-entry labels for a manual
// assignment, cross-pairing group winners with other groups' runners-up (same
// scheme as the auto path, generalised to per-group advance counts).
func advancerLabels(specs []GroupSpec) []string {
	letter := func(i int) string { return string(rune('A' + i)) }
	pos := func(p int) string {
		switch p {
		case 0:
			return "Winner"
		case 1:
			return "Runner-up"
		default:
			return "#" + string(rune('0'+p+1))
		}
	}
	var labels []string
	n := len(specs)
	for g := 0; g < n; g++ {
		labels = append(labels, "Winner Group "+letter(g))
		if specs[g].AdvanceCount > 1 {
			other := (g + 1) % n
			labels = append(labels, pos(1)+" Group "+letter(other))
		}
		for p := 2; p < specs[g].AdvanceCount; p++ {
			labels = append(labels, pos(p)+" Group "+letter(g))
		}
	}
	return labels
}

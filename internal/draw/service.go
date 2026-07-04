// Package draw orchestrates draw generation: it loads an event's participants,
// runs the pure format generator (single_elim.go et al.), and persists the
// resulting matches + progression graph. The generators stay HTTP/DB-free; this
// service is the only part that touches the database.
package draw

import (
	"context"
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

	// Only single elimination is wired in M1's first draw slice.
	if format != "single_elim" {
		return 0, ErrUnsupportedForm
	}

	entries := make([]Entry, len(ps))
	for i, p := range ps {
		seed := 0
		if p.seed != nil {
			seed = *p.seed
		}
		entries[i] = Entry{ParticipantID: p.id, Seed: seed}
	}

	matches := GenerateSingleElimination(entries)
	if len(matches) == 0 {
		return 0, ErrNotEnough
	}

	return len(matches), s.persist(ctx, eventID, matches)
}

// persist writes the generated matches and their progression + slot entries in
// a single transaction. It maps the generator's flat indices to DB uuids, then
// wires next_match_id in a second pass.
func (s *Service) persist(ctx context.Context, eventID uuid.UUID, matches []Match) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Fresh draw: clear any existing matches for this event.
	if _, err := tx.Exec(ctx, `DELETE FROM matches WHERE event_id = $1`, eventID); err != nil {
		return err
	}

	ids := make([]uuid.UUID, len(matches))

	// Pass 1: insert matches, capturing generated ids. Bye/known slots set the
	// match status accordingly.
	for i, m := range matches {
		status := "pending"
		if m.Slot1.IsBye || m.Slot2.IsBye {
			status = "bye"
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO matches (event_id, match_no, status)
			VALUES ($1, $2, $3::match_status)
			RETURNING id`, eventID, i+1, status).Scan(&id); err != nil {
			return fmt.Errorf("insert match %d: %w", i, err)
		}
		ids[i] = id
	}

	// Pass 2: wire progression + slot participants.
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
}

type BracketMatch struct {
	ID           uuid.UUID     `json:"id"`
	MatchNo      int           `json:"match_no"`
	Status       string        `json:"status"`
	Participants []BracketSlot `json:"participants"`
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

	// Load matches with their next pointer, plus each slot's participant name/seed.
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.match_no, m.status, m.next_match_id,
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
		slots   []BracketSlot
	}
	order := []uuid.UUID{}
	byID := map[uuid.UUID]*mrec{}
	for rows.Next() {
		var mid uuid.UUID
		var matchNo int
		var status string
		var next *uuid.UUID
		var slot *int
		var pid *uuid.UUID
		var name *string
		var seed *int
		if err := rows.Scan(&mid, &matchNo, &status, &next, &slot, &pid, &name, &seed); err != nil {
			return nil, err
		}
		rec, ok := byID[mid]
		if !ok {
			rec = &mrec{matchNo: matchNo, status: status, next: next}
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
			ID: id, MatchNo: rec.matchNo, Status: rec.status, Participants: rec.slots,
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

func insertSlot(ctx context.Context, tx pgx.Tx, matchID uuid.UUID, slot int, s Slot) error {
	// A bye or a fed (winner-of) slot has no participant yet.
	var pid *uuid.UUID
	if s.ParticipantID != nil {
		pid = s.ParticipantID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO match_participants (match_id, participant_id, slot)
		VALUES ($1, $2, $3)`, matchID, pid, slot)
	return err
}

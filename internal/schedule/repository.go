package schedule

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("not permitted")
)

type Court struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sort_order"`
}

// Slot is a scheduled match on a court at a time. MatchLabel is a human summary
// of the assigned match (participant names) for display.
type Slot struct {
	ID         uuid.UUID  `json:"id"`
	CourtID    uuid.UUID  `json:"court_id"`
	CourtName  string     `json:"court_name"`
	MatchID    *uuid.UUID `json:"match_id"`
	MatchLabel *string    `json:"match_label"`
	StartsAt   time.Time  `json:"starts_at"`
	EndsAt     time.Time  `json:"ends_at"`
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// TournamentOrg returns the owning org of a tournament (for authz).
func (r *Repository) TournamentOrg(ctx context.Context, tournamentID uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT org_id FROM tournaments WHERE id = $1`, tournamentID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return orgID, err
}

// defaultVenue returns (creating if needed) the tournament's default venue id.
// Organizers manage courts directly; venues are an implementation detail.
func (r *Repository) defaultVenue(ctx context.Context, tx pgx.Tx, tournamentID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM venues WHERE tournament_id = $1 ORDER BY id LIMIT 1`, tournamentID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx,
			`INSERT INTO venues (tournament_id, name) VALUES ($1, 'Main Venue') RETURNING id`,
			tournamentID).Scan(&id); err != nil {
			return uuid.Nil, err
		}
		return id, nil
	}
	return id, err
}

func (r *Repository) ListCourts(ctx context.Context, tournamentID uuid.UUID) ([]Court, error) {
	const q = `
		SELECT c.id, c.name, c.sort_order
		FROM courts c JOIN venues v ON v.id = c.venue_id
		WHERE v.tournament_id = $1
		ORDER BY c.sort_order, c.name`
	rows, err := r.pool.Query(ctx, q, tournamentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Court{}
	for rows.Next() {
		var c Court
		if err := rows.Scan(&c.ID, &c.Name, &c.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repository) CreateCourt(ctx context.Context, tournamentID uuid.UUID, name string, sortOrder int) (*Court, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	venueID, err := r.defaultVenue(ctx, tx, tournamentID)
	if err != nil {
		return nil, err
	}
	var c Court
	if err := tx.QueryRow(ctx,
		`INSERT INTO courts (venue_id, name, sort_order) VALUES ($1, $2, $3)
		 RETURNING id, name, sort_order`, venueID, name, sortOrder).
		Scan(&c.ID, &c.Name, &c.SortOrder); err != nil {
		return nil, err
	}
	return &c, tx.Commit(ctx)
}

func (r *Repository) ListSlots(ctx context.Context, tournamentID uuid.UUID) ([]Slot, error) {
	const q = `
		SELECT s.id, s.court_id, c.name, s.match_id,
		       (SELECT string_agg(p.display_name, ' vs ' ORDER BY mp.slot)
		        FROM match_participants mp
		        LEFT JOIN participants p ON p.id = mp.participant_id
		        WHERE mp.match_id = s.match_id) AS match_label,
		       s.starts_at, s.ends_at
		FROM schedule_slots s JOIN courts c ON c.id = s.court_id
		WHERE s.tournament_id = $1
		ORDER BY s.starts_at, c.sort_order`
	rows, err := r.pool.Query(ctx, q, tournamentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Slot{}
	for rows.Next() {
		var s Slot
		if err := rows.Scan(&s.ID, &s.CourtID, &s.CourtName, &s.MatchID, &s.MatchLabel, &s.StartsAt, &s.EndsAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repository) CreateSlot(ctx context.Context, tournamentID, courtID uuid.UUID, matchID *uuid.UUID, startsAt, endsAt time.Time) (*Slot, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO schedule_slots (tournament_id, court_id, match_id, starts_at, ends_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		tournamentID, courtID, matchID, startsAt, endsAt).Scan(&id)
	if err != nil {
		return nil, err
	}
	// Also stamp the match's scheduled_at + court for the bracket/match views.
	if matchID != nil {
		_, _ = r.pool.Exec(ctx,
			`UPDATE matches SET scheduled_at = $2, court_id = $3,
			   status = CASE WHEN status = 'pending' THEN 'scheduled'::match_status ELSE status END
			 WHERE id = $1`, *matchID, startsAt, courtID)
	}
	return r.getSlot(ctx, id)
}

func (r *Repository) getSlot(ctx context.Context, id uuid.UUID) (*Slot, error) {
	var s Slot
	err := r.pool.QueryRow(ctx, `
		SELECT s.id, s.court_id, c.name, s.match_id,
		       (SELECT string_agg(p.display_name, ' vs ' ORDER BY mp.slot)
		        FROM match_participants mp LEFT JOIN participants p ON p.id = mp.participant_id
		        WHERE mp.match_id = s.match_id),
		       s.starts_at, s.ends_at
		FROM schedule_slots s JOIN courts c ON c.id = s.court_id
		WHERE s.id = $1`, id).
		Scan(&s.ID, &s.CourtID, &s.CourtName, &s.MatchID, &s.MatchLabel, &s.StartsAt, &s.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// UpdateSlot re-points a slot to a (possibly new) court/match/time. The prior
// match (if any and now different) is un-stamped; the new match is stamped with
// the slot's court + start, mirroring CreateSlot's side effect.
func (r *Repository) UpdateSlot(ctx context.Context, id, courtID uuid.UUID, matchID *uuid.UUID, startsAt, endsAt time.Time) (*Slot, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Which match was on this slot before? Needed to clear its stamp if it's
	// being unassigned or swapped out.
	var prevMatch *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT match_id FROM schedule_slots WHERE id = $1`, id).Scan(&prevMatch); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE schedule_slots SET court_id = $2, match_id = $3, starts_at = $4, ends_at = $5
		WHERE id = $1`, id, courtID, matchID, startsAt, endsAt); err != nil {
		return nil, err
	}

	// Un-stamp the previous match if it's gone or replaced.
	if prevMatch != nil && (matchID == nil || *prevMatch != *matchID) {
		_, _ = tx.Exec(ctx,
			`UPDATE matches SET scheduled_at = NULL, court_id = NULL,
			   status = CASE WHEN status = 'scheduled' THEN 'pending'::match_status ELSE status END
			 WHERE id = $1`, *prevMatch)
	}
	// Stamp the new match.
	if matchID != nil {
		_, _ = tx.Exec(ctx,
			`UPDATE matches SET scheduled_at = $2, court_id = $3,
			   status = CASE WHEN status = 'pending' THEN 'scheduled'::match_status ELSE status END
			 WHERE id = $1`, *matchID, startsAt, courtID)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.getSlot(ctx, id)
}

func (r *Repository) DeleteSlot(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var tournamentID uuid.UUID
	err := r.pool.QueryRow(ctx,
		`DELETE FROM schedule_slots WHERE id = $1 RETURNING tournament_id`, id).Scan(&tournamentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return tournamentID, err
}

// SlotTournamentOrg returns the owning org of a slot's tournament.
func (r *Repository) SlotTournamentOrg(ctx context.Context, slotID uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT t.org_id FROM schedule_slots s
		JOIN tournaments t ON t.id = s.tournament_id WHERE s.id = $1`, slotID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return orgID, err
}

// PublishedTournamentBySlug returns the tournament id for a published slug (for
// public schedule reads), or ErrNotFound.
func (r *Repository) PublishedTournamentBySlug(ctx context.Context, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM tournaments WHERE slug = $1 AND status = 'published'`, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}

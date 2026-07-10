package event

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("event not found")

// Event is an event/division within a tournament, with a rolled-up participant
// count for lists.
type Event struct {
	ID               uuid.UUID  `json:"id"`
	TournamentID     uuid.UUID  `json:"tournament_id"`
	Name             string     `json:"name"`
	Discipline       string     `json:"discipline"`
	Format           string     `json:"format"`
	PairingMode      string     `json:"pairing_mode"`
	ScoringProfileID *uuid.UUID `json:"scoring_profile_id"`
	CreatedAt        time.Time  `json:"created_at"`
	ParticipantCount int        `json:"participant_count"`
	MatchCount       int        `json:"match_count"`
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// tournamentOwned verifies a tournament belongs to the org (nil = super admin).
// Returns ErrNotFound if the tournament isn't visible to the caller, so event
// operations can't leak across orgs.
func (r *Repository) tournamentOwned(ctx context.Context, tournamentID uuid.UUID, orgID *uuid.UUID) error {
	var ok bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM tournaments WHERE id = $1 AND ($2::uuid IS NULL OR org_id = $2))`,
		tournamentID, orgID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) ListByTournament(ctx context.Context, tournamentID uuid.UUID) ([]Event, error) {
	const q = `
		SELECT e.id, e.tournament_id, e.name, e.discipline, e.format, e.pairing_mode, e.scoring_profile_id, e.created_at,
		       (SELECT COUNT(*) FROM participants p WHERE p.event_id = e.id) AS participant_count,
		       (SELECT COUNT(*) FROM matches m WHERE m.event_id = e.id) AS match_count
		FROM events e
		WHERE e.tournament_id = $1
		ORDER BY e.created_at ASC`
	rows, err := r.pool.Query(ctx, q, tournamentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TournamentID, &e.Name, &e.Discipline, &e.Format,
			&e.PairingMode, &e.ScoringProfileID, &e.CreatedAt, &e.ParticipantCount, &e.MatchCount); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*Event, error) {
	const q = `
		SELECT e.id, e.tournament_id, e.name, e.discipline, e.format, e.pairing_mode, e.scoring_profile_id, e.created_at,
		       (SELECT COUNT(*) FROM participants p WHERE p.event_id = e.id),
		       (SELECT COUNT(*) FROM matches m WHERE m.event_id = e.id)
		FROM events e
		WHERE e.id = $1`
	return scan(r.pool.QueryRow(ctx, q, id))
}

func (r *Repository) Create(ctx context.Context, in CreateInput) (*Event, error) {
	const q = `
		INSERT INTO events (tournament_id, name, discipline, format)
		VALUES ($1, $2, $3::event_discipline, $4::event_format)
		RETURNING id, tournament_id, name, discipline, format, pairing_mode, scoring_profile_id, created_at, 0, 0`
	return scan(r.pool.QueryRow(ctx, q, in.TournamentID, in.Name, in.Discipline, in.Format))
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	ct, err := r.pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scan(row pgx.Row) (*Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.TournamentID, &e.Name, &e.Discipline, &e.Format,
		&e.PairingMode, &e.ScoringProfileID, &e.CreatedAt, &e.ParticipantCount, &e.MatchCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

type CreateInput struct {
	TournamentID uuid.UUID
	Name         string
	Discipline   string
	Format       string
}

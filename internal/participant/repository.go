package participant

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("participant not found")

// Participant is an entry in an event. For singles it points at a player; for
// doubles at a team. DisplayName is what shows in the bracket/roster.
type Participant struct {
	ID          uuid.UUID  `json:"id"`
	EventID     uuid.UUID  `json:"event_id"`
	PlayerID    *uuid.UUID `json:"player_id"`
	TeamID      *uuid.UUID `json:"team_id"`
	DisplayName string     `json:"display_name"`
	Seed        *int       `json:"seed"`
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// eventOrg returns the org that owns the event's tournament, plus its
// discipline, so the service can authorize and branch singles/doubles.
func (r *Repository) eventOrg(ctx context.Context, eventID uuid.UUID) (orgID uuid.UUID, discipline string, err error) {
	err = r.pool.QueryRow(ctx, `
		SELECT t.org_id, e.discipline
		FROM events e JOIN tournaments t ON t.id = e.tournament_id
		WHERE e.id = $1`, eventID).Scan(&orgID, &discipline)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", ErrNotFound
	}
	return orgID, discipline, err
}

func (r *Repository) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Participant, error) {
	const q = `
		SELECT id, event_id, player_id, team_id, display_name, seed
		FROM participants
		WHERE event_id = $1
		ORDER BY COALESCE(seed, 9999), display_name`
	rows, err := r.pool.Query(ctx, q, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.ID, &p.EventID, &p.PlayerID, &p.TeamID, &p.DisplayName, &p.Seed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreateSingles creates a player (org-scoped) and a participant entry in one tx.
func (r *Repository) CreateSingles(ctx context.Context, orgID, eventID uuid.UUID, name string, seed *int) (*Participant, error) {
	name = strings.TrimSpace(name)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var playerID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO players (org_id, name) VALUES ($1, $2) RETURNING id`, orgID, name).Scan(&playerID); err != nil {
		return nil, err
	}
	var p Participant
	if err := tx.QueryRow(ctx, `
		INSERT INTO participants (event_id, player_id, display_name, seed)
		VALUES ($1, $2, $3, $4)
		RETURNING id, event_id, player_id, team_id, display_name, seed`,
		eventID, playerID, name, seed).Scan(&p.ID, &p.EventID, &p.PlayerID, &p.TeamID, &p.DisplayName, &p.Seed); err != nil {
		return nil, err
	}
	return &p, tx.Commit(ctx)
}

// CreateDoubles creates a team + participant entry. name is the pairing label.
func (r *Repository) CreateDoubles(ctx context.Context, eventID uuid.UUID, name string, seed *int) (*Participant, error) {
	name = strings.TrimSpace(name)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var teamID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO teams (event_id, name) VALUES ($1, $2) RETURNING id`, eventID, name).Scan(&teamID); err != nil {
		return nil, err
	}
	var p Participant
	if err := tx.QueryRow(ctx, `
		INSERT INTO participants (event_id, team_id, display_name, seed)
		VALUES ($1, $2, $3, $4)
		RETURNING id, event_id, player_id, team_id, display_name, seed`,
		eventID, teamID, name, seed).Scan(&p.ID, &p.EventID, &p.PlayerID, &p.TeamID, &p.DisplayName, &p.Seed); err != nil {
		return nil, err
	}
	return &p, tx.Commit(ctx)
}

// Rename updates a participant's display name, keeping the underlying player
// (singles) or team (doubles) name in sync so the roster and player/team
// records don't drift. Runs in one tx.
func (r *Repository) Rename(ctx context.Context, id uuid.UUID, name string) (*Participant, error) {
	name = strings.TrimSpace(name)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var p Participant
	err = tx.QueryRow(ctx, `
		UPDATE participants SET display_name = $2
		WHERE id = $1
		RETURNING id, event_id, player_id, team_id, display_name, seed`,
		id, name).Scan(&p.ID, &p.EventID, &p.PlayerID, &p.TeamID, &p.DisplayName, &p.Seed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Keep the source record's name aligned with the entry.
	if p.PlayerID != nil {
		if _, err := tx.Exec(ctx, `UPDATE players SET name = $2 WHERE id = $1`, *p.PlayerID, name); err != nil {
			return nil, err
		}
	}
	if p.TeamID != nil {
		if _, err := tx.Exec(ctx, `UPDATE teams SET name = $2 WHERE id = $1`, *p.TeamID, name); err != nil {
			return nil, err
		}
	}
	return &p, tx.Commit(ctx)
}

// SetSeed updates a participant's seed within its event.
func (r *Repository) SetSeed(ctx context.Context, id uuid.UUID, seed *int) (*Participant, error) {
	var p Participant
	err := r.pool.QueryRow(ctx, `
		UPDATE participants SET seed = $2
		WHERE id = $1
		RETURNING id, event_id, player_id, team_id, display_name, seed`,
		id, seed).Scan(&p.ID, &p.EventID, &p.PlayerID, &p.TeamID, &p.DisplayName, &p.Seed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var eventID uuid.UUID
	err := r.pool.QueryRow(ctx,
		`DELETE FROM participants WHERE id = $1 RETURNING event_id`, id).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return eventID, err
}

// EventTournamentPublished reports whether an event's tournament is published
// (gates public reads). Returns ErrNotFound if the event does not exist.
func (r *Repository) EventTournamentPublished(ctx context.Context, eventID uuid.UUID) (bool, error) {
	var status string
	err := r.pool.QueryRow(ctx, `
		SELECT t.status FROM events e
		JOIN tournaments t ON t.id = e.tournament_id
		WHERE e.id = $1`, eventID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return status == "published", nil
}

// EventOrgFor exposes the owning org of a participant's event (for authz).
func (r *Repository) EventOrgFor(ctx context.Context, participantID uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT t.org_id FROM participants p
		JOIN events e ON e.id = p.event_id
		JOIN tournaments t ON t.id = e.tournament_id
		WHERE p.id = $1`, participantID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return orgID, err
}

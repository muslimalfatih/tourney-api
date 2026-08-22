package event

import (
	"context"
	"encoding/json"
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
	Slug             string     `json:"slug"` // URL identifier, unique per tournament
	Discipline       string     `json:"discipline"`
	Format           string     `json:"format"`
	PairingMode      string     `json:"pairing_mode"`
	ScoringProfileID *uuid.UUID `json:"scoring_profile_id"`
	CreatedAt        time.Time  `json:"created_at"`
	ParticipantCount int        `json:"participant_count"`
	MatchCount       int        `json:"match_count"`

	// Scoring configuration (migration 00009): raw jsonb, validated on write
	// by match.ParseScoringConfig; '{}' = defaults (best of 3, full deciding
	// set). Served raw so the client sees exactly what is stored.
	Scoring json.RawMessage `json:"scoring"`

	// Public-facing configuration (migration 00003).
	Category          *string `json:"category"`            // organizer-defined; null = uncategorised
	Gender            string  `json:"gender"`              // men | women | mixed
	IsPublic          bool    `json:"is_public"`           // appears in the public category strip
	PublicDisplayName *string `json:"public_display_name"` // null → fall back to name
	PublicOrder       int     `json:"public_order"`        // tab order in the strip

	// Derived from format (not stored) so callers never special-case it.
	HasGroupStage    bool `json:"has_group_stage"`
	HasKnockoutStage bool `json:"has_knockout_stage"`
}

// deriveStages fills HasGroupStage/HasKnockoutStage from the format. This is the
// single source of truth — no stored booleans that could desync from the draw.
func (e *Event) deriveStages() {
	switch e.Format {
	case "group_knockout":
		e.HasGroupStage, e.HasKnockoutStage = true, true
	case "round_robin":
		e.HasGroupStage, e.HasKnockoutStage = true, false
	default: // single_elim
		e.HasGroupStage, e.HasKnockoutStage = false, true
	}
}

// columns and scanArgs keep the SELECT list and Scan targets in lockstep — the
// two most error-prone halves of a pgx query. Both public + count columns are
// included; callers that don't need counts pass literal 0s in RETURNING.
const eventCols = `e.id, e.tournament_id, e.name, e.slug, e.discipline, e.format, e.pairing_mode,
	e.scoring_profile_id, e.created_at, e.scoring, e.category, e.gender, e.is_public,
	e.public_display_name, e.public_order`

func (e *Event) scanArgs(participant, match *int) []any {
	return []any{
		&e.ID, &e.TournamentID, &e.Name, &e.Slug, &e.Discipline, &e.Format, &e.PairingMode,
		&e.ScoringProfileID, &e.CreatedAt, &e.Scoring, &e.Category, &e.Gender, &e.IsPublic,
		&e.PublicDisplayName, &e.PublicOrder, participant, match,
	}
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
	q := `
		SELECT ` + eventCols + `,
		       (SELECT COUNT(*) FROM participants p WHERE p.event_id = e.id) AS participant_count,
		       (SELECT COUNT(*) FROM matches m WHERE m.event_id = e.id) AS match_count
		FROM events e
		WHERE e.tournament_id = $1
		ORDER BY e.public_order ASC, e.created_at ASC`
	rows, err := r.pool.Query(ctx, q, tournamentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var pc, mc int
		if err := rows.Scan(e.scanArgs(&pc, &mc)...); err != nil {
			return nil, err
		}
		e.ParticipantCount, e.MatchCount = pc, mc
		e.deriveStages()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*Event, error) {
	q := `
		SELECT ` + eventCols + `,
		       (SELECT COUNT(*) FROM participants p WHERE p.event_id = e.id),
		       (SELECT COUNT(*) FROM matches m WHERE m.event_id = e.id)
		FROM events e
		WHERE e.id = $1`
	return scan(r.pool.QueryRow(ctx, q, id))
}

func (r *Repository) Create(ctx context.Context, in CreateInput) (*Event, error) {
	// New columns take their schema defaults (gender 'mixed', is_public true,
	// public_order 0, category/public_display_name null). RETURNING mirrors the
	// SELECT column order with 0/0 for the participant/match counts.
	//
	// `AS e` is load-bearing, not decoration: eventCols qualifies every column
	// with `e.` because the read queries alias the table (FROM events e) and
	// Update aliases it too (UPDATE events e SET). Without an alias here the
	// RETURNING list references a table that isn't in scope and Postgres rejects
	// the whole statement with 42P01 "missing FROM-clause entry for table e",
	// which surfaced as a 500 on every single Add Event.
	q := `
		INSERT INTO events AS e (tournament_id, name, slug, discipline, format, category)
		VALUES ($1, $2, $3, $4::event_discipline, $5::event_format, NULLIF(btrim($6), ''))
		RETURNING ` + eventCols + `, 0, 0`
	return scan(r.pool.QueryRow(ctx, q,
		in.TournamentID, in.Name, in.Slug, in.Discipline, in.Format, in.Category))
}

// Update mutates the public-facing config fields with patch semantics. For the
// nullable text columns, presence is signalled by the arg itself: a nil arg
// (JSON key absent or null) leaves the column unchanged; a present string —
// including "" — is applied, with NULLIF(btrim(...),”) turning empty/whitespace
// into SQL NULL (category → uncategorised; public_display_name → falls back to
// events.name on read). gender/is_public/public_order use plain COALESCE.
// Format/discipline are immutable here.
func (r *Repository) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*Event, error) {
	q := `
		UPDATE events e SET
			category            = CASE WHEN $2::text IS NULL THEN e.category ELSE NULLIF(btrim($2), '') END,
			gender              = COALESCE($3::event_gender, e.gender),
			is_public           = COALESCE($4, e.is_public),
			public_display_name = CASE WHEN $5::text IS NULL THEN e.public_display_name ELSE NULLIF(btrim($5), '') END,
			public_order        = COALESCE($6, e.public_order),
			scoring             = COALESCE($7::jsonb, e.scoring)
		WHERE e.id = $1
		RETURNING ` + eventCols + `,
		          (SELECT COUNT(*) FROM participants p WHERE p.event_id = e.id),
		          (SELECT COUNT(*) FROM matches m WHERE m.event_id = e.id)`
	return scan(r.pool.QueryRow(ctx, q,
		id, in.Category, in.Gender, in.IsPublic, in.PublicDisplayName, in.PublicOrder, in.Scoring,
	))
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
	var pc, mc int
	err := row.Scan(e.scanArgs(&pc, &mc)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.ParticipantCount, e.MatchCount = pc, mc
	e.deriveStages()
	return &e, nil
}

type CreateInput struct {
	TournamentID uuid.UUID
	Name         string
	Slug         string
	Category     string
	Discipline   string
	Format       string
}

// UpdateInput is patch-shaped: a nil pointer leaves the column unchanged. For
// Category and PublicDisplayName, a non-nil pointer to "" clears the column to
// SQL NULL (see Update's NULLIF); the caller passes the JSON *string straight
// through, since JSON omitted/null → nil and present-"" → &"" are distinct.
type UpdateInput struct {
	Category          *string
	Gender            *string
	IsPublic          *bool
	PublicDisplayName *string
	PublicOrder       *int
	// Scoring is the raw config document; nil leaves it unchanged. Validated
	// in the service before it reaches SQL.
	Scoring *string
}

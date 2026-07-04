package tournament

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a tournament lookup misses.
var ErrNotFound = errors.New("tournament not found")

// Tournament is the persisted record plus a rolled-up event count for lists.
type Tournament struct {
	ID          uuid.UUID      `json:"id"`
	OrgID       uuid.UUID      `json:"org_id"`
	Name        string         `json:"name"`
	Slug        string         `json:"slug"`
	Sport       string         `json:"sport"`
	Location    *string        `json:"location"`
	StartsOn    *time.Time     `json:"starts_on"`
	EndsOn      *time.Time     `json:"ends_on"`
	Branding    map[string]any `json:"branding"`
	Status      string         `json:"status"`
	PublishedAt *time.Time     `json:"published_at"`
	CreatedAt   time.Time      `json:"created_at"`
	EventCount  int            `json:"event_count"`
	// Events is populated only on public reads so the public site can list
	// divisions and pick a default bracket.
	Events []PublicEvent `json:"events,omitempty"`
}

// PublicEvent is a lightweight event summary embedded in the public payload.
type PublicEvent struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Discipline string    `json:"discipline"`
	Format     string    `json:"format"`
}

// Repository is the tournament data-access layer (pgx directly, matching the
// pattern established by the auth module).
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// ListByOrg returns an org's tournaments (newest first) with an event count,
// plus the total row count for pagination.
func (r *Repository) ListByOrg(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]Tournament, int64, error) {
	const q = `
		SELECT t.id, t.org_id, t.name, t.slug, t.sport, t.location,
		       t.starts_on, t.ends_on, t.status, t.published_at, t.created_at,
		       COUNT(e.id) AS event_count,
		       COUNT(*) OVER() AS total
		FROM tournaments t
		LEFT JOIN events e ON e.tournament_id = t.id
		WHERE t.org_id = $1
		GROUP BY t.id
		ORDER BY t.created_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.pool.Query(ctx, q, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Tournament
	var total int64
	for rows.Next() {
		var t Tournament
		if err := rows.Scan(
			&t.ID, &t.OrgID, &t.Name, &t.Slug, &t.Sport, &t.Location,
			&t.StartsOn, &t.EndsOn, &t.Status, &t.PublishedAt, &t.CreatedAt,
			&t.EventCount, &total,
		); err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// GetByID returns one tournament scoped to an org (nil orgID = no scope, for
// super admin).
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID, orgID *uuid.UUID) (*Tournament, error) {
	const q = `
		SELECT t.id, t.org_id, t.name, t.slug, t.sport, t.location,
		       t.starts_on, t.ends_on, t.branding, t.status, t.published_at, t.created_at,
		       (SELECT COUNT(*) FROM events e WHERE e.tournament_id = t.id) AS event_count
		FROM tournaments t
		WHERE t.id = $1 AND ($2::uuid IS NULL OR t.org_id = $2)`
	return scanFull(r.pool.QueryRow(ctx, q, id, orgID))
}

// GetBySlug returns a published-or-not tournament by slug (for public reads).
func (r *Repository) GetBySlug(ctx context.Context, slug string) (*Tournament, error) {
	const q = `
		SELECT t.id, t.org_id, t.name, t.slug, t.sport, t.location,
		       t.starts_on, t.ends_on, t.branding, t.status, t.published_at, t.created_at,
		       (SELECT COUNT(*) FROM events e WHERE e.tournament_id = t.id) AS event_count
		FROM tournaments t
		WHERE t.slug = $1`
	return scanFull(r.pool.QueryRow(ctx, q, slug))
}

// EventsFor returns a tournament's events for the public payload.
func (r *Repository) EventsFor(ctx context.Context, tournamentID uuid.UUID) ([]PublicEvent, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, name, discipline, format FROM events WHERE tournament_id = $1 ORDER BY created_at`,
		tournamentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PublicEvent{}
	for rows.Next() {
		var e PublicEvent
		if err := rows.Scan(&e.ID, &e.Name, &e.Discipline, &e.Format); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Create inserts a tournament and returns the full row.
func (r *Repository) Create(ctx context.Context, in CreateInput) (*Tournament, error) {
	const q = `
		INSERT INTO tournaments (org_id, name, slug, sport, location, starts_on, ends_on, branding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, '{}'::jsonb))
		RETURNING id, org_id, name, slug, sport, location, starts_on, ends_on, branding, status, published_at, created_at, 0`
	return scanFull(r.pool.QueryRow(ctx, q,
		in.OrgID, in.Name, in.Slug, in.Sport, in.Location, in.StartsOn, in.EndsOn, in.Branding,
	))
}

// Update mutates editable fields, scoped to org.
func (r *Repository) Update(ctx context.Context, id uuid.UUID, orgID *uuid.UUID, in UpdateInput) (*Tournament, error) {
	const q = `
		UPDATE tournaments t SET
			name     = COALESCE($3, t.name),
			location = COALESCE($4, t.location),
			starts_on = COALESCE($5, t.starts_on),
			ends_on   = COALESCE($6, t.ends_on),
			branding  = COALESCE($7, t.branding)
		WHERE t.id = $1 AND ($2::uuid IS NULL OR t.org_id = $2)
		RETURNING t.id, t.org_id, t.name, t.slug, t.sport, t.location, t.starts_on, t.ends_on, t.branding, t.status, t.published_at, t.created_at,
		          (SELECT COUNT(*) FROM events e WHERE e.tournament_id = t.id)`
	return scanFull(r.pool.QueryRow(ctx, q,
		id, orgID, in.Name, in.Location, in.StartsOn, in.EndsOn, in.Branding,
	))
}

// SetStatus transitions publish state, stamping published_at on publish.
func (r *Repository) SetStatus(ctx context.Context, id uuid.UUID, orgID *uuid.UUID, status string) (*Tournament, error) {
	const q = `
		UPDATE tournaments t SET
			status = $3::tournament_status,
			published_at = CASE WHEN $3 = 'published' THEN now() ELSE t.published_at END
		WHERE t.id = $1 AND ($2::uuid IS NULL OR t.org_id = $2)
		RETURNING t.id, t.org_id, t.name, t.slug, t.sport, t.location, t.starts_on, t.ends_on, t.branding, t.status, t.published_at, t.created_at,
		          (SELECT COUNT(*) FROM events e WHERE e.tournament_id = t.id)`
	return scanFull(r.pool.QueryRow(ctx, q, id, orgID, status))
}

// SlugExists reports whether a slug is already taken (for validation).
func (r *Repository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tournaments WHERE slug = $1)`, slug).Scan(&exists)
	return exists, err
}

func scanFull(row pgx.Row) (*Tournament, error) {
	var t Tournament
	err := row.Scan(
		&t.ID, &t.OrgID, &t.Name, &t.Slug, &t.Sport, &t.Location,
		&t.StartsOn, &t.EndsOn, &t.Branding, &t.Status, &t.PublishedAt, &t.CreatedAt, &t.EventCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Input structs -------------------------------------------------------------

type CreateInput struct {
	OrgID    uuid.UUID
	Name     string
	Slug     string
	Sport    string
	Location *string
	StartsOn *time.Time
	EndsOn   *time.Time
	Branding map[string]any
}

type UpdateInput struct {
	Name     *string
	Location *string
	StartsOn *time.Time
	EndsOn   *time.Time
	Branding map[string]any
}

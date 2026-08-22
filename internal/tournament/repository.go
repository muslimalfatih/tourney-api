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
	ID uuid.UUID `json:"id"`
	// Never serialized: org ownership is a private organizer concern enforced
	// server-side; the public payload must not leak it and no client reads it
	// (caught by inttest.TestSecurityMatrix on its first run).
	OrgID       uuid.UUID  `json:"-"`
	Name        string     `json:"name"`
	Slug        string     `json:"slug"`
	Sport       string     `json:"sport"`
	Description *string    `json:"description"`
	Location    *string    `json:"location"`
	StartsOn    *time.Time `json:"starts_on"`
	EndsOn      *time.Time `json:"ends_on"`
	// Timezone is the tournament's IANA presentation zone (validated at the
	// write path with time.LoadLocation; default Asia/Makassar). Storage stays
	// UTC — this only drives tournament-local grouping and display.
	Timezone    string         `json:"timezone"`
	Branding    map[string]any `json:"branding"`
	Status      string         `json:"status"`
	PublishedAt *time.Time     `json:"published_at"`
	CreatedAt   time.Time      `json:"created_at"`
	EventCount  int            `json:"event_count"`
	// Events is populated only on public reads so the public site can list
	// divisions and pick a default bracket.
	Events []PublicEvent `json:"events,omitempty"`
}

// PublicEvent is an event summary embedded in the public payload — carries the
// public-facing config the public site's category/gender/phase filters need.
type PublicEvent struct {
	ID                uuid.UUID `json:"id"`
	Name              string    `json:"name"`
	Slug              string    `json:"slug"` // URL identifier, unique per tournament
	Discipline        string    `json:"discipline"`
	Format            string    `json:"format"`
	Category          *string   `json:"category"`
	Gender            string    `json:"gender"`
	PublicDisplayName *string   `json:"public_display_name"`
	PublicOrder       int       `json:"public_order"`
	HasGroupStage     bool      `json:"has_group_stage"`
	HasKnockoutStage  bool      `json:"has_knockout_stage"`
}

// deriveStages mirrors event.deriveStages (format → stage flags) so the public
// payload can drive the phase toggle without a second round-trip.
func (e *PublicEvent) deriveStages() {
	switch e.Format {
	case "group_knockout":
		e.HasGroupStage, e.HasKnockoutStage = true, true
	case "round_robin":
		e.HasGroupStage, e.HasKnockoutStage = true, false
	default:
		e.HasGroupStage, e.HasKnockoutStage = false, true
	}
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
		SELECT t.id, t.org_id, t.name, t.slug, t.sport, t.description, t.location,
		       t.starts_on, t.ends_on, t.timezone, t.status, t.published_at, t.created_at,
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
			&t.ID, &t.OrgID, &t.Name, &t.Slug, &t.Sport, &t.Description, &t.Location,
			&t.StartsOn, &t.EndsOn, &t.Timezone, &t.Status, &t.PublishedAt, &t.CreatedAt,
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
		SELECT t.id, t.org_id, t.name, t.slug, t.sport, t.description, t.location,
		       t.starts_on, t.ends_on, t.timezone, t.branding, t.status, t.published_at, t.created_at,
		       (SELECT COUNT(*) FROM events e WHERE e.tournament_id = t.id) AS event_count
		FROM tournaments t
		WHERE t.id = $1 AND ($2::uuid IS NULL OR t.org_id = $2)`
	return scanFull(r.pool.QueryRow(ctx, q, id, orgID))
}

// GetBySlug returns a published-or-not tournament by slug (for public reads).
func (r *Repository) GetBySlug(ctx context.Context, slug string) (*Tournament, error) {
	const q = `
		SELECT t.id, t.org_id, t.name, t.slug, t.sport, t.description, t.location,
		       t.starts_on, t.ends_on, t.timezone, t.branding, t.status, t.published_at, t.created_at,
		       (SELECT COUNT(*) FROM events e WHERE e.tournament_id = t.id) AS event_count
		FROM tournaments t
		WHERE t.slug = $1`
	return scanFull(r.pool.QueryRow(ctx, q, slug))
}

// EventsFor returns a tournament's PUBLIC events for the public payload —
// only is_public=true divisions, ordered by public_order then creation.
func (r *Repository) EventsFor(ctx context.Context, tournamentID uuid.UUID) ([]PublicEvent, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, name, slug, discipline, format, category, gender, public_display_name, public_order
		 FROM events
		 WHERE tournament_id = $1 AND is_public = true
		 ORDER BY public_order ASC, created_at ASC`,
		tournamentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PublicEvent{}
	for rows.Next() {
		var e PublicEvent
		if err := rows.Scan(&e.ID, &e.Name, &e.Slug, &e.Discipline, &e.Format,
			&e.Category, &e.Gender, &e.PublicDisplayName, &e.PublicOrder); err != nil {
			return nil, err
		}
		e.deriveStages()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Create inserts a tournament and returns the full row.
func (r *Repository) Create(ctx context.Context, in CreateInput) (*Tournament, error) {
	const q = `
		INSERT INTO tournaments (org_id, name, slug, sport, description, location, starts_on, ends_on, timezone, branding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10, '{}'::jsonb))
		RETURNING id, org_id, name, slug, sport, description, location, starts_on, ends_on, timezone, branding, status, published_at, created_at, 0`
	return scanFull(r.pool.QueryRow(ctx, q,
		in.OrgID, in.Name, in.Slug, in.Sport, in.Description, in.Location, in.StartsOn, in.EndsOn, in.Timezone, in.Branding,
	))
}

// Update mutates editable fields, scoped to org.
func (r *Repository) Update(ctx context.Context, id uuid.UUID, orgID *uuid.UUID, in UpdateInput) (*Tournament, error) {
	const q = `
		UPDATE tournaments t SET
			name        = COALESCE($3, t.name),
			description = COALESCE($4, t.description),
			location    = COALESCE($5, t.location),
			starts_on   = COALESCE($6, t.starts_on),
			ends_on     = COALESCE($7, t.ends_on),
			branding    = COALESCE($8, t.branding),
			timezone    = COALESCE($9, t.timezone)
		WHERE t.id = $1 AND ($2::uuid IS NULL OR t.org_id = $2)
		RETURNING t.id, t.org_id, t.name, t.slug, t.sport, t.description, t.location, t.starts_on, t.ends_on, t.timezone, t.branding, t.status, t.published_at, t.created_at,
		          (SELECT COUNT(*) FROM events e WHERE e.tournament_id = t.id)`
	return scanFull(r.pool.QueryRow(ctx, q,
		id, orgID, in.Name, in.Description, in.Location, in.StartsOn, in.EndsOn, in.Branding, in.Timezone,
	))
}

// SetStatus transitions publish state, stamping published_at on publish.
func (r *Repository) SetStatus(ctx context.Context, id uuid.UUID, orgID *uuid.UUID, status string) (*Tournament, error) {
	const q = `
		UPDATE tournaments t SET
			status = $3::tournament_status,
			published_at = CASE WHEN $3 = 'published' THEN now() ELSE t.published_at END
		WHERE t.id = $1 AND ($2::uuid IS NULL OR t.org_id = $2)
		RETURNING t.id, t.org_id, t.name, t.slug, t.sport, t.description, t.location, t.starts_on, t.ends_on, t.timezone, t.branding, t.status, t.published_at, t.created_at,
		          (SELECT COUNT(*) FROM events e WHERE e.tournament_id = t.id)`
	return scanFull(r.pool.QueryRow(ctx, q, id, orgID, status))
}

// SlugExists reports whether a slug is already taken (for validation).
// IsPublishedSlug reports whether a tournament with this slug exists AND is
// published — the gate for anonymous surfaces (public SSE stream) that key on
// slug alone. Draft/archived tournaments are invisible here by design.
func (r *Repository) IsPublishedSlug(ctx context.Context, slug string) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM tournaments WHERE slug = $1 AND status = 'published')`,
		slug).Scan(&ok)
	return ok, err
}

func (r *Repository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tournaments WHERE slug = $1)`, slug).Scan(&exists)
	return exists, err
}

func scanFull(row pgx.Row) (*Tournament, error) {
	var t Tournament
	err := row.Scan(
		&t.ID, &t.OrgID, &t.Name, &t.Slug, &t.Sport, &t.Description, &t.Location,
		&t.StartsOn, &t.EndsOn, &t.Timezone, &t.Branding, &t.Status, &t.PublishedAt, &t.CreatedAt, &t.EventCount,
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
	OrgID       uuid.UUID
	Name        string
	Slug        string
	Sport       string
	Description *string
	Location    *string
	StartsOn    *time.Time
	EndsOn      *time.Time
	Timezone    string
	Branding    map[string]any
}

type UpdateInput struct {
	Name        *string
	Description *string
	Location    *string
	StartsOn    *time.Time
	EndsOn      *time.Time
	Timezone    *string
	Branding    map[string]any
}

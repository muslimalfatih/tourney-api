package platform

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
	ErrEmailUsed = errors.New("email already in use")
	ErrSlugUsed  = errors.New("slug already in use")
)

// Organization is a super-admin view of an org with rolled-up counts.
type Organization struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	Slug            string    `json:"slug"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	OrganizerCount  int       `json:"organizer_count"`
	TournamentCount int       `json:"tournament_count"`
}

// GlobalTournament is a super-admin view of a tournament with its org name.
type GlobalTournament struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Status    string    `json:"status"`
	OrgID     uuid.UUID `json:"org_id"`
	OrgName   string    `json:"org_name"`
	CreatedAt time.Time `json:"created_at"`
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) ListOrgs(ctx context.Context, limit, offset int) ([]Organization, int64, error) {
	const q = `
		SELECT o.id, o.name, o.slug, o.status, o.created_at,
		       (SELECT COUNT(*) FROM users u WHERE u.org_id = o.id) AS organizer_count,
		       (SELECT COUNT(*) FROM tournaments t WHERE t.org_id = o.id) AS tournament_count,
		       COUNT(*) OVER() AS total
		FROM organizations o
		ORDER BY o.created_at DESC
		LIMIT $1 OFFSET $2`
	rows, err := r.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Organization{}
	var total int64
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.Slug, &o.Status, &o.CreatedAt,
			&o.OrganizerCount, &o.TournamentCount, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, o)
	}
	return out, total, rows.Err()
}

// CreateOrgWithOrganizer creates an org and its first organizer user in one tx.
func (r *Repository) CreateOrgWithOrganizer(ctx context.Context, orgName, orgSlug, email, name, passwordHash string) (*Organization, error) {
	// Pre-check uniqueness for clearer errors than a constraint violation.
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE slug=$1)`, orgSlug).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrSlugUsed
	}
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email=$1)`, email).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrEmailUsed
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var o Organization
	if err := tx.QueryRow(ctx, `
		INSERT INTO organizations (name, slug) VALUES ($1, $2)
		RETURNING id, name, slug, status, created_at`, orgName, orgSlug).
		Scan(&o.ID, &o.Name, &o.Slug, &o.Status, &o.CreatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (email, password_hash, name, role, org_id)
		VALUES ($1, $2, $3, 'organizer', $4)`, email, passwordHash, name, o.ID); err != nil {
		return nil, err
	}
	o.OrganizerCount = 1
	return &o, tx.Commit(ctx)
}

func (r *Repository) ListAllTournaments(ctx context.Context, limit, offset int) ([]GlobalTournament, int64, error) {
	const q = `
		SELECT t.id, t.name, t.slug, t.status, t.org_id, o.name AS org_name, t.created_at,
		       COUNT(*) OVER() AS total
		FROM tournaments t JOIN organizations o ON o.id = t.org_id
		ORDER BY t.created_at DESC
		LIMIT $1 OFFSET $2`
	rows, err := r.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []GlobalTournament{}
	var total int64
	for rows.Next() {
		var t GlobalTournament
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.Status, &t.OrgID, &t.OrgName, &t.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// SetTournamentStatus lets a super admin suspend/archive/restore any tournament.
func (r *Repository) SetTournamentStatus(ctx context.Context, id uuid.UUID, status string) (*GlobalTournament, error) {
	const q = `
		UPDATE tournaments t SET status = $2::tournament_status
		FROM organizations o
		WHERE t.id = $1 AND o.id = t.org_id
		RETURNING t.id, t.name, t.slug, t.status, t.org_id, o.name, t.created_at`
	var t GlobalTournament
	err := r.pool.QueryRow(ctx, q, id, status).
		Scan(&t.ID, &t.Name, &t.Slug, &t.Status, &t.OrgID, &t.OrgName, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

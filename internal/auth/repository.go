package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUserNotFound is returned when no user matches the lookup.
var ErrUserNotFound = errors.New("user not found")

// User is the persisted account record. OrgID is nil for super admins.
type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	Name         string
	Role         string
	OrgID        *uuid.UUID
}

// Repository is the auth data-access layer.
//
// NOTE: auth is written against pgx directly so the module is fully functional
// from day one. Once sqlc queries are generated (see db/queries/auth.sql) these
// method bodies can be swapped to call the generated Queries without changing
// the service or handler.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) FindByEmail(ctx context.Context, email string) (*User, error) {
	const q = `
		SELECT id, email, password_hash, name, role, org_id
		FROM users
		WHERE email = $1`
	row := r.pool.QueryRow(ctx, q, email)
	return scanUser(row)
}

func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*User, error) {
	const q = `
		SELECT id, email, password_hash, name, role, org_id
		FROM users
		WHERE id = $1`
	row := r.pool.QueryRow(ctx, q, id)
	return scanUser(row)
}

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.OrgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

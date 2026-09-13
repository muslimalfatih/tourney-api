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

// User statuses, mirroring the user_status enum in 00014.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
)

// User is the persisted account record. OrgID is nil for super admins.
type User struct {
	ID uuid.UUID
	// Email is always stored normalized (see NormalizeEmail).
	Email string
	// PasswordHash is nil for OTP-only users, who never had a password. It is
	// a pointer rather than "" so "no password" and "empty password" cannot be
	// confused at a call site.
	PasswordHash *string
	Name         string
	Role         string
	OrgID        *uuid.UUID
	Status       string
}

// IsActive reports whether this account may hold a session.
func (u *User) IsActive() bool { return u.Status == StatusActive }

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
	// Compare on lower(email) so the lookup matches the unique index added in
	// 00014 — an address is one account regardless of how it was typed.
	const q = `
		SELECT id, email, password_hash, name, role, org_id, status
		FROM users
		WHERE lower(email) = $1`
	row := r.pool.QueryRow(ctx, q, NormalizeEmail(email))
	return scanUser(row)
}

func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*User, error) {
	const q = `
		SELECT id, email, password_hash, name, role, org_id, status
		FROM users
		WHERE id = $1`
	row := r.pool.QueryRow(ctx, q, id)
	return scanUser(row)
}

// TouchLastLogin records a successful sign-in. Best-effort: failing to record
// it must never fail the login it was observing.
func (r *Repository) TouchLastLogin(ctx context.Context, id uuid.UUID) {
	_, _ = r.pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
}

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.OrgID, &u.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

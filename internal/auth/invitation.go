package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Invitation statuses, mirroring the invitation_status enum in 00015.
const (
	InvitePending  = "pending"
	InviteAccepted = "accepted"
	InviteRevoked  = "revoked"
)

var (
	// ErrNotInvited means no active invitation exists for the address. The
	// product deliberately says so out loud — an invite-only tool that gave a
	// vague error would leave invited people unable to tell a typo from an
	// outage — but it is the ONLY thing disclosed: nothing reveals whether a
	// revoked or expired invitation once existed.
	ErrNotInvited = errors.New("not invited")

	// ErrInvitationExpired means a pending invitation aged out. Distinct from
	// ErrNotInvited because the remedy differs: ask the admin to re-send.
	ErrInvitationExpired = errors.New("invitation expired")

	// ErrInvitationExists guards against a second active invitation for one
	// address, which the partial unique index also enforces.
	ErrInvitationExists = errors.New("an active invitation already exists for this email")
)

// Invitation is a row of the allowlist.
type Invitation struct {
	ID             uuid.UUID
	Email          string
	EmailDisplay   *string
	Role           string
	OrganizationID *uuid.UUID
	Status         string
	InvitedBy      *uuid.UUID
	CreatedAt      time.Time
	AcceptedAt     *time.Time
	RevokedAt      *time.Time
	LastSentAt     *time.Time
	ExpiresAt      time.Time
}

// IsActive reports whether this invitation still admits its holder.
//
// An accepted invitation ignores expires_at: the account exists and is in use,
// and an expiry date should never lock someone out of an account they have
// been signing into. Only pending invitations age out.
func (i *Invitation) IsActive() bool {
	if i.Status == InviteRevoked {
		return false
	}
	if i.Status == InviteAccepted {
		return true
	}
	return i.ExpiresAt.After(time.Now())
}

// InvitationRepository is the data-access layer for the allowlist.
type InvitationRepository struct {
	pool *pgxpool.Pool
}

func NewInvitationRepository(pool *pgxpool.Pool) *InvitationRepository {
	return &InvitationRepository{pool: pool}
}

const inviteCols = `id, email, email_display, role, organization_id, status,
	invited_by_user_id, created_at, accepted_at, revoked_at, last_sent_at, expires_at`

func scanInvitation(row pgx.Row) (*Invitation, error) {
	var i Invitation
	err := row.Scan(&i.ID, &i.Email, &i.EmailDisplay, &i.Role, &i.OrganizationID,
		&i.Status, &i.InvitedBy, &i.CreatedAt, &i.AcceptedAt, &i.RevokedAt,
		&i.LastSentAt, &i.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotInvited
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// FindActiveByEmail is the allowlist gate every OTP request passes through.
//
// Returns ErrNotInvited when there is no usable invitation, and
// ErrInvitationExpired when one exists but has aged out — the caller turns
// those into the two distinct 403s the product specifies.
func (r *InvitationRepository) FindActiveByEmail(ctx context.Context, email string) (*Invitation, error) {
	const q = `
		SELECT ` + inviteCols + `
		FROM invitations
		WHERE email = $1 AND status <> 'revoked'`
	inv, err := scanInvitation(r.pool.QueryRow(ctx, q, NormalizeEmail(email)))
	if err != nil {
		return nil, err
	}
	if !inv.IsActive() {
		return nil, ErrInvitationExpired
	}
	return inv, nil
}

// FindByID loads an invitation regardless of status, for admin views.
func (r *InvitationRepository) FindByID(ctx context.Context, id uuid.UUID) (*Invitation, error) {
	const q = `SELECT ` + inviteCols + ` FROM invitations WHERE id = $1`
	return scanInvitation(r.pool.QueryRow(ctx, q, id))
}

// CreateInvitationParams describes an invitation to issue. OrganizationID is
// required for organizers and must be nil for super admins — the
// invitations_org_scope CHECK rejects anything else, so a bug here surfaces as
// a failed insert rather than a user who cannot sign in.
type CreateInvitationParams struct {
	Email        string
	Role         string
	Organization *uuid.UUID
	InvitedBy    *uuid.UUID
	Note         *string
	// TTL offsets expires_at from now. Zero uses the column default of 14 days.
	// Any non-zero value is honoured, including a negative one — silently
	// substituting the default for a TTL the caller passed would hide the
	// mistake, and tests need to be able to construct an already-expired row.
	TTL time.Duration
}

// Create issues an invitation.
//
// A revoked invitation is never revived: the partial unique index allows one
// active row per address alongside unlimited revoked history, so re-inviting
// someone inserts a NEW row and the record of the original revocation survives.
func (r *InvitationRepository) Create(ctx context.Context, q Querier, p CreateInvitationParams) (*Invitation, error) {
	if q == nil {
		q = r.pool
	}
	normalized := NormalizeEmail(p.Email)

	expires := "now() + INTERVAL '14 days'"
	args := []any{normalized, p.Email, p.Role, p.Organization, p.InvitedBy, p.Note}
	if p.TTL != 0 {
		expires = "$7"
		args = append(args, time.Now().Add(p.TTL))
	}

	stmt := `
		INSERT INTO invitations
			(email, email_display, role, organization_id, invited_by_user_id, note, expires_at)
		VALUES ($1, $2, $3::user_role, $4, $5, $6, ` + expires + `)
		RETURNING ` + inviteCols

	inv, err := scanInvitation(q.QueryRow(ctx, stmt, args...))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrInvitationExists
	}
	return inv, err
}

// MarkAccepted records that the holder signed in for the first time. Scoped to
// pending rows so it can never resurrect a revoked invitation.
func (r *InvitationRepository) MarkAccepted(ctx context.Context, q Querier, id uuid.UUID) error {
	if q == nil {
		q = r.pool
	}
	_, err := q.Exec(ctx, `
		UPDATE invitations
		SET status = 'accepted', accepted_at = now()
		WHERE id = $1 AND status = 'pending'`, id)
	return err
}

// Revoke ends an invitation. Terminal: the row stays for audit and re-inviting
// creates a new one.
func (r *InvitationRepository) Revoke(ctx context.Context, q Querier, id uuid.UUID) error {
	if q == nil {
		q = r.pool
	}
	_, err := q.Exec(ctx, `
		UPDATE invitations
		SET status = 'revoked', revoked_at = now()
		WHERE id = $1 AND status <> 'revoked'`, id)
	return err
}

// TouchLastSent records an invitation email send, for the resend cooldown.
func (r *InvitationRepository) TouchLastSent(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE invitations SET last_sent_at = now() WHERE id = $1`, id)
	return err
}

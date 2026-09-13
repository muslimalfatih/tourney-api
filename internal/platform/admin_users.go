package platform

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/muslimalfatih/tourney-api/internal/audit"
	"github.com/muslimalfatih/tourney-api/internal/auth"
	"github.com/muslimalfatih/tourney-api/internal/email"
)

// Audit actions for platform user and invitation management.
const (
	ActionInvitationCreated = "admin.invitation_created"
	ActionInvitationResent  = "admin.invitation_resent"
	ActionInvitationRevoked = "admin.invitation_revoked"
	ActionUserRoleChanged   = "admin.user_role_changed"
	ActionUserSuspended     = "admin.user_suspended"
	ActionUserReactivated   = "admin.user_reactivated"
)

var (
	// ErrLastSuperAdmin refuses to remove the final active super admin. Without
	// this, one mis-click locks every human out of platform administration
	// with no path back but the database.
	ErrLastSuperAdmin = errors.New("cannot demote or suspend the last active super admin")
	ErrOrgRequired    = errors.New("an organization is required for organizers")
	ErrOrgForbidden   = errors.New("super admins do not belong to an organization")
	ErrUserNotFound   = errors.New("user not found")
	ErrBadRole        = errors.New("role must be organizer or super_admin")
)

// Attribution names the two people an admin action is recorded against.
// Effective and Session are nil unless the actor is impersonating — which,
// for platform actions, RequireSuperAdmin already forbids, but the type is
// shared with organizer-side writers where it matters.
type Attribution struct {
	Actor     uuid.UUID
	Effective *uuid.UUID
	Session   *uuid.UUID
}

func (a Attribution) entry(e audit.Entry) audit.Entry {
	e.ActorUserID = a.Actor
	e.EffectiveUserID = a.Effective
	e.ImpersonationSessionID = a.Session
	return e
}

// AdminUser is the platform view of an account.
type AdminUser struct {
	ID              uuid.UUID  `json:"id"`
	Email           string     `json:"email"`
	Name            string     `json:"name"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	OrgID           *uuid.UUID `json:"org_id"`
	OrgName         *string    `json:"org_name"`
	LastLoginAt     *time.Time `json:"last_login_at"`
	CreatedAt       time.Time  `json:"created_at"`
	InvitationState *string    `json:"invitation_state"`
	TournamentCount int        `json:"tournament_count"`
	HasPassword     bool       `json:"has_password"`
}

// UserFilter narrows the user list.
type UserFilter struct {
	Search string
	Role   string
	Status string
}

// ListUsers returns accounts with the context an admin needs to act on them.
func (s *Service) ListUsers(ctx context.Context, f UserFilter, limit, offset int) ([]AdminUser, int64, error) {
	var search *string
	if q := strings.TrimSpace(f.Search); q != "" {
		v := "%" + strings.ToLower(q) + "%"
		search = &v
	}
	var role, status *string
	if f.Role != "" {
		role = &f.Role
	}
	if f.Status != "" {
		status = &f.Status
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.name, u.role::text, u.status::text, u.org_id, o.name,
		       u.last_login_at, u.created_at, u.password_hash IS NOT NULL,
		       (SELECT status::text FROM invitations i
		          WHERE i.email = lower(u.email) AND i.status <> 'revoked' LIMIT 1),
		       (SELECT count(*) FROM tournaments t WHERE t.org_id = u.org_id),
		       COUNT(*) OVER()
		FROM users u
		LEFT JOIN organizations o ON o.id = u.org_id
		WHERE ($3::text IS NULL OR lower(u.email) LIKE $3 OR lower(u.name) LIKE $3)
		  AND ($4::text IS NULL OR u.role::text = $4)
		  AND ($5::text IS NULL OR u.status::text = $5)
		ORDER BY u.created_at DESC
		LIMIT $1 OFFSET $2`, limit, offset, search, role, status)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AdminUser{}
	var total int64
	for rows.Next() {
		var u AdminUser
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Status, &u.OrgID, &u.OrgName,
			&u.LastLoginAt, &u.CreatedAt, &u.HasPassword, &u.InvitationState, &u.TournamentCount, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// GetUser loads one account in the platform view.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*AdminUser, error) {
	users, _, err := s.listUsersByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, ErrUserNotFound
	}
	return &users[0], nil
}

func (s *Service) listUsersByID(ctx context.Context, id uuid.UUID) ([]AdminUser, int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.name, u.role::text, u.status::text, u.org_id, o.name,
		       u.last_login_at, u.created_at, u.password_hash IS NOT NULL,
		       (SELECT status::text FROM invitations i
		          WHERE i.email = lower(u.email) AND i.status <> 'revoked' LIMIT 1),
		       (SELECT count(*) FROM tournaments t WHERE t.org_id = u.org_id),
		       1::bigint
		FROM users u
		LEFT JOIN organizations o ON o.id = u.org_id
		WHERE u.id = $1`, id)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AdminUser{}
	var total int64
	for rows.Next() {
		var u AdminUser
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Status, &u.OrgID, &u.OrgName,
			&u.LastLoginAt, &u.CreatedAt, &u.HasPassword, &u.InvitationState, &u.TournamentCount, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// otherActiveSuperAdmins counts active super admins EXCLUDING one user, with
// the rows locked so a concurrent demotion cannot slip past the guard.
func otherActiveSuperAdmins(ctx context.Context, tx pgx.Tx, except uuid.UUID) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id FROM users
		WHERE role = 'super_admin' AND status = 'active' AND id <> $1
		FOR UPDATE`, except)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// ChangeRole switches an account between organizer and super_admin.
//
// users_org_scope means the organization must change in the same statement:
// a super admin has none, an organizer must have one. The caller supplies the
// organization when demoting; promoting always clears it.
func (s *Service) ChangeRole(ctx context.Context, by Attribution, targetID uuid.UUID, newRole string, orgID *uuid.UUID, reason *string) (*AdminUser, error) {
	if newRole != "organizer" && newRole != "super_admin" {
		return nil, ErrBadRole
	}
	if newRole == "organizer" && orgID == nil {
		return nil, ErrOrgRequired
	}
	if newRole == "super_admin" && orgID != nil {
		return nil, ErrOrgForbidden
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var curRole, curStatus string
	if err := tx.QueryRow(ctx,
		`SELECT role::text, status::text FROM users WHERE id = $1 FOR UPDATE`, targetID).
		Scan(&curRole, &curStatus); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	} else if err != nil {
		return nil, err
	}

	// The invariant, checked inside the same transaction as the change.
	if curRole == "super_admin" && curStatus == "active" && newRole != "super_admin" {
		others, err := otherActiveSuperAdmins(ctx, tx, targetID)
		if err != nil {
			return nil, err
		}
		if others == 0 {
			return nil, ErrLastSuperAdmin
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET role = $2::user_role, org_id = $3 WHERE id = $1`,
		targetID, newRole, orgID); err != nil {
		return nil, err
	}

	// A demoted super admin loses their platform sessions at once, and any
	// impersonation they had open. A promoted organizer's sessions stay — they
	// gain access, they do not lose it — but their role claim is stale, and
	// the verifier re-reads the row per request so that is harmless.
	if curRole == "super_admin" && newRole != "super_admin" {
		if err := s.sessions.RevokeAllForUser(ctx, tx, targetID, "role_changed"); err != nil {
			return nil, err
		}
	}

	if err := s.audit.RecordTx(ctx, tx, by.entry(audit.Entry{
		Action: ActionUserRoleChanged, TargetType: "user", TargetID: targetID.String(),
		Reason: reason, OrgID: orgID,
		Diff: map[string]any{"role": map[string]string{"from": curRole, "to": newRole}},
	})); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetUser(ctx, targetID)
}

// SetUserStatus suspends or reactivates an account, with the same last-super-
// admin guard as ChangeRole.
func (s *Service) SetUserStatus(ctx context.Context, by Attribution, targetID uuid.UUID, status string, reason *string) (*AdminUser, error) {
	if status != auth.StatusActive && status != auth.StatusSuspended {
		return nil, errors.New("status must be active or suspended")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var curRole, curStatus string
	if err := tx.QueryRow(ctx,
		`SELECT role::text, status::text FROM users WHERE id = $1 FOR UPDATE`, targetID).
		Scan(&curRole, &curStatus); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	} else if err != nil {
		return nil, err
	}
	if curStatus == status {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return s.GetUser(ctx, targetID)
	}

	if status == auth.StatusSuspended && curRole == "super_admin" && curStatus == "active" {
		others, err := otherActiveSuperAdmins(ctx, tx, targetID)
		if err != nil {
			return nil, err
		}
		if others == 0 {
			return nil, ErrLastSuperAdmin
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET status = $2::user_status WHERE id = $1`, targetID, status); err != nil {
		return nil, err
	}

	action := ActionUserReactivated
	if status == auth.StatusSuspended {
		action = ActionUserSuspended
		// Suspension means NOW, not at token expiry. This also ends any
		// impersonation targeting them, and any they started.
		if err := s.sessions.RevokeAllForUser(ctx, tx, targetID, "suspended"); err != nil {
			return nil, err
		}
	}

	if err := s.audit.RecordTx(ctx, tx, by.entry(audit.Entry{
		Action: action, TargetType: "user", TargetID: targetID.String(), Reason: reason,
		Diff: map[string]any{"status": map[string]string{"from": curStatus, "to": status}},
	})); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetUser(ctx, targetID)
}

// ---- Invitations -----------------------------------------------------------

// AdminInvitation is the platform view of an invitation.
type AdminInvitation struct {
	ID           uuid.UUID  `json:"id"`
	Email        string     `json:"email"`
	EmailDisplay *string    `json:"email_display"`
	Role         string     `json:"role"`
	OrgID        *uuid.UUID `json:"organization_id"`
	OrgName      *string    `json:"organization_name"`
	Status       string     `json:"status"`
	Active       bool       `json:"active"`
	InvitedBy    *string    `json:"invited_by"`
	Note         *string    `json:"note"`
	CreatedAt    time.Time  `json:"created_at"`
	AcceptedAt   *time.Time `json:"accepted_at"`
	RevokedAt    *time.Time `json:"revoked_at"`
	LastSentAt   *time.Time `json:"last_sent_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
}

// InvitationFilter narrows the invitation list.
type InvitationFilter struct {
	Search string
	Status string
	Role   string
}

func (s *Service) ListInvitations(ctx context.Context, f InvitationFilter, limit, offset int) ([]AdminInvitation, int64, error) {
	var search, status, role *string
	if q := strings.TrimSpace(f.Search); q != "" {
		v := "%" + strings.ToLower(q) + "%"
		search = &v
	}
	if f.Status != "" {
		status = &f.Status
	}
	if f.Role != "" {
		role = &f.Role
	}
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.email, i.email_display, i.role::text, i.organization_id, o.name,
		       i.status::text,
		       (i.status <> 'revoked' AND (i.status = 'accepted' OR i.expires_at > now())),
		       u.email, i.note, i.created_at, i.accepted_at, i.revoked_at, i.last_sent_at, i.expires_at,
		       COUNT(*) OVER()
		FROM invitations i
		LEFT JOIN organizations o ON o.id = i.organization_id
		LEFT JOIN users u ON u.id = i.invited_by_user_id
		WHERE ($3::text IS NULL OR i.email LIKE $3)
		  AND ($4::text IS NULL OR i.status::text = $4)
		  AND ($5::text IS NULL OR i.role::text = $5)
		ORDER BY i.created_at DESC
		LIMIT $1 OFFSET $2`, limit, offset, search, status, role)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AdminInvitation{}
	var total int64
	for rows.Next() {
		var i AdminInvitation
		if err := rows.Scan(&i.ID, &i.Email, &i.EmailDisplay, &i.Role, &i.OrgID, &i.OrgName,
			&i.Status, &i.Active, &i.InvitedBy, &i.Note, &i.CreatedAt, &i.AcceptedAt,
			&i.RevokedAt, &i.LastSentAt, &i.ExpiresAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, i)
	}
	return out, total, rows.Err()
}

func (s *Service) getInvitation(ctx context.Context, id uuid.UUID) (*AdminInvitation, error) {
	var i AdminInvitation
	err := s.pool.QueryRow(ctx, `
		SELECT i.id, i.email, i.email_display, i.role::text, i.organization_id, o.name,
		       i.status::text,
		       (i.status <> 'revoked' AND (i.status = 'accepted' OR i.expires_at > now())),
		       u.email, i.note, i.created_at, i.accepted_at, i.revoked_at, i.last_sent_at, i.expires_at
		FROM invitations i
		LEFT JOIN organizations o ON o.id = i.organization_id
		LEFT JOIN users u ON u.id = i.invited_by_user_id
		WHERE i.id = $1`, id).Scan(&i.ID, &i.Email, &i.EmailDisplay, &i.Role, &i.OrgID, &i.OrgName,
		&i.Status, &i.Active, &i.InvitedBy, &i.Note, &i.CreatedAt, &i.AcceptedAt,
		&i.RevokedAt, &i.LastSentAt, &i.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrNotInvited
	}
	return &i, err
}

// CreateInvitation issues an invitation and emails it.
//
// The email is sent AFTER the row commits. Sending first and then failing the
// insert would leave someone holding an invitation email that the system does
// not know about; the reverse leaves a row they can be re-sent to.
func (s *Service) CreateInvitation(ctx context.Context, by Attribution, p auth.CreateInvitationParams, sender email.Sender) (*AdminInvitation, error) {
	if p.Role != "organizer" && p.Role != "super_admin" {
		return nil, ErrBadRole
	}
	if p.Role == "organizer" && p.Organization == nil {
		return nil, ErrOrgRequired
	}
	if p.Role == "super_admin" && p.Organization != nil {
		return nil, ErrOrgForbidden
	}
	p.InvitedBy = &by.Actor

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inv, err := s.invitations.Create(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	if err := s.audit.RecordTx(ctx, tx, by.entry(audit.Entry{
		Action: ActionInvitationCreated, TargetType: "invitation", TargetID: inv.ID.String(),
		OrgID: p.Organization,
		Diff:  map[string]any{"email": inv.Email, "role": inv.Role},
	})); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// Best effort: a delivery failure does not undo the invitation. The admin
	// can resend, and the row's last_sent_at stays NULL as the signal.
	if err := sender.SendInvitation(ctx, inv.Email, inv.Role); err == nil {
		_ = s.invitations.TouchLastSent(ctx, inv.ID)
	}
	return s.getInvitation(ctx, inv.ID)
}

// ResendInvitation emails an active invitation again.
func (s *Service) ResendInvitation(ctx context.Context, by Attribution, id uuid.UUID, sender email.Sender) (*AdminInvitation, error) {
	inv, err := s.invitations.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !inv.IsActive() {
		return nil, auth.ErrInvitationExpired
	}
	if err := sender.SendInvitation(ctx, inv.Email, inv.Role); err != nil {
		return nil, auth.ErrDeliveryFailed
	}
	_ = s.invitations.TouchLastSent(ctx, inv.ID)
	_ = s.audit.Record(ctx, by.entry(audit.Entry{
		Action: ActionInvitationResent, TargetType: "invitation", TargetID: inv.ID.String(),
		OrgID: inv.OrganizationID,
	}))
	return s.getInvitation(ctx, id)
}

// RevokeInvitation withdraws an invitation and, if it was accepted, ends the
// account's sessions in the same transaction.
func (s *Service) RevokeInvitation(ctx context.Context, by Attribution, id uuid.UUID, reason *string) (*AdminInvitation, error) {
	inv, err := s.invitations.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authSvc.RevokeInvitation(ctx, s.invitations, id, "invitation_revoked"); err != nil {
		return nil, err
	}
	_ = s.audit.Record(ctx, by.entry(audit.Entry{
		Action: ActionInvitationRevoked, TargetType: "invitation", TargetID: inv.ID.String(),
		OrgID: inv.OrganizationID, Reason: reason,
		Diff: map[string]any{"email": inv.Email},
	}))
	return s.getInvitation(ctx, id)
}

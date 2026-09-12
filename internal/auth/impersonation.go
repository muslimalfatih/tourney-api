package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/audit"
)

// ImpersonationTTL is short on purpose. A support session should not outlive
// the problem it was opened for, and a forgotten tab should not hold a
// borrowed identity for a month.
const ImpersonationTTL = 30 * time.Minute

const (
	ActionImpersonationStarted = "admin.impersonation_started"
	ActionImpersonationEnded   = "admin.impersonation_ended"
	ActionImpersonationDenied  = "admin.impersonation_denied"
)

var (
	ErrImpersonationDenied = errors.New("impersonation not permitted")
	ErrNotImpersonating    = errors.New("current session is not an impersonation")
)

// ImpersonationInfo is the safe, display-only view of an impersonation that
// /me returns. The banner renders from this and nothing else, so it cannot be
// spoofed or hidden by client state.
type ImpersonationInfo struct {
	ActorEmail string    `json:"actor_email"`
	ActorName  string    `json:"actor_name"`
	StartedAt  time.Time `json:"started_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// StartImpersonation opens a session that acts as an organizer, on behalf of a
// super admin, and returns the token pair for it.
//
// Every rule is checked here, server-side, from the caller's own session row —
// nothing about who is asking or who is targeted is taken from the request
// body beyond the target id, and that is re-validated against the database.
func (s *Service) StartImpersonation(ctx context.Context, auditSvc *audit.Service, callerSessionID, targetID uuid.UUID, reason *string) (*TokenPair, *User, error) {
	deny := func(actorID uuid.UUID, why string) (*TokenPair, *User, error) {
		_ = auditSvc.Record(ctx, audit.Entry{
			ActorUserID: actorID, Action: ActionImpersonationDenied,
			TargetType: "user", TargetID: targetID.String(),
			Diff: map[string]any{"reason": why},
		})
		return nil, nil, ErrImpersonationDenied
	}

	// 1. The caller's session must be a live, NORMAL session. An impersonation
	//    session cannot start another — that is the nesting rule, enforced by
	//    reading the parent row rather than trusting a claim.
	callerSess, err := s.sessions.FindActive(ctx, callerSessionID)
	if err != nil {
		return nil, nil, ErrSessionNotFound
	}
	if callerSess.IsImpersonation() {
		return deny(*callerSess.ActorUserID, "nested impersonation")
	}

	// 2. The caller must be an active super admin RIGHT NOW, not merely when
	//    their token was minted.
	caller, err := s.repo.FindByID(ctx, callerSess.UserID)
	if err != nil || caller.Role != "super_admin" || !caller.IsActive() {
		return deny(callerSess.UserID, "caller is not an active super admin")
	}

	// 3. Not yourself.
	if targetID == caller.ID {
		return deny(caller.ID, "self")
	}

	// 4. Target exists, is an organizer (never another super admin), and is
	//    active. Suspended organizers must be reactivated first — there is no
	//    recovery exception in this version.
	target, err := s.repo.FindByID(ctx, targetID)
	if err != nil {
		return deny(caller.ID, "target not found")
	}
	if target.Role != "organizer" {
		return deny(caller.ID, "target is not an organizer")
	}
	if !target.IsActive() {
		return deny(caller.ID, "target is suspended")
	}

	// 5. No second concurrent impersonation from the same admin.
	var live int
	if err := s.repo.pool.QueryRow(ctx, `
		SELECT count(*) FROM auth_sessions
		WHERE actor_user_id = $1 AND kind = 'impersonation'
		  AND revoked_at IS NULL AND expires_at > now()`, caller.ID).Scan(&live); err != nil {
		return nil, nil, err
	}
	if live > 0 {
		return deny(caller.ID, "an impersonation is already active")
	}

	// Open the child session. The parent stays alive, untouched, server-side.
	rawRefresh, refreshHash, err := NewRefreshToken()
	if err != nil {
		return nil, nil, err
	}
	sess, err := s.sessions.Create(ctx, nil, CreateParams{
		UserID:          target.ID,
		ActorUserID:     &caller.ID,
		ParentSessionID: &callerSess.ID,
		Kind:            SessionImpersonation,
		RefreshHash:     refreshHash,
		ExpiresAt:       time.Now().Add(ImpersonationTTL),
		Reason:          reason,
	})
	if err != nil {
		return nil, nil, err
	}

	_ = auditSvc.Record(ctx, audit.Entry{
		ActorUserID: caller.ID, EffectiveUserID: &target.ID, ImpersonationSessionID: &sess.ID,
		Reason: reason, OrgID: target.OrgID,
		Action: ActionImpersonationStarted, TargetType: "user", TargetID: target.ID.String(),
	})

	access, err := s.tokens.IssueAccess(target.ID, target.Role, target.OrgID, sess.ID)
	if err != nil {
		return nil, nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: rawRefresh}, target, nil
}

// ExitImpersonation ends an impersonation session and re-issues tokens for the
// parent super-admin session.
//
// Authorized by the SESSION, not by role: the caller's effective role is
// organizer, so RequireSuperAdmin would (correctly) refuse them. What proves
// the right to exit is that the current session row is an impersonation with
// a parent — a fact read from the database, never from the browser.
func (s *Service) ExitImpersonation(ctx context.Context, auditSvc *audit.Service, currentSessionID uuid.UUID) (*TokenPair, *User, error) {
	cur, err := s.sessions.FindActive(ctx, currentSessionID)
	if err != nil {
		return nil, nil, ErrSessionNotFound
	}
	if !cur.IsImpersonation() || cur.ParentSessionID == nil {
		return nil, nil, ErrNotImpersonating
	}

	// The parent must still be usable and still belong to an active super
	// admin. If either stopped being true mid-impersonation — suspended,
	// demoted, signed out elsewhere — there is nothing safe to return to, and
	// the only correct outcome is a full sign-out.
	parent, err := s.sessions.FindActive(ctx, *cur.ParentSessionID)
	if err != nil {
		_ = s.sessions.Revoke(ctx, nil, cur.ID, RevokeImpersonation)
		return nil, nil, ErrSessionNotFound
	}
	admin, err := s.repo.FindByID(ctx, parent.UserID)
	if err != nil || admin.Role != "super_admin" || !admin.IsActive() {
		_ = s.sessions.Revoke(ctx, nil, cur.ID, RevokeImpersonation)
		return nil, nil, ErrSessionNotFound
	}

	// Revoke ONLY the impersonation. This is the whole difference between
	// "exit" and "sign out".
	if err := s.sessions.Revoke(ctx, nil, cur.ID, RevokeImpersonation); err != nil {
		return nil, nil, err
	}

	_ = auditSvc.Record(ctx, audit.Entry{
		ActorUserID: admin.ID, EffectiveUserID: &cur.UserID, ImpersonationSessionID: &cur.ID,
		Action: ActionImpersonationEnded, TargetType: "user", TargetID: cur.UserID.String(),
	})

	// Fresh tokens for the parent. The parent's refresh token is rotated
	// rather than re-issued, so the pre-impersonation refresh cookie (which the
	// browser no longer holds anyway) is dead too.
	rawRefresh, refreshHash, err := NewRefreshToken()
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.repo.pool.Exec(ctx, `
		UPDATE auth_sessions
		SET previous_refresh_token_hash = refresh_token_hash,
		    refresh_token_hash = $2, last_seen_at = now()
		WHERE id = $1`, parent.ID, refreshHash); err != nil {
		return nil, nil, err
	}
	access, err := s.tokens.IssueAccess(admin.ID, admin.Role, admin.OrgID, parent.ID)
	if err != nil {
		return nil, nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: rawRefresh}, admin, nil
}

// ImpersonationFor returns the display block for /me, or nil for a normal
// session.
func (s *Service) ImpersonationFor(ctx context.Context, sessionID uuid.UUID) (*ImpersonationInfo, error) {
	sess, err := s.sessions.FindActive(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !sess.IsImpersonation() {
		return nil, nil
	}
	actor, err := s.repo.FindByID(ctx, *sess.ActorUserID)
	if err != nil {
		return nil, err
	}
	var started time.Time
	_ = s.repo.pool.QueryRow(ctx, `SELECT created_at FROM auth_sessions WHERE id = $1`, sess.ID).Scan(&started)
	return &ImpersonationInfo{
		ActorEmail: actor.Email,
		ActorName:  actor.Name,
		StartedAt:  started,
		ExpiresAt:  sess.ExpiresAt,
	}, nil
}

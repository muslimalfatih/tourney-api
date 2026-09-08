package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidCredentials is returned for any failed login, whether the email is
// unknown or the password is wrong. Callers must not distinguish the two, to
// avoid leaking which emails have accounts.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrAccountSuspended is returned when the account exists and the credential is
// correct, but the account has been suspended. Distinct from
// ErrInvalidCredentials on purpose: telling a suspended organizer to contact an
// admin is useful, and reveals nothing an attacker could not learn by being
// suspended themselves.
var ErrAccountSuspended = errors.New("account suspended")

// ErrPasswordLoginDisabled is returned when AUTH_PASSWORD_LOGIN_ENABLED is off.
// This is a deployment state, not a secret.
var ErrPasswordLoginDisabled = errors.New("password login is disabled")

// TokenPair is the result of a successful login or refresh.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
}

// Service holds the auth business logic: credential checks, session lifecycle
// and token issuance.
type Service struct {
	repo                 *Repository
	tokens               *TokenService
	sessions             *SessionRepository
	passwordLoginEnabled bool
}

func NewService(repo *Repository, tokens *TokenService, sessions *SessionRepository, passwordLoginEnabled bool) *Service {
	return &Service{
		repo:                 repo,
		tokens:               tokens,
		sessions:             sessions,
		passwordLoginEnabled: passwordLoginEnabled,
	}
}

// Login verifies credentials and issues a fresh token pair.
func (s *Service) Login(ctx context.Context, email, password string) (*TokenPair, *User, error) {
	if !s.passwordLoginEnabled {
		return nil, nil, ErrPasswordLoginDisabled
	}

	user, err := s.repo.FindByEmail(ctx, email)
	if errors.Is(err, ErrUserNotFound) {
		return nil, nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, nil, err
	}

	// An OTP-only user has no password. This must be indistinguishable from a
	// wrong password: reporting "this account has no password" would turn the
	// login form into an oracle for which addresses hold accounts.
	if user.PasswordHash == nil {
		return nil, nil, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(password, *user.PasswordHash)
	if err != nil || !ok {
		return nil, nil, ErrInvalidCredentials
	}

	// Checked only AFTER the password verifies, so the suspension message is
	// never a way to probe which addresses exist.
	if !user.IsActive() {
		return nil, nil, ErrAccountSuspended
	}

	pair, err := s.openSession(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	s.repo.TouchLastLogin(ctx, user.ID)
	return pair, user, nil
}

// SuspendUser marks an account suspended and tears down its access in the same
// breath. Setting the flag alone would leave every live session working until
// its token expired, which is not what "suspended" means to the person who
// clicked it.
//
// RevokeAllForUser also catches impersonation sessions this user STARTED, so
// suspending a super admin cannot leave them operating under a borrowed
// identity.
func (s *Service) SuspendUser(ctx context.Context, userID uuid.UUID, reason string) error {
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE users SET status = 'suspended' WHERE id = $1`, userID); err != nil {
		return err
	}
	if err := s.sessions.RevokeAllForUser(ctx, tx, userID, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeInvitation withdraws someone's right to be here, and closes the door
// behind them in the same transaction.
//
// Revoking the invitation alone would only stop FUTURE sign-ins while every
// current session kept working — so the person you just removed keeps their
// access until a token happens to expire. Both halves belong together.
func (s *Service) RevokeInvitation(ctx context.Context, invitations *InvitationRepository, invitationID uuid.UUID, reason string) error {
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inv, err := invitations.FindByID(ctx, invitationID)
	if err != nil {
		return err
	}
	if err := invitations.Revoke(ctx, tx, invitationID); err != nil {
		return err
	}

	// The invitation may never have been accepted, in which case there is no
	// account and nothing further to do.
	user, err := s.repo.FindByEmail(ctx, inv.Email)
	if errors.Is(err, ErrUserNotFound) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if err := s.sessions.RevokeAllForUser(ctx, tx, user.ID, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReactivateUser clears suspension. It does NOT restore sessions: the ones
// revoked on suspension stay revoked, and the user signs in again.
func (s *Service) ReactivateUser(ctx context.Context, userID uuid.UUID) error {
	_, err := s.repo.pool.Exec(ctx,
		`UPDATE users SET status = 'active' WHERE id = $1`, userID)
	return err
}

// Refresh exchanges an opaque refresh token for a new pair, rotating the stored
// token so the presented one dies immediately.
//
// Every failure returns ErrInvalidCredentials so a caller cannot tell an unknown
// token from a revoked session from a replayed one. The distinction is recorded
// server-side (a replay revokes the session) but never disclosed.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	sess, newRefresh, err := s.sessions.Rotate(ctx, refreshToken)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	user, err := s.repo.FindByID(ctx, sess.UserID)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	access, err := s.tokens.IssueAccess(user.ID, user.Role, user.OrgID, sess.ID)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: newRefresh}, nil
}

// Logout revokes a session, and any impersonation sessions opened from it.
//
// Signing out of a normal session takes its children with it: a super admin who
// signs out while impersonating must not leave the borrowed identity alive.
// Exiting impersonation is a different operation (5.5) that revokes only the
// child.
func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	if sessionID == uuid.Nil {
		return nil
	}
	if err := s.sessions.RevokeChildren(ctx, nil, sessionID, RevokeParentLogout); err != nil {
		return err
	}
	return s.sessions.Revoke(ctx, nil, sessionID, RevokeLogout)
}

// LogoutByRefreshToken revokes the session a refresh token belongs to. The web
// app holds the refresh cookie and may have let its 15-minute access token
// lapse, so sign-out must not depend on a live access token.
func (s *Service) LogoutByRefreshToken(ctx context.Context, refreshToken string) error {
	sess, err := s.sessions.FindByRefreshToken(ctx, refreshToken)
	if err != nil {
		return err
	}
	return s.Logout(ctx, sess.ID)
}

// Me returns the current user by id.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (*User, error) {
	return s.repo.FindByID(ctx, userID)
}

// openSession creates a normal session row and the token pair that references
// it. The session is created FIRST so its id can go into the access token's sid
// claim — the token is meaningless without a session to resolve.
func (s *Service) openSession(ctx context.Context, user *User) (*TokenPair, error) {
	rawRefresh, refreshHash, err := NewRefreshToken()
	if err != nil {
		return nil, err
	}
	sess, err := s.sessions.Create(ctx, nil, CreateParams{
		UserID:      user.ID,
		Kind:        SessionNormal,
		RefreshHash: refreshHash,
		ExpiresAt:   time.Now().Add(s.tokens.RefreshTTL()),
	})
	if err != nil {
		return nil, err
	}
	access, err := s.tokens.IssueAccess(user.ID, user.Role, user.OrgID, sess.ID)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: rawRefresh}, nil
}

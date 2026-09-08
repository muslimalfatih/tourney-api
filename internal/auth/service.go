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

// TokenPair is the result of a successful login or refresh.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
}

// Service holds the auth business logic: credential checks, session lifecycle
// and token issuance.
type Service struct {
	repo     *Repository
	tokens   *TokenService
	sessions *SessionRepository
}

func NewService(repo *Repository, tokens *TokenService, sessions *SessionRepository) *Service {
	return &Service{repo: repo, tokens: tokens, sessions: sessions}
}

// Login verifies credentials and issues a fresh token pair.
func (s *Service) Login(ctx context.Context, email, password string) (*TokenPair, *User, error) {
	user, err := s.repo.FindByEmail(ctx, email)
	if errors.Is(err, ErrUserNotFound) {
		return nil, nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, nil, err
	}

	ok, err := VerifyPassword(password, user.PasswordHash)
	if err != nil || !ok {
		return nil, nil, ErrInvalidCredentials
	}

	pair, err := s.openSession(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	return pair, user, nil
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

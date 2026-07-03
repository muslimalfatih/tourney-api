package auth

import (
	"context"
	"errors"

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

// Service holds the auth business logic: credential checks and token issuance.
type Service struct {
	repo   *Repository
	tokens *TokenService
}

func NewService(repo *Repository, tokens *TokenService) *Service {
	return &Service{repo: repo, tokens: tokens}
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

	pair, err := s.issue(user)
	if err != nil {
		return nil, nil, err
	}
	return pair, user, nil
}

// Refresh mints a new token pair from a valid refresh token.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	userID, err := s.tokens.VerifyRefreshToken(refreshToken)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	user, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	return s.issue(user)
}

// Me returns the current user by id.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (*User, error) {
	return s.repo.FindByID(ctx, userID)
}

func (s *Service) issue(user *User) (*TokenPair, error) {
	access, err := s.tokens.IssueAccess(user.ID, user.Role, user.OrgID)
	if err != nil {
		return nil, err
	}
	refresh, err := s.tokens.IssueRefresh(user.ID)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: refresh}, nil
}

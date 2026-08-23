package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

// TokenService issues and verifies JWT access and refresh tokens. Access tokens
// are short-lived and carry the caller's identity; refresh tokens are long-lived
// and carry only the subject, used to mint a new access token.
type TokenService struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func NewTokenService(secret string, accessTTL, refreshTTL time.Duration) *TokenService {
	return &TokenService{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}
}

// accessClaims is the JWT payload for access tokens.
type accessClaims struct {
	Role  string  `json:"role"`
	OrgID *string `json:"org_id,omitempty"`
	jwt.RegisteredClaims
}

// IssueAccess mints a signed access token for a user.
func (s *TokenService) IssueAccess(userID uuid.UUID, role string, orgID *uuid.UUID) (string, error) {
	var org *string
	if orgID != nil {
		v := orgID.String()
		org = &v
	}
	now := time.Now()
	claims := accessClaims{
		Role:  role,
		OrgID: org,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// IssueRefresh mints a signed refresh token carrying only the subject.
func (s *TokenService) IssueRefresh(userID uuid.UUID) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   userID.String(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.refreshTTL)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// VerifyAccessToken implements middleware.TokenVerifier.
func (s *TokenService) VerifyAccessToken(raw string) (*middleware.Claims, error) {
	var claims accessClaims
	if err := s.parse(raw, &claims); err != nil {
		return nil, err
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("invalid subject: %w", err)
	}
	out := &middleware.Claims{UserID: userID, Role: claims.Role}
	if claims.OrgID != nil {
		orgID, err := uuid.Parse(*claims.OrgID)
		if err != nil {
			return nil, fmt.Errorf("invalid org_id: %w", err)
		}
		out.OrgID = &orgID
	}
	return out, nil
}

// VerifyRefreshToken validates a refresh token and returns its subject.
func (s *TokenService) VerifyRefreshToken(raw string) (uuid.UUID, error) {
	var claims jwt.RegisteredClaims
	if err := s.parse(raw, &claims); err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(claims.Subject)
}

func (s *TokenService) parse(raw string, claims jwt.Claims) error {
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	return err
}

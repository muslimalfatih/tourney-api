package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

// TokenService issues and verifies JWT access tokens. Refresh tokens are NOT
// JWTs since 00013 — they are opaque random values stored hashed against an
// auth_sessions row (see session.go), so they can be revoked and rotated.
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
	// SID names the auth_sessions row this token belongs to. Without it a
	// token cannot be tied to a revocable session, so it is required.
	SID string `json:"sid"`
	jwt.RegisteredClaims
}

// RefreshTTL is how long a new session stays valid before it must be
// re-established by signing in again.
func (s *TokenService) RefreshTTL() time.Duration { return s.refreshTTL }

// IssueAccess mints a signed access token for a user.
func (s *TokenService) IssueAccess(userID uuid.UUID, role string, orgID *uuid.UUID, sessionID uuid.UUID) (string, error) {
	var org *string
	if orgID != nil {
		v := orgID.String()
		org = &v
	}
	now := time.Now()
	claims := accessClaims{
		Role:  role,
		OrgID: org,
		SID:   sessionID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// VerifyAccessToken parses and checks the JWT ONLY. It deliberately does not
// touch the database: auth.SessionVerifier layers the session lookup on top,
// which keeps this unit pure and fast to test.
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
	if claims.SID != "" {
		sid, err := uuid.Parse(claims.SID)
		if err != nil {
			return nil, fmt.Errorf("invalid sid: %w", err)
		}
		out.SessionID = sid
	}
	if claims.OrgID != nil {
		orgID, err := uuid.Parse(*claims.OrgID)
		if err != nil {
			return nil, fmt.Errorf("invalid org_id: %w", err)
		}
		out.OrgID = &orgID
	}
	return out, nil
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

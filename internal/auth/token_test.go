package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestTokens() *TokenService {
	return NewTokenService("a-test-secret-at-least-16-chars", 15*time.Minute, 720*time.Hour)
}

func TestAccessToken_RoundTrip(t *testing.T) {
	ts := newTestTokens()
	userID := uuid.New()
	orgID := uuid.New()

	tok, err := ts.IssueAccess(userID, "organizer", &orgID)
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	claims, err := ts.VerifyAccessToken(tok)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if claims.UserID != userID {
		t.Errorf("UserID = %v, want %v", claims.UserID, userID)
	}
	if claims.Role != "organizer" {
		t.Errorf("Role = %q, want organizer", claims.Role)
	}
	if claims.OrgID == nil || *claims.OrgID != orgID {
		t.Errorf("OrgID = %v, want %v", claims.OrgID, orgID)
	}
}

func TestAccessToken_SuperAdminNoOrg(t *testing.T) {
	ts := newTestTokens()
	tok, _ := ts.IssueAccess(uuid.New(), "super_admin", nil)
	claims, err := ts.VerifyAccessToken(tok)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if claims.OrgID != nil {
		t.Errorf("super admin OrgID should be nil, got %v", claims.OrgID)
	}
}

func TestAccessToken_RejectsWrongSecret(t *testing.T) {
	issuer := newTestTokens()
	verifier := NewTokenService("a-DIFFERENT-secret-16chars+", 15*time.Minute, 720*time.Hour)

	tok, _ := issuer.IssueAccess(uuid.New(), "organizer", nil)
	if _, err := verifier.VerifyAccessToken(tok); err == nil {
		t.Error("token signed with a different secret should not verify")
	}
}

func TestAccessToken_RejectsExpired(t *testing.T) {
	// Zero TTL → immediately expired.
	ts := NewTokenService("a-test-secret-at-least-16-chars", -time.Minute, time.Hour)
	tok, _ := ts.IssueAccess(uuid.New(), "organizer", nil)
	if _, err := ts.VerifyAccessToken(tok); err == nil {
		t.Error("expired token should not verify")
	}
}

func TestAccessToken_RejectsGarbage(t *testing.T) {
	ts := newTestTokens()
	if _, err := ts.VerifyAccessToken("not.a.jwt"); err == nil {
		t.Error("garbage token should not verify")
	}
}

func TestRefreshToken_RoundTrip(t *testing.T) {
	ts := newTestTokens()
	userID := uuid.New()
	tok, err := ts.IssueRefresh(userID)
	if err != nil {
		t.Fatalf("IssueRefresh: %v", err)
	}
	got, err := ts.VerifyRefreshToken(tok)
	if err != nil {
		t.Fatalf("VerifyRefreshToken: %v", err)
	}
	if got != userID {
		t.Errorf("subject = %v, want %v", got, userID)
	}
}

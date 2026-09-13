//go:build integration

package inttest

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/auth"
)

// Subphase 5.2: the allowlist and the account status that gate every sign-in.

func invitationRepo(e *env) *auth.InvitationRepository { return auth.NewInvitationRepository(e.pool) }

// uniqueEmail keeps runs independent — invitations are unique per address and
// revoked rows are kept forever, so reusing one would collide across runs.
func uniqueEmail(prefix string) string {
	return fmt.Sprintf("itest-%s-%s@tourney.test", prefix, uuid.NewString()[:8])
}

func orgID(t *testing.T, e *env) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM organizations WHERE slug = $1`, orgSlug).Scan(&id); err != nil {
		t.Fatalf("load itest org: %v", err)
	}
	return id
}

func TestInvitation_AllowlistGate(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := invitationRepo(e)
	org := orgID(t, e)

	// Not invited at all.
	if _, err := repo.FindActiveByEmail(ctx, uniqueEmail("stranger")); err != auth.ErrNotInvited {
		t.Errorf("uninvited lookup = %v, want ErrNotInvited", err)
	}

	// Invited: found regardless of how the address is capitalised.
	email := uniqueEmail("invited")
	if _, err := repo.Create(ctx, nil, auth.CreateInvitationParams{
		Email: email, Role: "organizer", Organization: &org,
	}); err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	for _, variant := range []string{email, "  " + email + "  "} {
		if _, err := repo.FindActiveByEmail(ctx, variant); err != nil {
			t.Errorf("lookup %q: %v, want found", variant, err)
		}
	}
}

func TestInvitation_ExpiredPendingIsRejected(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := invitationRepo(e)
	org := orgID(t, e)

	email := uniqueEmail("expired")
	if _, err := repo.Create(ctx, nil, auth.CreateInvitationParams{
		Email: email, Role: "organizer", Organization: &org, TTL: -time.Hour,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.FindActiveByEmail(ctx, email); err != auth.ErrInvitationExpired {
		t.Errorf("expired pending = %v, want ErrInvitationExpired", err)
	}
}

func TestInvitation_AcceptedIgnoresExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := invitationRepo(e)
	org := orgID(t, e)

	email := uniqueEmail("accepted")
	inv, err := repo.Create(ctx, nil, auth.CreateInvitationParams{
		Email: email, Role: "organizer", Organization: &org, TTL: -time.Hour,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.MarkAccepted(ctx, nil, inv.ID); err != nil {
		t.Fatalf("mark accepted: %v", err)
	}
	// The expiry is in the past, but the account is in use. Locking that person
	// out would be a bug, not a security control.
	if _, err := repo.FindActiveByEmail(ctx, email); err != nil {
		t.Errorf("accepted-but-expired = %v, want active", err)
	}
}

func TestInvitation_RevokedIsTerminalAndReInviteMakesNewRow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := invitationRepo(e)
	org := orgID(t, e)

	email := uniqueEmail("revoked")
	first, err := repo.Create(ctx, nil, auth.CreateInvitationParams{
		Email: email, Role: "organizer", Organization: &org,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A second ACTIVE invitation for one address must be impossible.
	if _, err := repo.Create(ctx, nil, auth.CreateInvitationParams{
		Email: email, Role: "organizer", Organization: &org,
	}); err != auth.ErrInvitationExists {
		t.Errorf("duplicate active invitation = %v, want ErrInvitationExists", err)
	}

	if err := repo.Revoke(ctx, nil, first.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := repo.FindActiveByEmail(ctx, email); err != auth.ErrNotInvited {
		t.Errorf("after revoke = %v, want ErrNotInvited", err)
	}

	// Re-inviting must create a NEW row and leave the revoked one intact, so
	// the record of who was removed survives.
	second, err := repo.Create(ctx, nil, auth.CreateInvitationParams{
		Email: email, Role: "organizer", Organization: &org,
	})
	if err != nil {
		t.Fatalf("re-invite: %v", err)
	}
	if second.ID == first.ID {
		t.Error("re-invite reused the revoked row; history was overwritten")
	}
	var revokedStillThere int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM invitations WHERE email = $1 AND status = 'revoked'`,
		email).Scan(&revokedStillThere); err != nil {
		t.Fatalf("count revoked: %v", err)
	}
	if revokedStillThere != 1 {
		t.Errorf("revoked history rows = %d, want 1", revokedStillThere)
	}
}

func TestInvitation_OrgScopeIsEnforcedByDatabase(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := invitationRepo(e)
	org := orgID(t, e)

	// An organizer with no organization would later fail at OTP verify with a
	// users_org_scope violation, stranding someone who did nothing wrong. The
	// CHECK stops it at the invitation instead.
	if _, err := repo.Create(ctx, nil, auth.CreateInvitationParams{
		Email: uniqueEmail("no-org"), Role: "organizer", Organization: nil,
	}); err == nil {
		t.Error("organizer invitation without an organization was accepted")
	}
	// And a super admin must not be pinned to one.
	if _, err := repo.Create(ctx, nil, auth.CreateInvitationParams{
		Email: uniqueEmail("admin-org"), Role: "super_admin", Organization: &org,
	}); err == nil {
		t.Error("super_admin invitation with an organization was accepted")
	}
}

func TestUserStatus_SuspensionRevokesLiveSessions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	access, refresh := loginPair(t, e, organizerEmail)

	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusOK {
		t.Fatalf("/me before suspension: %d", st)
	}

	var uid uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, organizerEmail).Scan(&uid); err != nil {
		t.Fatalf("load user: %v", err)
	}
	svc := auth.NewService(auth.NewRepository(e.pool), nil, auth.NewSessionRepository(e.pool), true)
	if err := svc.SuspendUser(ctx, uid, "itest"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	t.Cleanup(func() { _ = svc.ReactivateUser(context.Background(), uid) })

	// Suspension is meaningless if the person keeps working until their token
	// expires. Both credentials must die at once.
	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusUnauthorized {
		t.Errorf("/me after suspension: %d, want 401", st)
	}
	if st, _ := e.call(t, "POST", "/auth/refresh", map[string]any{"refresh_token": refresh}, ""); st != http.StatusUnauthorized {
		t.Errorf("refresh after suspension: %d, want 401", st)
	}
}

func TestUserStatus_SuspendedCannotLogIn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	var uid uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, organizerEmail).Scan(&uid); err != nil {
		t.Fatalf("load user: %v", err)
	}
	svc := auth.NewService(auth.NewRepository(e.pool), nil, auth.NewSessionRepository(e.pool), true)
	if err := svc.SuspendUser(ctx, uid, "itest"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	t.Cleanup(func() { _ = svc.ReactivateUser(context.Background(), uid) })

	st, res := e.call(t, "POST", "/auth/login", map[string]any{
		"email": organizerEmail, "password": password,
	}, "")
	if st != http.StatusForbidden {
		t.Fatalf("suspended login: %d, want 403 (body %v)", st, res)
	}
	errObj, _ := res["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "account_suspended" {
		t.Errorf("error code = %q, want account_suspended", code)
	}
}

func TestUserStatus_EmailIsCaseInsensitiveOnLogin(t *testing.T) {
	e := setup(t)
	// The allowlist is keyed by normalized email; login must agree, or an
	// invited person is refused for capitalising their own address.
	upper := ""
	for _, r := range organizerEmail {
		if r >= 'a' && r <= 'z' {
			upper += string(r - 32)
		} else {
			upper += string(r)
		}
	}
	if st, _ := e.call(t, "POST", "/auth/login", map[string]any{
		"email": upper, "password": password,
	}, ""); st != http.StatusOK {
		t.Errorf("login with %q: %d, want 200", upper, st)
	}
}

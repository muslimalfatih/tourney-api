//go:build integration

package inttest

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Subphase 5.5a: platform controls. The security matrix already proves
// organizer → 403 and anonymous → 401 on the admin group; these cover what
// super admins can do and the invariants that stop them hurting themselves.

func userIDByEmail(t *testing.T, e *env, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE lower(email) = lower($1)`, email).Scan(&id); err != nil {
		t.Fatalf("load %s: %v", email, err)
	}
	return id
}

func TestAdmin_BootstrapSuperAdminExists(t *testing.T) {
	e := setup(t)
	var role, status string
	var org *uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT role::text, status::text, org_id FROM users WHERE lower(email) = 'kgobang570@gmail.com'`).
		Scan(&role, &status, &org); err != nil {
		t.Fatalf("bootstrap user missing: %v", err)
	}
	if role != "super_admin" || status != "active" || org != nil {
		t.Errorf("bootstrap = role %s status %s org %v, want super_admin/active/nil", role, status, org)
	}
}

func TestAdmin_InvitationLifecycleViaAPI(t *testing.T) {
	e := setup(t)
	adminTok, _ := loginPair(t, e, adminEmail)
	org := orgID(t, e)
	addr := uniqueEmail("adm-inv")

	// Create → an invitation email goes out.
	st, res := e.call(t, "POST", "/admin/invitations", map[string]any{
		"email": addr, "role": "organizer", "organization_id": org.String(), "note": "itest",
	}, adminTok)
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, res)
	}
	data, _ := res["data"].(map[string]any)
	invID, _ := data["id"].(string)
	if invID == "" {
		t.Fatal("no invitation id returned")
	}
	sentInvite := false
	for _, m := range e.mail.Sent() {
		if m.Kind == "invitation" && m.To == addr {
			sentInvite = true
		}
	}
	if !sentInvite {
		t.Error("no invitation email was sent")
	}

	// Organizer invitation without an org is refused at the API too.
	if st, _ := e.call(t, "POST", "/admin/invitations", map[string]any{
		"email": uniqueEmail("adm-noorg"), "role": "organizer",
	}, adminTok); st != http.StatusUnprocessableEntity {
		t.Errorf("organizer without org: %d, want 422", st)
	}

	// Duplicate active invitation → 409.
	if st, _ := e.call(t, "POST", "/admin/invitations", map[string]any{
		"email": addr, "role": "organizer", "organization_id": org.String(),
	}, adminTok); st != http.StatusConflict {
		t.Errorf("duplicate: %d, want 409", st)
	}

	// Resend.
	if st, _ := e.call(t, "POST", "/admin/invitations/"+invID+"/resend", nil, adminTok); st != http.StatusOK {
		t.Errorf("resend: %d, want 200", st)
	}

	// The invited person signs in, then gets revoked: their session must die.
	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	st, res = e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": e.mail.LastCodeFor(addr)}, "")
	if st != http.StatusOK {
		t.Fatalf("invitee sign-in: %d %v", st, res)
	}
	inviteeTok, _ := res["data"].(map[string]any)["access_token"].(string)

	if st, _ := e.call(t, "PATCH", "/admin/invitations/"+invID, map[string]any{
		"status": "revoked", "reason": "itest",
	}, adminTok); st != http.StatusOK {
		t.Fatalf("revoke: %d", st)
	}
	if st, _ := e.call(t, "GET", "/me", nil, inviteeTok); st != http.StatusUnauthorized {
		t.Errorf("invitee /me after revoke: %d, want 401", st)
	}
	if st, _ := e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, ""); st != http.StatusForbidden {
		t.Errorf("revoked invitee request: %d, want 403", st)
	}

	// Audit rows exist for create + revoke, with the admin as actor.
	var n int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_logs
		WHERE target_id = $1 AND action IN ('admin.invitation_created','admin.invitation_revoked')
		  AND actor_user_id = $2`, invID, userIDByEmail(t, e, adminEmail)).Scan(&n); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if n != 2 {
		t.Errorf("audit rows for invitation = %d, want 2", n)
	}
}

func TestAdmin_LastSuperAdminCannotBeRemoved(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminTok, _ := loginPair(t, e, adminEmail)
	adminID := userIDByEmail(t, e, adminEmail)

	// Make the itest admin the ONLY active super admin by suspending the
	// bootstrap one for the duration of the test.
	bootID := userIDByEmail(t, e, "kgobang570@gmail.com")
	if _, err := e.pool.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, bootID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `UPDATE users SET status = 'active' WHERE id = $1`, bootID)
	})

	// Self-demotion refused.
	st, res := e.call(t, "PATCH", "/admin/users/"+adminID.String(), map[string]any{
		"role": "organizer", "organization_id": orgID(t, e).String(), "reason": "itest",
	}, adminTok)
	if st != http.StatusConflict || errCode(res) != "last_super_admin" {
		t.Errorf("self-demote: %d %q, want 409 last_super_admin", st, errCode(res))
	}
	// Self-suspension refused.
	st, res = e.call(t, "PATCH", "/admin/users/"+adminID.String(), map[string]any{
		"status": "suspended", "reason": "itest",
	}, adminTok)
	if st != http.StatusConflict || errCode(res) != "last_super_admin" {
		t.Errorf("self-suspend: %d %q, want 409 last_super_admin", st, errCode(res))
	}
	// And the admin is still able to act.
	if st, _ := e.call(t, "GET", "/admin/overview", nil, adminTok); st != http.StatusOK {
		t.Errorf("admin still works: %d, want 200", st)
	}
}

func TestAdmin_RoleChangeIsAtomicWithOrgAndAudited(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminTok, _ := loginPair(t, e, adminEmail)
	org := orgID(t, e)

	// A fresh organizer to promote and demote.
	addr := invite(t, e, "adm-role")
	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": e.mail.LastCodeFor(addr)}, "")
	uid := userIDByEmail(t, e, addr)

	// Promote: org must become NULL or users_org_scope rejects it.
	st, res := e.call(t, "PATCH", "/admin/users/"+uid.String(), map[string]any{
		"role": "super_admin", "reason": "itest promote",
	}, adminTok)
	if st != http.StatusOK {
		t.Fatalf("promote: %d %v", st, res)
	}
	var role string
	var orgAfter *uuid.UUID
	_ = e.pool.QueryRow(ctx, `SELECT role::text, org_id FROM users WHERE id = $1`, uid).Scan(&role, &orgAfter)
	if role != "super_admin" || orgAfter != nil {
		t.Errorf("after promote: role=%s org=%v, want super_admin/nil", role, orgAfter)
	}

	// Demote without an org → 422; with one → ok.
	if st, _ := e.call(t, "PATCH", "/admin/users/"+uid.String(), map[string]any{
		"role": "organizer", "reason": "itest",
	}, adminTok); st != http.StatusUnprocessableEntity {
		t.Errorf("demote without org: %d, want 422", st)
	}
	if st, _ := e.call(t, "PATCH", "/admin/users/"+uid.String(), map[string]any{
		"role": "organizer", "organization_id": org.String(), "reason": "itest demote",
	}, adminTok); st != http.StatusOK {
		t.Errorf("demote with org: %d, want 200", st)
	}

	var audited int
	_ = e.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_logs
		WHERE action = 'admin.user_role_changed' AND target_id = $1 AND reason IS NOT NULL`, uid.String()).Scan(&audited)
	if audited != 2 {
		t.Errorf("role-change audit rows = %d, want 2 (each with a reason)", audited)
	}
}

func TestAdmin_ForceUnpublishClosesPublicReadAndSSE(t *testing.T) {
	e := setup(t)
	adminTok, _ := loginPair(t, e, adminEmail)
	orgTok, _ := loginPair(t, e, organizerEmail)

	f := makeFixture(t, e, orgTok, "itest-force-"+uuid.NewString()[:6])
	if st, _ := e.call(t, "POST", "/tournaments/"+f.tournamentID+"/publish", nil, orgTok); st != http.StatusOK {
		t.Fatalf("publish: %d", st)
	}
	if st, _ := e.call(t, "GET", "/public/tournaments/"+f.slug, nil, ""); st != http.StatusOK {
		t.Fatalf("public read while published: %d", st)
	}

	// Reason is mandatory.
	if st, _ := e.call(t, "POST", "/admin/tournaments/"+f.tournamentID+"/status",
		map[string]any{"action": "unpublish"}, adminTok); st != http.StatusUnprocessableEntity {
		t.Errorf("unpublish without reason: %d, want 422", st)
	}

	if st, res := e.call(t, "POST", "/admin/tournaments/"+f.tournamentID+"/status",
		map[string]any{"action": "unpublish", "reason": "itest force"}, adminTok); st != http.StatusOK {
		t.Fatalf("force unpublish: %d %v", st, res)
	}

	// Every public surface closes at once through the one status gate.
	if st, _ := e.call(t, "GET", "/public/tournaments/"+f.slug, nil, ""); st != http.StatusNotFound {
		t.Errorf("public read after force unpublish: %d, want 404", st)
	}
	if st, _ := e.call(t, "GET", "/public/matches/"+f.matchID, nil, ""); st != http.StatusNotFound {
		t.Errorf("public match after force unpublish: %d, want 404", st)
	}
	if _, st := streamEvents(context.Background(), t, e, f.slug); st != http.StatusNotFound {
		t.Errorf("SSE after force unpublish: %d, want 404", st)
	}

	var reason *string
	_ = e.pool.QueryRow(context.Background(), `
		SELECT reason FROM audit_logs
		WHERE action = 'admin.tournament_force_unpublished' AND tournament_id = $1
		ORDER BY created_at DESC LIMIT 1`, f.tournamentID).Scan(&reason)
	if reason == nil || *reason != "itest force" {
		t.Errorf("force action audit reason = %v, want 'itest force'", reason)
	}
}

func TestAdmin_OverviewAndSettingsAreReadable(t *testing.T) {
	e := setup(t)
	adminTok, _ := loginPair(t, e, adminEmail)

	st, res := e.call(t, "GET", "/admin/overview", nil, adminTok)
	if st != http.StatusOK {
		t.Fatalf("overview: %d %v", st, res)
	}
	data, _ := res["data"].(map[string]any)
	for _, k := range []string{"organizers", "active_users", "pending_invitations", "tournaments", "live_matches", "recent_audit"} {
		if _, ok := data[k]; !ok {
			t.Errorf("overview missing %q", k)
		}
	}

	st, res = e.call(t, "GET", "/admin/settings", nil, adminTok)
	if st != http.StatusOK {
		t.Fatalf("settings: %d", st)
	}
	data, _ = res["data"].(map[string]any)
	if data["default_timezone"] != "Asia/Makassar" {
		t.Errorf("default_timezone = %v", data["default_timezone"])
	}
	// No secret shape may appear anywhere in the settings payload.
	for k := range data {
		if k == "plunk_api_key" || k == "otp_pepper" || k == "jwt_secret" {
			t.Errorf("settings exposes %q", k)
		}
	}
}

func TestAdmin_PlunkTestSendsOnlyToCaller(t *testing.T) {
	e := setup(t)
	adminTok, _ := loginPair(t, e, adminEmail)
	e.mail.Reset()

	// The itest admin has no invitation row; give them one so the normal OTP
	// path (which the test action deliberately reuses) admits them.
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO invitations (email, role, organization_id, status, accepted_at)
		VALUES ($1, 'super_admin', NULL, 'accepted', now())
		ON CONFLICT (email) WHERE status <> 'revoked' DO NOTHING`, adminEmail); err != nil {
		t.Fatal(err)
	}

	// A recipient in the body must be ignored, not honoured.
	st, res := e.call(t, "POST", "/admin/settings/plunk-test", map[string]any{"to": "victim@example.com"}, adminTok)
	if st != http.StatusOK {
		t.Fatalf("plunk-test: %d %v", st, res)
	}
	for _, m := range e.mail.Sent() {
		if m.To == "victim@example.com" {
			t.Fatal("test email went to a recipient supplied by the browser")
		}
	}
	if e.mail.LastCodeFor(adminEmail) == "" {
		t.Error("no code was sent to the caller's own address")
	}
	var n int
	_ = e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = 'admin.plunk_test_otp_sent'`).Scan(&n)
	if n == 0 {
		t.Error("plunk test was not audited")
	}
}

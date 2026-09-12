//go:build integration

package inttest

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Subphase 5.5a: impersonation. Every rule is server-enforced from session
// rows; nothing here trusts the request body beyond the target id.

// freshOrganizer invites and signs in a new organizer, returning their id and
// access token.
func freshOrganizer(t *testing.T, e *env, prefix string) (uuid.UUID, string) {
	t.Helper()
	addr := invite(t, e, prefix)
	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	st, res := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": e.mail.LastCodeFor(addr)}, "")
	if st != http.StatusOK {
		t.Fatalf("organizer sign-in: %d %v", st, res)
	}
	tok, _ := res["data"].(map[string]any)["access_token"].(string)
	return userIDByEmail(t, e, addr), tok
}

// impersonate starts an impersonation and returns the borrowed token pair.
func impersonate(t *testing.T, e *env, adminTok string, target uuid.UUID) (access, refresh string, st int, res map[string]any) {
	t.Helper()
	st, res = e.call(t, "POST", "/admin/users/"+target.String()+"/impersonate",
		map[string]any{"reason": "itest support"}, adminTok)
	if st == http.StatusOK {
		data, _ := res["data"].(map[string]any)
		access, _ = data["access_token"].(string)
		refresh, _ = data["refresh_token"].(string)
	}
	return
}

func TestImpersonation_HappyPathAndExit(t *testing.T) {
	e := setup(t)
	adminTok, _ := loginPair(t, e, adminEmail)
	adminID := userIDByEmail(t, e, adminEmail)
	target, _ := freshOrganizer(t, e, "imp-target")

	access, _, st, res := impersonate(t, e, adminTok, target)
	if st != http.StatusOK {
		t.Fatalf("impersonate: %d %v", st, res)
	}

	// /me shows the ORGANIZER as the effective identity, with the admin named
	// in the impersonation block. This is what the banner renders from.
	st, res = e.call(t, "GET", "/me", nil, access)
	if st != http.StatusOK {
		t.Fatalf("/me as impersonated: %d", st)
	}
	me, _ := res["data"].(map[string]any)
	if me["id"] != target.String() || me["role"] != "organizer" {
		t.Errorf("effective identity = %v/%v, want target organizer", me["id"], me["role"])
	}
	imp, _ := me["impersonation"].(map[string]any)
	if imp == nil || imp["actor_email"] != adminEmail {
		t.Errorf("impersonation block = %v, want actor %s", imp, adminEmail)
	}

	// Effective permissions are the organizer's: platform routes are closed.
	if st, _ := e.call(t, "GET", "/admin/overview", nil, access); st != http.StatusForbidden {
		t.Errorf("admin route while impersonating: %d, want 403", st)
	}
	// ...and no nesting.
	other, _ := freshOrganizer(t, e, "imp-other")
	if _, _, st, _ := impersonate(t, e, access, other); st != http.StatusForbidden {
		t.Errorf("nested impersonation: %d, want 403", st)
	}

	// A mutation made while impersonating carries BOTH people.
	st, res = e.call(t, "POST", "/tournaments", map[string]any{
		"name": "Imp Cup", "slug": "itest-imp-" + uuid.NewString()[:6], "sport": "tennis",
	}, access)
	if st != http.StatusCreated {
		t.Fatalf("create tournament while impersonating: %d %v", st, res)
	}
	tid, _ := res["data"].(map[string]any)["id"].(string)
	// Publish is an audited organizer mutation. The service passes the
	// EFFECTIVE user as actor; the audit layer must correct that from context.
	if st, res := e.call(t, "POST", "/tournaments/"+tid+"/publish", nil, access); st != http.StatusOK {
		t.Fatalf("publish while impersonating: %d %v", st, res)
	}
	var actor, effective *uuid.UUID
	var sess *uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `
		SELECT actor_user_id, effective_user_id, impersonation_session_id
		FROM audit_logs WHERE tournament_id = $1 AND action LIKE 'tournament.%'
		ORDER BY created_at DESC LIMIT 1`, tid).Scan(&actor, &effective, &sess); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if actor == nil || *actor != adminID {
		t.Errorf("audit actor = %v, want admin %s", actor, adminID)
	}
	if effective == nil || *effective != target {
		t.Errorf("audit effective = %v, want organizer %s", effective, target)
	}
	if sess == nil {
		t.Error("audit row has no impersonation_session_id")
	}

	// Exit: the impersonation dies, the admin's own session comes back, and
	// the admin's ORIGINAL token still works because exit never revoked it.
	st, res = e.call(t, "POST", "/admin/impersonation/exit", nil, access)
	if st != http.StatusOK {
		t.Fatalf("exit: %d %v", st, res)
	}
	restored, _ := res["data"].(map[string]any)["access_token"].(string)
	if st, _ := e.call(t, "GET", "/admin/overview", nil, restored); st != http.StatusOK {
		t.Errorf("admin route with restored token: %d, want 200", st)
	}
	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusUnauthorized {
		t.Errorf("impersonation token after exit: %d, want 401", st)
	}
	if st, _ := e.call(t, "GET", "/admin/overview", nil, adminTok); st != http.StatusOK {
		t.Errorf("original admin token after exit: %d, want 200 (exit must not revoke the parent)", st)
	}

	var started, ended int
	_ = e.pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE action='admin.impersonation_started'),
		        count(*) FILTER (WHERE action='admin.impersonation_ended')
		 FROM audit_logs WHERE target_id = $1`, target.String()).Scan(&started, &ended)
	if started == 0 || ended == 0 {
		t.Errorf("impersonation audit: started=%d ended=%d, want both ≥1", started, ended)
	}
}

func TestImpersonation_DeniedCasesAreAudited(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminTok, _ := loginPair(t, e, adminEmail)
	adminID := userIDByEmail(t, e, adminEmail)
	orgTok, _ := loginPair(t, e, organizerEmail)
	organizerID := userIDByEmail(t, e, organizerEmail)

	var before int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'admin.impersonation_denied'`).Scan(&before)

	cases := []struct {
		name   string
		token  string
		target uuid.UUID
		want   int
	}{
		{"organizer cannot impersonate anyone", orgTok, adminID, http.StatusForbidden},
		{"super admin cannot impersonate self", adminTok, adminID, http.StatusForbidden},
		{"cannot impersonate another super admin", adminTok, userIDByEmail(t, e, "kgobang570@gmail.com"), http.StatusForbidden},
		{"unknown target", adminTok, uuid.New(), http.StatusForbidden},
	}
	for _, c := range cases {
		if _, _, st, _ := impersonate(t, e, c.token, c.target); st != c.want {
			t.Errorf("%s: %d, want %d", c.name, st, c.want)
		}
	}

	// Suspended organizer: reactivate first, no exception.
	if _, err := e.pool.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, organizerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `UPDATE users SET status = 'active' WHERE id = $1`, organizerID)
	})
	if _, _, st, _ := impersonate(t, e, adminTok, organizerID); st != http.StatusForbidden {
		t.Errorf("suspended target: %d, want 403", st)
	}

	// The super admin's denials are audited (the organizer's attempt is
	// stopped by RequireSuperAdmin before reaching the service).
	var after int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'admin.impersonation_denied'`).Scan(&after)
	if after-before < 3 {
		t.Errorf("denied audit rows added = %d, want ≥3", after-before)
	}
}

func TestImpersonation_SignOutEndsBothSessions(t *testing.T) {
	e := setup(t)
	adminTok, _ := loginPair(t, e, adminEmail)
	target, _ := freshOrganizer(t, e, "imp-signout")

	access, refresh, st, _ := impersonate(t, e, adminTok, target)
	if st != http.StatusOK {
		t.Fatal("impersonate failed")
	}

	// Sign out WHILE impersonating: full sign-out, both sessions gone.
	if st, _ := e.call(t, "POST", "/auth/logout", map[string]any{"refresh_token": refresh}, access); st != http.StatusNoContent {
		t.Fatalf("logout: %d", st)
	}
	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusUnauthorized {
		t.Errorf("impersonation token after sign-out: %d, want 401", st)
	}
	if st, _ := e.call(t, "GET", "/admin/overview", nil, adminTok); st != http.StatusUnauthorized {
		t.Errorf("PARENT admin token after sign-out while impersonating: %d, want 401", st)
	}
}

func TestImpersonation_SuspendingTargetEndsIt(t *testing.T) {
	e := setup(t)
	adminTok, _ := loginPair(t, e, adminEmail)
	target, _ := freshOrganizer(t, e, "imp-suspend")

	access, _, st, _ := impersonate(t, e, adminTok, target)
	if st != http.StatusOK {
		t.Fatal("impersonate failed")
	}
	// The admin's own session must survive to perform the suspension: exit
	// first is not required, because the parent token is independent.
	if st, res := e.call(t, "PATCH", "/admin/users/"+target.String(), map[string]any{
		"status": "suspended", "reason": "itest",
	}, adminTok); st != http.StatusOK {
		t.Fatalf("suspend target: %d %v", st, res)
	}
	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusUnauthorized {
		t.Errorf("impersonation token after target suspended: %d, want 401", st)
	}
}

func TestImpersonation_ExitOnNormalSessionIsRejected(t *testing.T) {
	e := setup(t)
	adminTok, _ := loginPair(t, e, adminEmail)
	if st, _ := e.call(t, "POST", "/admin/impersonation/exit", nil, adminTok); st != http.StatusBadRequest {
		t.Errorf("exit on a normal session: %d, want 400", st)
	}
}

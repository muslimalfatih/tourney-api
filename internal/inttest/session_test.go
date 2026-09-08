//go:build integration

package inttest

import (
	"context"
	"net/http"
	"testing"

	"github.com/muslimalfatih/tourney-api/internal/auth"
)

// Subphase 5.1: sessions are rows, so they can be withdrawn. Before 00013 a
// refresh token was a self-contained JWT that stayed valid for its full 30 days
// no matter what — every test here would have failed by design.

// loginPair signs in and returns both tokens.
func loginPair(t *testing.T, e *env, email string) (access, refresh string) {
	t.Helper()
	st, res := e.call(t, "POST", "/auth/login", map[string]any{
		"email": email, "password": password,
	}, "")
	if st != http.StatusOK {
		t.Fatalf("login %s: status %d, body %v", email, st, res)
	}
	data, ok := res["data"].(map[string]any)
	if !ok {
		t.Fatalf("login response has no data object: %v", res)
	}
	access, _ = data["access_token"].(string)
	refresh, _ = data["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("login returned empty tokens: %v", data)
	}
	return access, refresh
}

func TestSession_AccessTokenCarriesResolvableSession(t *testing.T) {
	e := setup(t)
	access, _ := loginPair(t, e, organizerEmail)

	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusOK {
		t.Fatalf("/me with fresh token: status %d, want 200", st)
	}

	// A row must exist and be active.
	var n int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM auth_sessions s
		JOIN users u ON u.id = s.user_id
		WHERE u.email = $1 AND s.revoked_at IS NULL AND s.kind = 'normal'`,
		organizerEmail).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if n == 0 {
		t.Error("login did not create an auth_sessions row")
	}
}

func TestSession_RefreshTokenIsOpaqueNotJWT(t *testing.T) {
	e := setup(t)
	_, refresh := loginPair(t, e, organizerEmail)

	// A JWT has two dots. The refresh credential must carry no self-contained
	// authority — that is the whole point of the change.
	dots := 0
	for _, c := range refresh {
		if c == '.' {
			dots++
		}
	}
	if dots == 2 {
		t.Errorf("refresh token looks like a JWT: %q", refresh)
	}
}

func TestSession_LogoutRevokesImmediately(t *testing.T) {
	e := setup(t)
	access, refresh := loginPair(t, e, organizerEmail)

	if st, _ := e.call(t, "POST", "/auth/logout", map[string]any{"refresh_token": refresh}, access); st != http.StatusNoContent {
		t.Fatalf("logout: status %d, want 204", st)
	}

	// The access token has NOT expired — 15 minutes remain — but its session is
	// gone, so it must stop working at once.
	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusUnauthorized {
		t.Errorf("/me after logout: status %d, want 401", st)
	}
	// And the refresh token cannot mint a replacement.
	if st, _ := e.call(t, "POST", "/auth/refresh", map[string]any{"refresh_token": refresh}, ""); st != http.StatusUnauthorized {
		t.Errorf("refresh after logout: status %d, want 401", st)
	}
}

func TestSession_RefreshRotatesAndKillsOldToken(t *testing.T) {
	e := setup(t)
	_, refresh := loginPair(t, e, organizerEmail)

	st, res := e.call(t, "POST", "/auth/refresh", map[string]any{"refresh_token": refresh}, "")
	if st != http.StatusOK {
		t.Fatalf("refresh: status %d, body %v", st, res)
	}
	data, _ := res["data"].(map[string]any)
	newRefresh, _ := data["refresh_token"].(string)
	newAccess, _ := data["access_token"].(string)
	if newRefresh == "" || newAccess == "" {
		t.Fatalf("refresh returned empty tokens: %v", data)
	}
	if newRefresh == refresh {
		t.Error("refresh token was not rotated")
	}
	if st, _ := e.call(t, "GET", "/me", nil, newAccess); st != http.StatusOK {
		t.Errorf("/me with rotated access token: status %d, want 200", st)
	}
}

func TestSession_ReplayedRefreshTokenRevokesSession(t *testing.T) {
	e := setup(t)
	_, refresh := loginPair(t, e, organizerEmail)

	st, res := e.call(t, "POST", "/auth/refresh", map[string]any{"refresh_token": refresh}, "")
	if st != http.StatusOK {
		t.Fatalf("first refresh: status %d", st)
	}
	data, _ := res["data"].(map[string]any)
	newRefresh, _ := data["refresh_token"].(string)
	newAccess, _ := data["access_token"].(string)

	// Replaying the consumed token is not a retry — a real client never does
	// it. It is treated as theft: the session dies.
	if st, _ := e.call(t, "POST", "/auth/refresh", map[string]any{"refresh_token": refresh}, ""); st != http.StatusUnauthorized {
		t.Errorf("replayed refresh: status %d, want 401", st)
	}
	if st, _ := e.call(t, "POST", "/auth/refresh", map[string]any{"refresh_token": newRefresh}, ""); st != http.StatusUnauthorized {
		t.Errorf("rotated token after replay detected: status %d, want 401 (session revoked)", st)
	}
	if st, _ := e.call(t, "GET", "/me", nil, newAccess); st != http.StatusUnauthorized {
		t.Errorf("/me after replay detected: status %d, want 401", st)
	}

	var reason *string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT revoked_reason FROM auth_sessions s
		JOIN users u ON u.id = s.user_id
		WHERE u.email = $1 ORDER BY s.created_at DESC LIMIT 1`,
		organizerEmail).Scan(&reason); err != nil {
		t.Fatalf("read revoked_reason: %v", err)
	}
	if reason == nil || *reason != auth.RevokeRefreshReuse {
		t.Errorf("revoked_reason = %v, want %q", reason, auth.RevokeRefreshReuse)
	}
}

func TestSession_RevokedSessionBlocksEveryAuthenticatedRoute(t *testing.T) {
	e := setup(t)
	access, _ := loginPair(t, e, adminEmail)

	if st, _ := e.call(t, "GET", "/admin/organizations", nil, access); st != http.StatusOK {
		t.Fatalf("admin route before revoke: status %d, want 200", st)
	}
	// Revoke out of band, the way suspension will in 5.2.
	if _, err := e.pool.Exec(context.Background(), `
		UPDATE auth_sessions SET revoked_at = now(), revoked_reason = 'test'
		WHERE user_id = (SELECT id FROM users WHERE email = $1)`, adminEmail); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if st, _ := e.call(t, "GET", "/admin/organizations", nil, access); st != http.StatusUnauthorized {
		t.Errorf("admin route after revoke: status %d, want 401", st)
	}
	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusUnauthorized {
		t.Errorf("/me after revoke: status %d, want 401", st)
	}
}

func TestSession_ShapeConstraintRejectsMalformedImpersonation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	var uid string
	if err := e.pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, organizerEmail).Scan(&uid); err != nil {
		t.Fatalf("load user: %v", err)
	}

	// An impersonation row without an actor or parent must be impossible, and
	// nobody may impersonate themselves. These are database guarantees, not
	// service conventions, so a bug in 5.5 cannot create a bad row.
	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"impersonation without actor or parent",
			`INSERT INTO auth_sessions (user_id, kind, refresh_token_hash, expires_at)
			 VALUES ($1, 'impersonation', 'h1', now() + interval '1 hour')`, []any{uid}},
		{"normal session carrying an actor",
			`INSERT INTO auth_sessions (user_id, actor_user_id, kind, refresh_token_hash, expires_at)
			 VALUES ($1, $1, 'normal', 'h2', now() + interval '1 hour')`, []any{uid}},
	}
	for _, tc := range cases {
		if _, err := e.pool.Exec(ctx, tc.sql, tc.args...); err == nil {
			t.Errorf("%s: insert succeeded, want CHECK violation", tc.name)
			_, _ = e.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE refresh_token_hash IN ('h1','h2')`)
		}
	}
}

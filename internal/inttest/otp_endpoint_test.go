//go:build integration

package inttest

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/auth"
)

// Subphase 5.4: the two endpoints, end to end through the real engine with a
// fake sender. No real email is ever delivered.

// invite creates an active organizer invitation and returns the address.
func invite(t *testing.T, e *env, prefix string) string {
	t.Helper()
	addr := uniqueEmail(prefix)
	if _, err := invitationRepo(e).Create(context.Background(), nil, auth.CreateInvitationParams{
		Email: addr, Role: "organizer", Organization: ptr(orgID(t, e)),
	}); err != nil {
		t.Fatalf("invite %s: %v", addr, err)
	}
	return addr
}

func ptr[T any](v T) *T { return &v }

// errCode pulls the machine-readable code out of an error envelope.
func errCode(res map[string]any) string {
	if e, ok := res["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return ""
}

func TestOTPEndpoint_HappyPath(t *testing.T) {
	e := setup(t)
	addr := invite(t, e, "ep-ok")

	if st, res := e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, ""); st != http.StatusOK {
		t.Fatalf("request: %d %v", st, res)
	}
	code := e.mail.LastCodeFor(addr)
	if len(code) != 6 {
		t.Fatalf("no six-digit code delivered, got %q", code)
	}

	st, res := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": code}, "")
	if st != http.StatusOK {
		t.Fatalf("verify: %d %v", st, res)
	}
	data, _ := res["data"].(map[string]any)
	access, _ := data["access_token"].(string)
	if access == "" {
		t.Fatal("verify returned no access token")
	}
	user, _ := data["user"].(map[string]any)
	if user["email"] != addr {
		t.Errorf("user.email = %v, want %v", user["email"], addr)
	}
	if user["role"] != "organizer" {
		t.Errorf("role = %v, want organizer (from the invitation)", user["role"])
	}

	// The session works immediately.
	if st, _ := e.call(t, "GET", "/me", nil, access); st != http.StatusOK {
		t.Errorf("/me after OTP sign-in: %d, want 200", st)
	}

	// The invitation is now accepted, and a user row exists.
	var status string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status FROM invitations WHERE email = $1`, addr).Scan(&status); err != nil {
		t.Fatalf("read invitation: %v", err)
	}
	if status != "accepted" {
		t.Errorf("invitation status = %q, want accepted", status)
	}
}

func TestOTPEndpoint_UninvitedIsRejected(t *testing.T) {
	e := setup(t)
	addr := uniqueEmail("ep-stranger")

	st, res := e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	if st != http.StatusForbidden {
		t.Fatalf("uninvited request: %d, want 403", st)
	}
	if got := errCode(res); got != "not_invited" {
		t.Errorf("code = %q, want not_invited", got)
	}
	// And nothing was sent.
	if c := e.mail.LastCodeFor(addr); c != "" {
		t.Error("a code was delivered to an uninvited address")
	}
}

func TestOTPEndpoint_InvalidEmailShape(t *testing.T) {
	e := setup(t)
	for _, bad := range []string{"not-an-email", "no@domain", "@example.com", "two@@x.com"} {
		st, res := e.call(t, "POST", "/auth/otp/request", map[string]any{"email": bad}, "")
		if st != http.StatusUnprocessableEntity {
			t.Errorf("%q: status %d, want 422", bad, st)
		}
		if got := errCode(res); got != "invalid_email" {
			t.Errorf("%q: code %q, want invalid_email", bad, got)
		}
	}
}

func TestOTPEndpoint_WrongCodeThenLockout(t *testing.T) {
	e := setup(t)
	addr := invite(t, e, "ep-wrong")

	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	code := e.mail.LastCodeFor(addr)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}

	for i := 1; i < auth.OTPMaxAttempts; i++ {
		st, res := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": wrong}, "")
		if st != http.StatusUnauthorized || errCode(res) != "invalid_code" {
			t.Fatalf("attempt %d: %d %q, want 401 invalid_code", i, st, errCode(res))
		}
	}
	st, res := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": wrong}, "")
	if st != http.StatusTooManyRequests || errCode(res) != "otp_rate_limited" {
		t.Errorf("lockout: %d %q, want 429 otp_rate_limited", st, errCode(res))
	}
	// The correct code is worthless now.
	if st, _ := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": code}, ""); st == http.StatusOK {
		t.Error("correct code still worked after lockout")
	}
}

func TestOTPEndpoint_CodeIsSingleUse(t *testing.T) {
	e := setup(t)
	addr := invite(t, e, "ep-replay")

	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	code := e.mail.LastCodeFor(addr)

	if st, _ := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": code}, ""); st != http.StatusOK {
		t.Fatal("first verify failed")
	}
	if st, _ := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": code}, ""); st == http.StatusOK {
		t.Error("the same code signed in twice")
	}
}

func TestOTPEndpoint_RateLimitedPerEmail(t *testing.T) {
	e := setup(t)
	addr := invite(t, e, "ep-rl")

	for i := 0; i < auth.OTPPerEmailLimit; i++ {
		if st, _ := e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, ""); st != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i+1, st)
		}
	}
	st, res := e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	if st != http.StatusTooManyRequests || errCode(res) != "otp_rate_limited" {
		t.Errorf("over the limit: %d %q, want 429 otp_rate_limited", st, errCode(res))
	}
}

func TestOTPEndpoint_RevokedInvitationBlocksVerify(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	addr := invite(t, e, "ep-revoked")

	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	code := e.mail.LastCodeFor(addr)

	// Revoked AFTER the code was sent: verify must re-check, not trust the
	// state at request time.
	var invID uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM invitations WHERE email = $1`, addr).Scan(&invID); err != nil {
		t.Fatalf("load invitation: %v", err)
	}
	if err := invitationRepo(e).Revoke(ctx, nil, invID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	st, res := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": code}, "")
	if st != http.StatusForbidden || errCode(res) != "not_invited" {
		t.Errorf("verify after revoke: %d %q, want 403 not_invited", st, errCode(res))
	}
}

func TestOTPEndpoint_CodeForOneEmailCannotVerifyAnother(t *testing.T) {
	e := setup(t)
	alice := invite(t, e, "ep-alice")
	bob := invite(t, e, "ep-bob")

	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": alice}, "")
	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": bob}, "")
	aliceCode := e.mail.LastCodeFor(alice)

	if st, _ := e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": bob, "code": aliceCode}, ""); st == http.StatusOK {
		t.Error("alice's code signed bob in")
	}
}

// The audit trail must never carry the code itself.
func TestOTPEndpoint_AuditNeverStoresTheCode(t *testing.T) {
	e := setup(t)
	addr := invite(t, e, "ep-audit")

	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	code := e.mail.LastCodeFor(addr)
	e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": code}, "")

	var hits int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE diff::text LIKE '%' || $1 || '%'`,
		code).Scan(&hits); err != nil {
		t.Fatalf("scan audit: %v", err)
	}
	if hits != 0 {
		t.Errorf("the plaintext code appears in %d audit row(s)", hits)
	}

	var actions int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action IN ($1, $2)`,
		auth.ActionOTPRequested, auth.ActionOTPVerified).Scan(&actions); err != nil {
		t.Fatalf("count audit actions: %v", err)
	}
	if actions == 0 {
		t.Error("no otp_requested / otp_verified audit rows were written")
	}
}

func TestOTPEndpoint_SuspendedUserGetsNoCode(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	addr := invite(t, e, "ep-susp")

	// Sign in once so the account exists, then suspend it.
	e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	e.call(t, "POST", "/auth/otp/verify", map[string]any{"email": addr, "code": e.mail.LastCodeFor(addr)}, "")

	var uid uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = $1`, addr).Scan(&uid); err != nil {
		t.Fatalf("load user: %v", err)
	}
	svc := auth.NewService(auth.NewRepository(e.pool), nil, auth.NewSessionRepository(e.pool), true)
	if err := svc.SuspendUser(ctx, uid, "itest"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	st, res := e.call(t, "POST", "/auth/otp/request", map[string]any{"email": addr}, "")
	if st != http.StatusForbidden || errCode(res) != "account_suspended" {
		t.Errorf("suspended request: %d %q, want 403 account_suspended", st, errCode(res))
	}
}

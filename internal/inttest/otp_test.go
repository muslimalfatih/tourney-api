//go:build integration

package inttest

import (
	"context"
	"testing"
	"time"

	"github.com/muslimalfatih/tourney-api/internal/auth"
	"github.com/muslimalfatih/tourney-api/internal/email"
)

// Subphase 5.3: the challenge lifecycle against a real database. The HTTP
// endpoints arrive in 5.4; these exercise the engine those endpoints will call.

const itestPepper = "itest-pepper-at-least-16-chars"

func otpRepo(e *env) *auth.OTPRepository { return auth.NewOTPRepository(e.pool, itestPepper) }

func TestOTP_IssueAndVerify(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := otpRepo(e)
	addr := uniqueEmail("otp-ok")

	code, ch, err := repo.Issue(ctx, auth.IssueParams{Email: addr})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("code %q is not six digits", code)
	}
	if !ch.ExpiresAt.After(time.Now()) {
		t.Error("challenge is born expired")
	}

	if _, err := repo.Verify(ctx, addr, "", code); err != nil {
		t.Fatalf("Verify with correct code: %v", err)
	}
}

// The plaintext code must exist only in memory and in the email.
func TestOTP_PlaintextIsNeverStored(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	addr := uniqueEmail("otp-store")

	code, ch, err := otpRepo(e).Issue(ctx, auth.IssueParams{Email: addr})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var stored string
	if err := e.pool.QueryRow(ctx,
		`SELECT code_hash FROM otp_challenges WHERE id = $1`, ch.ID).Scan(&stored); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if stored == code {
		t.Fatal("the code itself was stored")
	}
	var hits int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM otp_challenges WHERE code_hash LIKE '%' || $1 || '%'`,
		code).Scan(&hits); err != nil {
		t.Fatalf("scan for plaintext: %v", err)
	}
	if hits != 0 {
		t.Errorf("plaintext code appears inside %d stored hash(es)", hits)
	}
}

func TestOTP_SingleUse(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := otpRepo(e)
	addr := uniqueEmail("otp-replay")

	code, _, _ := repo.Issue(ctx, auth.IssueParams{Email: addr})
	if _, err := repo.Verify(ctx, addr, "", code); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	// Replaying a consumed code must fail even though it is still "correct".
	if _, err := repo.Verify(ctx, addr, "", code); err != auth.ErrOTPNotFound {
		t.Errorf("replayed code = %v, want ErrOTPNotFound", err)
	}
}

func TestOTP_NewRequestInvalidatesPrevious(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := otpRepo(e)
	addr := uniqueEmail("otp-resend")

	first, _, _ := repo.Issue(ctx, auth.IssueParams{Email: addr})
	second, _, _ := repo.Issue(ctx, auth.IssueParams{Email: addr})

	// A resend must REPLACE the old code, not add a second valid one —
	// otherwise every resend widens the guessable surface.
	if _, err := repo.Verify(ctx, addr, "", first); err == nil {
		t.Error("the superseded code still verified")
	}
	if _, err := repo.Verify(ctx, addr, "", second); err != nil {
		t.Errorf("the newest code failed: %v", err)
	}
}

func TestOTP_ExpiredIsRejected(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := otpRepo(e)
	addr := uniqueEmail("otp-expired")

	code, ch, _ := repo.Issue(ctx, auth.IssueParams{Email: addr})
	if _, err := e.pool.Exec(ctx,
		`UPDATE otp_challenges SET expires_at = now() - interval '1 second' WHERE id = $1`,
		ch.ID); err != nil {
		t.Fatalf("age the challenge: %v", err)
	}
	if _, err := repo.Verify(ctx, addr, "", code); err != auth.ErrOTPExpired {
		t.Errorf("expired code = %v, want ErrOTPExpired", err)
	}
}

func TestOTP_AttemptsAreCappedThenLocked(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := otpRepo(e)
	addr := uniqueEmail("otp-attempts")

	code, ch, _ := repo.Issue(ctx, auth.IssueParams{Email: addr})
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}

	// Four wrong guesses stay open...
	for i := 1; i < auth.OTPMaxAttempts; i++ {
		if _, err := repo.Verify(ctx, addr, "", wrong); err != auth.ErrOTPInvalid {
			t.Fatalf("attempt %d = %v, want ErrOTPInvalid", i, err)
		}
	}
	// ...the fifth kills the challenge outright.
	if _, err := repo.Verify(ctx, addr, "", wrong); err != auth.ErrOTPLocked {
		t.Errorf("final attempt = %v, want ErrOTPLocked", err)
	}
	// And the CORRECT code is now worthless — the lock is not bypassable by
	// finally guessing right.
	if _, err := repo.Verify(ctx, addr, "", code); err == nil {
		t.Error("correct code still worked after the attempt cap was reached")
	}

	var attempts int
	var invalidated *time.Time
	if err := e.pool.QueryRow(ctx,
		`SELECT attempts, invalidated_at FROM otp_challenges WHERE id = $1`,
		ch.ID).Scan(&attempts, &invalidated); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if attempts != auth.OTPMaxAttempts {
		t.Errorf("attempts = %d, want %d", attempts, auth.OTPMaxAttempts)
	}
	if invalidated == nil {
		t.Error("challenge was not invalidated at the cap")
	}
}

// The headline cross-account property, end to end against the database.
func TestOTP_CodeForOneEmailCannotVerifyAnother(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repo := otpRepo(e)

	alice := uniqueEmail("otp-alice")
	bob := uniqueEmail("otp-bob")

	aliceCode, _, _ := repo.Issue(ctx, auth.IssueParams{Email: alice})
	if _, _, err := repo.Issue(ctx, auth.IssueParams{Email: bob}); err != nil {
		t.Fatalf("issue for bob: %v", err)
	}

	if _, err := repo.Verify(ctx, bob, "", aliceCode); err == nil {
		t.Error("alice's code authenticated bob")
	}
	// Alice's own code still works — the failure above was the binding, not a
	// side effect of issuing bob's.
	if _, err := repo.Verify(ctx, alice, "", aliceCode); err != nil {
		t.Errorf("alice's code stopped working: %v", err)
	}
}

func TestOTP_UnknownAddressHasNoChallenge(t *testing.T) {
	e := setup(t)
	if _, err := otpRepo(e).Verify(context.Background(), uniqueEmail("otp-none"), "", "123456"); err != auth.ErrOTPNotFound {
		t.Errorf("verify with no challenge = %v, want ErrOTPNotFound", err)
	}
}

func TestFakeSender_CapturesCodesForTests(t *testing.T) {
	fake := email.NewFakeSender()
	ctx := context.Background()

	if err := fake.SendOTP(ctx, "a@x.com", "482913"); err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	if err := fake.SendInvitation(ctx, "b@x.com", "organizer"); err != nil {
		t.Fatalf("SendInvitation: %v", err)
	}
	if got := fake.LastCodeFor("a@x.com"); got != "482913" {
		t.Errorf("LastCodeFor = %q, want 482913", got)
	}
	if got := fake.LastCodeFor("nobody@x.com"); got != "" {
		t.Errorf("LastCodeFor(unknown) = %q, want empty", got)
	}
	if n := len(fake.Sent()); n != 2 {
		t.Errorf("Sent() = %d messages, want 2", n)
	}

	fake.FailNext = true
	if err := fake.SendOTP(ctx, "a@x.com", "111111"); err == nil {
		t.Error("FailNext did not produce an error")
	}
	if err := fake.SendOTP(ctx, "a@x.com", "222222"); err != nil {
		t.Error("FailNext should only affect one send")
	}
}

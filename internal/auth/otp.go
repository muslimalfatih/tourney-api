package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// OTPPurposeLogin is the only purpose today. The column exists so a future
	// flow (email change, say) cannot accidentally reuse a login code.
	OTPPurposeLogin = "login"

	// OTPTTL is deliberately short. A six-digit code has only 10^6 values, so
	// its safety comes from being useless quickly, not from being hard to
	// guess.
	OTPTTL = 10 * time.Minute

	// OTPMaxAttempts caps guesses per challenge. Five wrong tries against 10^6
	// values is a 1-in-200,000 chance; the lock is what keeps it there.
	OTPMaxAttempts = 5

	otpDigits = 6
)

var (
	ErrOTPNotFound = errors.New("no active verification code")
	ErrOTPExpired  = errors.New("verification code expired")
	ErrOTPInvalid  = errors.New("verification code invalid")
	// ErrOTPLocked means the attempt cap was reached and the challenge is dead.
	// The holder must request a new code.
	ErrOTPLocked = errors.New("too many attempts")
)

// GenerateCode returns a uniformly random six-digit code as a string.
//
// crypto/rand with rand.Int over exactly 10^6, not `n % 1000000`: the modulo
// form is biased toward low values, and while the bias is small it is exactly
// the kind of thing that quietly shrinks a keyspace nobody re-examines. The
// result keeps leading zeros — "004821" is a valid code and must stay six
// characters, or it would be a five-digit code with a tenth of the space.
func GenerateCode() (string, error) {
	max := big.NewInt(1)
	for i := 0; i < otpDigits; i++ {
		max.Mul(max, big.NewInt(10))
	}
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("generate otp: %w", err)
	}
	return fmt.Sprintf("%0*d", otpDigits, n), nil
}

// HashCode returns the stored hash for a code.
//
// HMAC-SHA256 over "email:code", keyed by the pepper. Three properties matter:
//
//   - Cheap and constant-memory. The password hasher uses argon2id at 64 MiB
//     per call, which on a 256 MB machine turns an unauthenticated verify
//     endpoint into a memory-exhaustion vector. A KDF's work factor buys
//     almost nothing against 10^6 values anyway.
//   - Peppered. Someone who reads the table still cannot enumerate the code
//     space offline, because the key is not in the database.
//   - Bound to the address. A code minted for one email cannot verify another,
//     as a matter of arithmetic rather than of query correctness.
func HashCode(email, code, pepper string) string {
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(NormalizeEmail(email)))
	mac.Write([]byte{':'})
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyCode reports whether a code matches a stored hash, in constant time.
func VerifyCode(email, code, storedHash, pepper string) bool {
	return hmac.Equal([]byte(HashCode(email, code, pepper)), []byte(storedHash))
}

// OTPChallenge is a live or spent verification attempt.
type OTPChallenge struct {
	ID           uuid.UUID
	Email        string
	Purpose      string
	InvitationID *uuid.UUID
	Attempts     int
	MaxAttempts  int
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// OTPRepository owns the challenge lifecycle.
type OTPRepository struct {
	pool   *pgxpool.Pool
	pepper string
}

func NewOTPRepository(pool *pgxpool.Pool, pepper string) *OTPRepository {
	return &OTPRepository{pool: pool, pepper: pepper}
}

// IssueParams describes a code to mint.
type IssueParams struct {
	Email        string
	Purpose      string
	InvitationID *uuid.UUID
	IPHash       *string
	UAHash       *string
}

// Issue mints a code, invalidating any earlier live challenge for the address.
//
// Returns the PLAINTEXT code for the sender to deliver. It is never persisted,
// never logged and never returned over HTTP — the only correct thing to do
// with it is hand it to a Sender and forget it.
//
// Both statements run in one transaction: a crash between them would otherwise
// leave two live challenges, and the older one would still be guessable.
func (r *OTPRepository) Issue(ctx context.Context, p IssueParams) (string, *OTPChallenge, error) {
	email := NormalizeEmail(p.Email)
	purpose := p.Purpose
	if purpose == "" {
		purpose = OTPPurposeLogin
	}

	code, err := GenerateCode()
	if err != nil {
		return "", nil, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Requesting a new code retires the old one. Otherwise every resend would
	// widen the attack surface instead of replacing it.
	if _, err := tx.Exec(ctx, `
		UPDATE otp_challenges
		SET invalidated_at = now()
		WHERE email = $1 AND purpose = $2
		  AND consumed_at IS NULL AND invalidated_at IS NULL`, email, purpose); err != nil {
		return "", nil, err
	}

	var ch OTPChallenge
	err = tx.QueryRow(ctx, `
		INSERT INTO otp_challenges
			(email, purpose, code_hash, invitation_id, max_attempts, expires_at,
			 requested_ip_hash, request_user_agent_hash)
		VALUES ($1, $2, $3, $4, $5, now() + $6::interval, $7, $8)
		RETURNING id, email, purpose, invitation_id, attempts, max_attempts, created_at, expires_at`,
		email, purpose, HashCode(email, code, r.pepper), p.InvitationID,
		OTPMaxAttempts, fmt.Sprintf("%d seconds", int(OTPTTL.Seconds())),
		p.IPHash, p.UAHash,
	).Scan(&ch.ID, &ch.Email, &ch.Purpose, &ch.InvitationID, &ch.Attempts,
		&ch.MaxAttempts, &ch.CreatedAt, &ch.ExpiresAt)
	if err != nil {
		return "", nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, err
	}
	return code, &ch, nil
}

// LockLive loads the newest usable challenge for an address and holds a row
// lock on it, inside the caller's transaction.
//
// Exported as its own step so the SUCCESS path can consume the challenge, create
// the user, accept the invitation and open the session in ONE transaction —
// either the whole sign-in happens or none of it does.
func (r *OTPRepository) LockLive(ctx context.Context, q Querier, email, purpose string) (*OTPChallenge, string, error) {
	email = NormalizeEmail(email)
	if purpose == "" {
		purpose = OTPPurposeLogin
	}
	var (
		ch   OTPChallenge
		hash string
	)
	err := q.QueryRow(ctx, `
		SELECT id, email, purpose, invitation_id, attempts, max_attempts,
		       created_at, expires_at, code_hash
		FROM otp_challenges
		WHERE email = $1 AND purpose = $2
		  AND consumed_at IS NULL AND invalidated_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1
		FOR UPDATE`, email, purpose).
		Scan(&ch.ID, &ch.Email, &ch.Purpose, &ch.InvitationID, &ch.Attempts,
			&ch.MaxAttempts, &ch.CreatedAt, &ch.ExpiresAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrOTPNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return &ch, hash, nil
}

// CheckLive applies the non-cryptographic rules to a locked challenge.
func CheckLive(ch *OTPChallenge) error {
	if !ch.ExpiresAt.After(time.Now()) {
		return ErrOTPExpired
	}
	if ch.Attempts >= ch.MaxAttempts {
		return ErrOTPLocked
	}
	return nil
}

// Consume marks a challenge used, inside the caller's transaction.
func (r *OTPRepository) Consume(ctx context.Context, q Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE otp_challenges
		SET consumed_at = now(), last_attempt_at = now()
		WHERE id = $1`, id)
	return err
}

// RecordFailure increments the attempt counter in its OWN transaction, and
// reports whether that reached the cap.
//
// Separate on purpose: the caller's transaction is rolled back on a wrong code,
// and an attempt that vanished with it would let anyone escape the cap by
// guessing until they hit the right answer. The increment is `attempts + 1` in
// SQL, so concurrent wrong guesses each count.
func (r *OTPRepository) RecordFailure(ctx context.Context, id uuid.UUID) (locked bool, err error) {
	err = r.pool.QueryRow(ctx, `
		UPDATE otp_challenges
		SET attempts        = attempts + 1,
		    last_attempt_at = now(),
		    invalidated_at  = CASE WHEN attempts + 1 >= max_attempts THEN now() ELSE invalidated_at END
		WHERE id = $1
		RETURNING attempts >= max_attempts`, id).Scan(&locked)
	return locked, err
}

// Verify is the standalone form: check a code and consume it, managing its own
// transaction. Composed from the pieces above so there is only one definition
// of the rules.
func (r *OTPRepository) Verify(ctx context.Context, email, purpose, code string) (*OTPChallenge, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ch, hash, err := r.LockLive(ctx, tx, email, purpose)
	if err != nil {
		return nil, err
	}
	if err := CheckLive(ch); err != nil {
		return nil, err
	}
	if !VerifyCode(email, code, hash, r.pepper) {
		_ = tx.Rollback(ctx)
		locked, e := r.RecordFailure(ctx, ch.ID)
		if e != nil {
			return nil, e
		}
		if locked {
			return nil, ErrOTPLocked
		}
		return nil, ErrOTPInvalid
	}
	if err := r.Consume(ctx, tx, ch.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ch, nil
}

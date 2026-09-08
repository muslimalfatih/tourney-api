package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Session kinds, mirroring the session_kind enum in 00013.
const (
	SessionNormal        = "normal"
	SessionImpersonation = "impersonation"
)

// Revocation reasons. Stored on the row so the audit trail says why a session
// ended, not merely that it did.
const (
	RevokeLogout        = "logout"
	RevokeParentLogout  = "parent_logout"
	RevokeRefreshReuse  = "refresh_token_reuse"
	RevokeImpersonation = "impersonation_ended"
)

var (
	// ErrSessionNotFound covers "no such session" and "not active any more".
	// Callers must not distinguish the two: a revoked session and a forged id
	// should look identical from outside.
	ErrSessionNotFound = errors.New("session not found or inactive")

	// ErrRefreshReused means the presented refresh token was already rotated
	// away. A legitimate client never replays one, so the session is revoked.
	ErrRefreshReused = errors.New("refresh token reuse detected")
)

// Session is a persisted login. UserID is the EFFECTIVE identity — who the
// request acts as. ActorUserID is the real human during impersonation.
type Session struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	ActorUserID     *uuid.UUID
	ParentSessionID *uuid.UUID
	Kind            string
	ExpiresAt       time.Time
	RevokedAt       *time.Time
}

// IsImpersonation reports whether this session acts as someone else.
func (s *Session) IsImpersonation() bool { return s.Kind == SessionImpersonation }

// NewRefreshToken mints an opaque refresh token and the hash to store.
//
// 32 bytes from crypto/rand is 256 bits of entropy, so SHA-256 is the right
// digest here — a KDF like argon2id exists to slow down guessing of
// low-entropy secrets (passwords), and buys nothing against a value that
// cannot be guessed. Only the hash is ever persisted.
func NewRefreshToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashRefreshToken(raw), nil
}

// HashRefreshToken returns the storage hash for an opaque refresh token.
func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// SessionRepository is the data-access layer for auth_sessions.
type SessionRepository struct {
	pool *pgxpool.Pool
}

func NewSessionRepository(pool *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{pool: pool}
}

const sessionCols = `id, user_id, actor_user_id, parent_session_id, kind, expires_at, revoked_at`

func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.UserID, &s.ActorUserID, &s.ParentSessionID, &s.Kind, &s.ExpiresAt, &s.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateParams describes a session to open. Actor/Parent/Reason are set only
// for impersonation; the CHECK constraint rejects any other combination.
type CreateParams struct {
	UserID          uuid.UUID
	ActorUserID     *uuid.UUID
	ParentSessionID *uuid.UUID
	Kind            string
	RefreshHash     string
	ExpiresAt       time.Time
	IPHash          *string
	UserAgentHash   *string
	Reason          *string
}

// Create opens a session. q may be a pool or a transaction, so opening a
// session can join the caller's transaction — OTP verify (5.3) needs the
// challenge consumption and the session to commit together.
func (r *SessionRepository) Create(ctx context.Context, q Querier, p CreateParams) (*Session, error) {
	const stmt = `
		INSERT INTO auth_sessions
			(user_id, actor_user_id, parent_session_id, kind, refresh_token_hash,
			 expires_at, ip_hash, user_agent_hash, impersonation_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING ` + sessionCols
	if q == nil {
		q = r.pool
	}
	row := q.QueryRow(ctx, stmt,
		p.UserID, p.ActorUserID, p.ParentSessionID, p.Kind, p.RefreshHash,
		p.ExpiresAt, p.IPHash, p.UserAgentHash, p.Reason)
	return scanSession(row)
}

// FindActive loads a session by id, but only while it is usable. This runs on
// every authenticated request: it is the check that makes revocation immediate
// rather than "whenever the access token happens to expire".
func (r *SessionRepository) FindActive(ctx context.Context, id uuid.UUID) (*Session, error) {
	const q = `
		SELECT ` + sessionCols + `
		FROM auth_sessions
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > now()`
	return scanSession(r.pool.QueryRow(ctx, q, id))
}

// FindByRefreshToken resolves a session from an opaque refresh token, active or
// not. Sign-out uses it: revoking an already-expired session is harmless, and
// refusing to look it up would leave a user unable to sign out.
func (r *SessionRepository) FindByRefreshToken(ctx context.Context, rawToken string) (*Session, error) {
	const q = `
		SELECT ` + sessionCols + `
		FROM auth_sessions
		WHERE refresh_token_hash = $1`
	return scanSession(r.pool.QueryRow(ctx, q, HashRefreshToken(rawToken)))
}

// TouchLastSeen records activity. Best-effort: a failure here must never fail
// the request it was observing.
func (r *SessionRepository) TouchLastSeen(ctx context.Context, id uuid.UUID) {
	_, _ = r.pool.Exec(ctx, `UPDATE auth_sessions SET last_seen_at = now() WHERE id = $1`, id)
}

// Revoke ends one session. Already-revoked rows keep their original reason, so
// the first cause of death is the one recorded.
func (r *SessionRepository) Revoke(ctx context.Context, q Querier, id uuid.UUID, reason string) error {
	if q == nil {
		q = r.pool
	}
	_, err := q.Exec(ctx, `
		UPDATE auth_sessions
		SET revoked_at = now(), revoked_reason = $2
		WHERE id = $1 AND revoked_at IS NULL`, id, reason)
	return err
}

// RevokeChildren ends every impersonation session started from a parent. Used
// when a super admin signs out while impersonating.
func (r *SessionRepository) RevokeChildren(ctx context.Context, q Querier, parentID uuid.UUID, reason string) error {
	if q == nil {
		q = r.pool
	}
	_, err := q.Exec(ctx, `
		UPDATE auth_sessions
		SET revoked_at = now(), revoked_reason = $2
		WHERE parent_session_id = $1 AND revoked_at IS NULL`, parentID, reason)
	return err
}

// RevokeAllForUser ends every session belonging to a user AND every
// impersonation they started. Suspension and invitation revocation (5.2) call
// this: leaving an impersonation alive after suspending the super admin who
// opened it would keep their access alive under another name.
func (r *SessionRepository) RevokeAllForUser(ctx context.Context, q Querier, userID uuid.UUID, reason string) error {
	if q == nil {
		q = r.pool
	}
	_, err := q.Exec(ctx, `
		UPDATE auth_sessions
		SET revoked_at = now(), revoked_reason = $2
		WHERE revoked_at IS NULL AND (user_id = $1 OR actor_user_id = $1)`, userID, reason)
	return err
}

// Rotate exchanges a refresh token for a new one, returning the session and the
// new opaque token. The old token stops working immediately.
//
// Runs in one transaction with SELECT ... FOR UPDATE so two concurrent refreshes
// cannot both succeed: the loser finds the row already rotated and is treated
// as a replay.
func (r *SessionRepository) Rotate(ctx context.Context, rawToken string) (*Session, string, error) {
	hash := HashRefreshToken(rawToken)

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sess, err := scanSession(tx.QueryRow(ctx, `
		SELECT `+sessionCols+`
		FROM auth_sessions
		WHERE refresh_token_hash = $1
		FOR UPDATE`, hash))

	if errors.Is(err, ErrSessionNotFound) {
		// Not a live token. Before calling it unknown, check whether it is a
		// token we already rotated away — that is a replay, and the session it
		// belongs to can no longer be trusted.
		var reusedID uuid.UUID
		scanErr := tx.QueryRow(ctx, `
			SELECT id FROM auth_sessions
			WHERE previous_refresh_token_hash = $1 AND revoked_at IS NULL
			FOR UPDATE`, hash).Scan(&reusedID)
		if scanErr == nil {
			if _, e := tx.Exec(ctx, `
				UPDATE auth_sessions
				SET revoked_at = now(), revoked_reason = $2
				WHERE id = $1`, reusedID, RevokeRefreshReuse); e != nil {
				return nil, "", e
			}
			if e := tx.Commit(ctx); e != nil {
				return nil, "", e
			}
			return nil, "", ErrRefreshReused
		}
		return nil, "", ErrSessionNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if sess.RevokedAt != nil || !sess.ExpiresAt.After(time.Now()) {
		return nil, "", ErrSessionNotFound
	}

	newRaw, newHash, err := NewRefreshToken()
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auth_sessions
		SET previous_refresh_token_hash = refresh_token_hash,
		    refresh_token_hash = $2,
		    last_seen_at = now()
		WHERE id = $1`, sess.ID, newHash); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return sess, newRaw, nil
}

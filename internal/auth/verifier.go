package auth

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

// Querier is the subset of pgx that both *pgxpool.Pool and pgx.Tx satisfy, so
// a session write can either stand alone or join a caller's transaction. Same
// idea as audit.Execer, extended with QueryRow because opening a session needs
// its RETURNING row.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SessionVerifier is what the Auth middleware actually calls. It combines the
// two halves of authentication that must BOTH hold:
//
//  1. the access JWT is well-formed, unexpired and signed by us, and
//  2. the session its sid names is still alive.
//
// Splitting them matters. The JWT alone is a claim the server made up to 15
// minutes ago; the session row is the current truth. TokenService stays a pure
// signing/parsing unit with no database, which keeps its unit tests fast, and
// this type is the only place the two are combined.
type SessionVerifier struct {
	tokens   *TokenService
	sessions *SessionRepository
	users    *Repository
}

func NewSessionVerifier(tokens *TokenService, sessions *SessionRepository, users *Repository) *SessionVerifier {
	return &SessionVerifier{tokens: tokens, sessions: sessions, users: users}
}

// VerifyAccessToken implements middleware.TokenVerifier.
//
// The role and org_id claims are NOT trusted on the strength of the signature
// alone — the session must resolve first. 5.2 adds the user-status check here
// once users.status exists, which is the natural home for it: one lookup, one
// place, every authenticated route.
func (v *SessionVerifier) VerifyAccessToken(ctx context.Context, raw string) (*middleware.Claims, error) {
	claims, err := v.tokens.VerifyAccessToken(raw)
	if err != nil {
		return nil, err
	}
	// A token minted before 00013 has no sid and cannot be resolved to a
	// session. Rejecting it is deliberate: it signs out anyone holding a
	// pre-migration token instead of silently trusting an unrevocable claim.
	if claims.SessionID == uuid.Nil {
		return nil, ErrSessionNotFound
	}
	sess, err := v.sessions.FindActive(ctx, claims.SessionID)
	if err != nil {
		return nil, err
	}
	// The effective user must still be allowed in. Suspension revokes sessions
	// directly (SuspendUser), so this is belt-and-braces — it also covers a
	// status changed by hand in the database, which is exactly the case where a
	// missed session would be least expected and most damaging.
	user, err := v.users.FindByID(ctx, sess.UserID)
	if err != nil {
		return nil, err
	}
	if !user.IsActive() {
		return nil, ErrAccountSuspended
	}

	// The database is authoritative about who this is and what they may do; the
	// JWT only carries it to save a lookup. Where they disagree, the row wins —
	// so a role change takes effect on the next request rather than at token
	// expiry.
	claims.UserID = user.ID
	claims.Role = user.Role
	claims.OrgID = user.OrgID
	claims.SessionID = sess.ID
	return claims, nil
}

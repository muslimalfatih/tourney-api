// Package audit records critical tournament changes (create/publish/suspend,
// score overrides) for accountability, and serves the super-admin audit views.
package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

// Entry is a single audit record to write. Diff holds an optional before/after
// snapshot serialized to JSON.
type Entry struct {
	// ActorUserID is the human who acted. Leave it as uuid.Nil for events that
	// happen before anyone is authenticated — an OTP request names an address,
	// not yet an account — and it is stored as NULL rather than a zero UUID,
	// which would fail the foreign key.
	ActorUserID uuid.UUID
	// EffectiveUserID is the identity the request ran under during
	// impersonation — the organizer — and nil at every other time. Both
	// columns exist so the log can answer "who really did this" and "as whom"
	// separately.
	EffectiveUserID        *uuid.UUID
	ImpersonationSessionID *uuid.UUID
	// Reason is required for platform force actions and encouraged elsewhere.
	Reason       *string
	OrgID        *uuid.UUID
	TournamentID *uuid.UUID
	Action       string
	TargetType   string
	TargetID     string
	Diff         map[string]any
}

// Log is a stored audit record joined with the actor's name for display.
type Log struct {
	ID             uuid.UUID      `json:"id"`
	ActorName      *string        `json:"actor_name"`
	ActorEmail     *string        `json:"actor_email"`
	EffectiveName  *string        `json:"effective_name"`
	EffectiveEmail *string        `json:"effective_email"`
	Impersonated   bool           `json:"impersonated"`
	Reason         *string        `json:"reason"`
	Action         string         `json:"action"`
	TargetType     string         `json:"target_type"`
	TargetID       string         `json:"target_id"`
	TournamentID   *uuid.UUID     `json:"tournament_id"`
	Diff           map[string]any `json:"diff"`
	CreatedAt      time.Time      `json:"created_at"`
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Record writes an audit entry. Callers must not fail their operation if
// auditing fails — log and continue. Errors are returned so callers can log.
//
// The diff is passed as a JSON string (not []byte): under the simple query
// protocol used behind the Supabase pooler, a string binds cleanly to jsonb
// whereas a raw byte slice does not.
// Execer is the subset of pgx both *pgxpool.Pool and pgx.Tx satisfy, so an
// audit row can join a caller's transaction (write-then-commit, per the
// correction flow) instead of racing it from a separate connection.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// RecordTx is Record on a caller-supplied transaction/connection.
// attribute fills in the impersonation fields from the request context when
// the caller did not set them explicitly.
//
// Services today pass middleware.UserID(c) as the actor, which during
// impersonation is the ORGANIZER. The context knows better: the real actor is
// the super admin, and the organizer is the effective user. Correcting that
// here, once, means no existing writer records the wrong person and no future
// writer can forget.
func attribute(ctx context.Context, e Entry) Entry {
	a, ok := middleware.AttributionFrom(ctx)
	if !ok || a.Effective == nil {
		return e
	}
	if e.EffectiveUserID == nil {
		e.EffectiveUserID = a.Effective
	}
	if e.ImpersonationSessionID == nil {
		e.ImpersonationSessionID = a.Session
	}
	// If the writer named the effective user as the actor, or nobody at all,
	// the actor is really the human behind the impersonation.
	if e.ActorUserID == uuid.Nil || e.ActorUserID == *a.Effective {
		e.ActorUserID = a.Actor
	}
	return e
}

func (s *Service) RecordTx(ctx context.Context, q Execer, e Entry) error {
	e = attribute(ctx, e)
	var diff *string
	if e.Diff != nil {
		b, err := json.Marshal(e.Diff)
		if err != nil {
			return err
		}
		str := string(b)
		diff = &str
	}
	_, err := q.Exec(ctx, `
		INSERT INTO audit_logs
			(org_id, actor_user_id, effective_user_id, impersonation_session_id, reason,
			 tournament_id, action, target_type, target_id, diff)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`,
		e.OrgID, actorOrNil(e.ActorUserID), e.EffectiveUserID, e.ImpersonationSessionID, e.Reason,
		e.TournamentID, e.Action, e.TargetType, e.TargetID, diff)
	return err
}

// actorOrNil maps the zero UUID to a SQL NULL, so an unauthenticated event
// records "no actor" instead of violating the users foreign key.
func actorOrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func (s *Service) Record(ctx context.Context, e Entry) error {
	e = attribute(ctx, e)
	var diff *string
	if e.Diff != nil {
		b, err := json.Marshal(e.Diff)
		if err != nil {
			return err
		}
		str := string(b)
		diff = &str
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_logs
			(org_id, actor_user_id, effective_user_id, impersonation_session_id, reason,
			 tournament_id, action, target_type, target_id, diff)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`,
		e.OrgID, actorOrNil(e.ActorUserID), e.EffectiveUserID, e.ImpersonationSessionID, e.Reason,
		e.TournamentID, e.Action, e.TargetType, e.TargetID, diff)
	return err
}

// ListFilter narrows the platform audit view. Zero values mean "no filter".
type ListFilter struct {
	Action           string
	ActorUserID      *uuid.UUID
	EffectiveUserID  *uuid.UUID
	TournamentID     *uuid.UUID
	OrgID            *uuid.UUID
	ImpersonatedOnly bool
	From             *time.Time
	To               *time.Time
}

// List returns audit records (super-admin view), newest first.
//
// Both people on a row are resolved to names for display; the raw diff is
// returned as-is because it never contains secrets — writers are responsible
// for that, and a test greps for OTP codes to keep them honest.
func (s *Service) List(ctx context.Context, f ListFilter, limit, offset int) ([]Log, int64, error) {
	const q = `
		SELECT a.id,
		       actor.name, actor.email,
		       eff.name,   eff.email,
		       a.impersonation_session_id IS NOT NULL,
		       a.reason,
		       a.action, a.target_type, a.target_id, a.tournament_id, a.diff, a.created_at,
		       COUNT(*) OVER() AS total
		FROM audit_logs a
		LEFT JOIN users actor ON actor.id = a.actor_user_id
		LEFT JOIN users eff   ON eff.id   = a.effective_user_id
		WHERE ($3::text IS NULL OR a.action = $3)
		  AND ($4::uuid IS NULL OR a.actor_user_id = $4)
		  AND ($5::uuid IS NULL OR a.effective_user_id = $5)
		  AND ($6::uuid IS NULL OR a.tournament_id = $6)
		  AND ($7::uuid IS NULL OR a.org_id = $7)
		  AND (NOT $8::bool OR a.impersonation_session_id IS NOT NULL)
		  AND ($9::timestamptz IS NULL OR a.created_at >= $9)
		  AND ($10::timestamptz IS NULL OR a.created_at < $10)
		ORDER BY a.created_at DESC
		LIMIT $1 OFFSET $2`
	var action *string
	if f.Action != "" {
		action = &f.Action
	}
	rows, err := s.pool.Query(ctx, q, limit, offset, action, f.ActorUserID, f.EffectiveUserID,
		f.TournamentID, f.OrgID, f.ImpersonatedOnly, f.From, f.To)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Log{}
	var total int64
	for rows.Next() {
		var l Log
		var diff []byte
		if err := rows.Scan(&l.ID, &l.ActorName, &l.ActorEmail, &l.EffectiveName, &l.EffectiveEmail,
			&l.Impersonated, &l.Reason, &l.Action, &l.TargetType, &l.TargetID,
			&l.TournamentID, &diff, &l.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		if len(diff) > 0 {
			_ = json.Unmarshal(diff, &l.Diff)
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

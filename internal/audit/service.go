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
)

// Entry is a single audit record to write. Diff holds an optional before/after
// snapshot serialized to JSON.
type Entry struct {
	ActorUserID  uuid.UUID
	OrgID        *uuid.UUID
	TournamentID *uuid.UUID
	Action       string
	TargetType   string
	TargetID     string
	Diff         map[string]any
}

// Log is a stored audit record joined with the actor's name for display.
type Log struct {
	ID           uuid.UUID      `json:"id"`
	ActorName    *string        `json:"actor_name"`
	Action       string         `json:"action"`
	TargetType   string         `json:"target_type"`
	TargetID     string         `json:"target_id"`
	TournamentID *uuid.UUID     `json:"tournament_id"`
	Diff         map[string]any `json:"diff"`
	CreatedAt    time.Time      `json:"created_at"`
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
func (s *Service) RecordTx(ctx context.Context, q Execer, e Entry) error {
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
		INSERT INTO audit_logs (org_id, actor_user_id, tournament_id, action, target_type, target_id, diff)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		e.OrgID, e.ActorUserID, e.TournamentID, e.Action, e.TargetType, e.TargetID, diff)
	return err
}

func (s *Service) Record(ctx context.Context, e Entry) error {
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
		INSERT INTO audit_logs (org_id, actor_user_id, tournament_id, action, target_type, target_id, diff)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		e.OrgID, e.ActorUserID, e.TournamentID, e.Action, e.TargetType, e.TargetID, diff)
	return err
}

// List returns recent audit records (super-admin view), newest first.
func (s *Service) List(ctx context.Context, limit, offset int) ([]Log, int64, error) {
	const q = `
		SELECT a.id, u.name, a.action, a.target_type, a.target_id, a.tournament_id, a.diff, a.created_at,
		       COUNT(*) OVER() AS total
		FROM audit_logs a
		LEFT JOIN users u ON u.id = a.actor_user_id
		ORDER BY a.created_at DESC
		LIMIT $1 OFFSET $2`
	rows, err := s.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Log{}
	var total int64
	for rows.Next() {
		var l Log
		var diff []byte
		if err := rows.Scan(&l.ID, &l.ActorName, &l.Action, &l.TargetType, &l.TargetID,
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

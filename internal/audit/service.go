// Package audit records critical tournament changes (create/publish/score
// overrides, suspensions) for accountability. It is a write-mostly service
// other modules call after a state change; queries back the super-admin audit
// views. Skeleton: interface defined, persistence lands with milestone 1.
package audit

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Entry is a single audit record. Diff holds a before/after snapshot as JSON.
type Entry struct {
	ActorUserID  uuid.UUID
	OrgID        *uuid.UUID
	TournamentID *uuid.UUID
	Action       string
	TargetType   string
	TargetID     string
	Diff         map[string]any
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Record writes an audit entry. Callers should not fail their operation if
// auditing fails; log and continue.
func (s *Service) Record(ctx context.Context, e Entry) error {
	// TODO(m1): INSERT INTO audit_logs (...) VALUES (...).
	_ = ctx
	_ = e
	return nil
}

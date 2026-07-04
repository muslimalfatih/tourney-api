// Package postgres owns the PostgreSQL connection pool and transaction helpers.
// It is the single place that knows about pgx; everything else works through
// the sqlc-generated Queries type or the small helpers here.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pgx connection pool sized for the Supabase Free tier.
//
// maxConns is intentionally small: the free tier's transaction pooler has a
// limited connection budget, so we keep the pool tight rather than exhausting
// it. The URL should point at the pooler endpoint (port 6543) in production.
func Connect(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	// Supabase's transaction pooler (pgbouncer, port 6543) does not support the
	// prepared-statement protocol pgx uses by default — the first query on each
	// fresh connection fails with a prepared-statement error. Using the simple
	// query protocol avoids server-side prepares entirely, which is the
	// supported mode behind a transaction pooler.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

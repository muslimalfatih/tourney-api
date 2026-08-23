// Command migrate applies (or rolls back) database migrations using the goose
// library against the embedded migration files. It uses MIGRATION_DATABASE_URL
// (a direct connection) because migrations need session-level features the
// Supabase transaction pooler does not provide.
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate down
//	go run ./cmd/migrate status
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/stdlib" // database/sql driver for pgx
	"github.com/pressly/goose/v3"

	"github.com/muslimalfatih/tourney-api/db"
	"github.com/muslimalfatih/tourney-api/internal/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migrate failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// goose runs over database/sql; open a pgx stdlib connection to the DIRECT
	// (non-pooler) database URL.
	conn, err := sql.Open("pgx", cfg.MigrationDatabaseURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()

	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	return goose.RunContext(context.Background(), command, conn, "migrations")
}

var _ = stdlib.GetDefaultDriver // ensure the pgx stdlib driver is registered

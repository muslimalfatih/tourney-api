// Command seed creates the initial super-admin account. Run once after
// migrations:
//
//	SEED_ADMIN_EMAIL=you@example.com SEED_ADMIN_PASSWORD=change-me \
//	  DATABASE_URL=... go run ./cmd/seed
//
// It is idempotent: if a user with the given email already exists it exits
// without error.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/muslimalfatih/laga-api/internal/auth"
	"github.com/muslimalfatih/laga-api/internal/config"
	"github.com/muslimalfatih/laga-api/internal/storage/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	email := os.Getenv("SEED_ADMIN_EMAIL")
	password := os.Getenv("SEED_ADMIN_PASSWORD")
	name := os.Getenv("SEED_ADMIN_NAME")
	if email == "" || password == "" {
		return errors.New("SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD are required")
	}
	if name == "" {
		name = "Super Admin"
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		return fmt.Errorf("check existing user: %w", err)
	}
	if exists {
		slog.Info("super admin already exists, nothing to do", slog.String("email", email))
		return nil
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO users (email, password_hash, name, role, org_id)
		VALUES ($1, $2, $3, 'super_admin', NULL)`,
		email, hash, name,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("insert super admin: %w", err)
	}

	slog.Info("super admin created", slog.String("email", email))
	return nil
}

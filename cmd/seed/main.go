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
	"github.com/jackc/pgx/v5/pgxpool"

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

	// 1. Super admin (org_id NULL).
	if err := upsertSuperAdmin(ctx, pool, email, password, name); err != nil {
		return err
	}

	// 2. A default organization + organizer account so the M1 organizer flows
	//    are usable before the super-admin org-management screens exist (M4).
	//    Controlled by SEED_ORG_* env; skipped if the organizer email is unset.
	orgName := envDefault("SEED_ORG_NAME", "Laga Demo Org")
	orgSlug := envDefault("SEED_ORG_SLUG", "laga-demo")
	orgEmail := os.Getenv("SEED_ORGANIZER_EMAIL")
	orgPass := os.Getenv("SEED_ORGANIZER_PASSWORD")
	orgUserName := envDefault("SEED_ORGANIZER_NAME", "Organizer")
	if orgEmail != "" && orgPass != "" {
		if err := upsertOrganizer(ctx, pool, orgName, orgSlug, orgEmail, orgPass, orgUserName); err != nil {
			return err
		}
	} else {
		slog.Info("no SEED_ORGANIZER_EMAIL/PASSWORD set, skipping organizer seed")
	}

	return nil
}

func upsertSuperAdmin(ctx context.Context, pool *pgxpool.Pool, email, password, name string) error {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		return fmt.Errorf("check existing user: %w", err)
	}
	if exists {
		slog.Info("super admin already exists", slog.String("email", email))
		return nil
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (email, password_hash, name, role, org_id)
		VALUES ($1, $2, $3, 'super_admin', NULL)`, email, hash, name); err != nil {
		return fmt.Errorf("insert super admin: %w", err)
	}
	slog.Info("super admin created", slog.String("email", email))
	return nil
}

func upsertOrganizer(ctx context.Context, pool *pgxpool.Pool, orgName, orgSlug, email, password, name string) error {
	// Org (idempotent by slug).
	var orgID string
	err := pool.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = $1`, orgSlug).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := pool.QueryRow(ctx, `
			INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING id`,
			orgName, orgSlug).Scan(&orgID); err != nil {
			return fmt.Errorf("insert org: %w", err)
		}
		slog.Info("organization created", slog.String("slug", orgSlug))
	} else if err != nil {
		return fmt.Errorf("check org: %w", err)
	}

	// Organizer user (idempotent by email).
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		return err
	}
	if exists {
		slog.Info("organizer already exists", slog.String("email", email))
		return nil
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (email, password_hash, name, role, org_id)
		VALUES ($1, $2, $3, 'organizer', $4)`, email, hash, name, orgID); err != nil {
		return fmt.Errorf("insert organizer: %w", err)
	}
	slog.Info("organizer created", slog.String("email", email), slog.String("org", orgSlug))
	return nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

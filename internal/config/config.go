// Package config loads and validates all runtime configuration from the
// environment. Configuration is read once at startup; a validation failure is
// fatal so the process never runs half-configured.
package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the fully-resolved application configuration.
type Config struct {
	Env  string `env:"APP_ENV" envDefault:"development"`
	Port int    `env:"PORT" envDefault:"8080"`

	// DatabaseURL is the runtime connection string. On Supabase Free use the
	// transaction pooler endpoint (port 6543) so we stay within the small
	// direct-connection budget of the free tier.
	DatabaseURL string `env:"DATABASE_URL,required"`
	// MigrationDatabaseURL is a DIRECT connection (port 5432) used only by
	// goose migrations, which need session-level features the pooler lacks.
	MigrationDatabaseURL string `env:"MIGRATION_DATABASE_URL"`

	DBMaxConns int32 `env:"DB_MAX_CONNS" envDefault:"8"`

	JWTSecret       string        `env:"JWT_SECRET,required"`
	AccessTokenTTL  time.Duration `env:"ACCESS_TOKEN_TTL" envDefault:"15m"`
	RefreshTokenTTL time.Duration `env:"REFRESH_TOKEN_TTL" envDefault:"720h"` // 30 days

	// CORSOrigins is the comma-separated list of allowed web origins.
	CORSOrigins []string `env:"CORS_ORIGINS" envSeparator:"," envDefault:"http://localhost:5173"`

	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// OTPPepper keys the HMAC that hashes one-time codes. Required once OTP
	// login is live; without it a database reader could brute-force the 10^6
	// code space offline in moments.
	OTPPepper string `env:"OTP_PEPPER"`

	// Plunk delivers transactional email. The key belongs in Fly secrets and
	// the gitignored local .env, and must never reach Vercel, a PUBLIC_
	// variable, source, tests or logs.
	PlunkAPIKey    string `env:"PLUNK_API_KEY"`
	PlunkFromEmail string `env:"PLUNK_FROM_EMAIL"`
	PlunkFromName  string `env:"PLUNK_FROM_NAME" envDefault:"Tourney.social"`
	// PlunkAllowRealSend permits real delivery outside production. Off unless
	// deliberately set, so a stray run cannot email a real person.
	PlunkAllowRealSend bool `env:"PLUNK_ALLOW_REAL_SEND" envDefault:"false"`

	// PasswordLoginEnabled gates POST /auth/login while OTP replaces it.
	//
	// Default OFF: from 00014 onward the intended way in is an emailed code,
	// and leaving the password path live by default would mean a deploy could
	// silently keep accepting credentials the product has retired. It stays in
	// the codebase, and existing argon2id hashes stay in the database, so
	// flipping this back to true is the rollback if OTP delivery fails —
	// no redeploy, no data restore.
	PasswordLoginEnabled bool `env:"AUTH_PASSWORD_LOGIN_ENABLED" envDefault:"false"`
}

// Load parses the environment into a Config and validates it.
func Load() (*Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}
	if cfg.MigrationDatabaseURL == "" {
		// Fall back to the runtime URL if a dedicated migration URL is not set.
		cfg.MigrationDatabaseURL = cfg.DatabaseURL
	}
	if len(cfg.JWTSecret) < 16 {
		return nil, fmt.Errorf("JWT_SECRET must be at least 16 characters")
	}
	// Only enforced when the password path is closed, i.e. when OTP is the
	// only way in. That keeps existing deployments bootable while Phase 5 is
	// still landing, but makes a production config with no way to sign in
	// fail at startup rather than at a user's first attempt.
	if !cfg.PasswordLoginEnabled && len(cfg.OTPPepper) < 16 {
		return nil, fmt.Errorf(
			"OTP_PEPPER must be at least 16 characters when AUTH_PASSWORD_LOGIN_ENABLED is false")
	}
	return &cfg, nil
}

// IsProduction reports whether the app is running in a production environment.
func (c *Config) IsProduction() bool { return c.Env == "production" }

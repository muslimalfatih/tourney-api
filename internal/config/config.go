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
	return &cfg, nil
}

// IsProduction reports whether the app is running in a production environment.
func (c *Config) IsProduction() bool { return c.Env == "production" }

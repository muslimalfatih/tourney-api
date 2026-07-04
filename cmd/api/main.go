// Command api is the laga-api entrypoint. It stays thin: load config, open the
// database, construct each module, wire the router, and run until interrupted.
// All construction lives here so the dependency graph is visible in one place.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/audit"
	"github.com/muslimalfatih/laga-api/internal/auth"
	"github.com/muslimalfatih/laga-api/internal/config"
	"github.com/muslimalfatih/laga-api/internal/draw"
	"github.com/muslimalfatih/laga-api/internal/event"
	"github.com/muslimalfatih/laga-api/internal/match"
	"github.com/muslimalfatih/laga-api/internal/participant"
	"github.com/muslimalfatih/laga-api/internal/platform"
	"github.com/muslimalfatih/laga-api/internal/realtime"
	"github.com/muslimalfatih/laga-api/internal/schedule"
	"github.com/muslimalfatih/laga-api/internal/server"
	"github.com/muslimalfatih/laga-api/internal/server/middleware"
	"github.com/muslimalfatih/laga-api/internal/storage/postgres"
	"github.com/muslimalfatih/laga-api/internal/tournament"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	slog.SetDefault(log)

	// Root context cancelled on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("database connected")

	// --- Construct modules (explicit wiring, no DI container) ---

	tokens := auth.NewTokenService(cfg.JWTSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	authHandler := auth.NewHandler(auth.NewService(auth.NewRepository(pool), tokens))

	hub := realtime.NewHub()
	realtimeHandler := realtime.NewHandler(hub)

	auditHandler := audit.NewHandler(audit.NewService(pool))

	drawService := draw.NewService(pool)
	tournamentHandler := tournament.NewHandler(tournament.NewService(pool))
	eventHandler := event.NewHandler(event.NewService(pool), drawService)
	participantHandler := participant.NewHandler(participant.NewService(pool))
	matchHandler := match.NewHandler(match.NewService(pool), hub)
	scheduleHandler := schedule.NewHandler(schedule.NewService(pool))
	platformHandler := platform.NewHandler(platform.NewService(pool))

	// --- Wire routes via the server's registrar hooks ---

	deps := server.Deps{
		Config:   cfg,
		Log:      log,
		Pool:     pool,
		Verifier: tokens,

		RegisterAuthRoutes: func(rg *gin.RouterGroup, v middleware.TokenVerifier) {
			authHandler.Register(rg, v)
		},
		RegisterPublicRoutes: func(rg *gin.RouterGroup) {
			tournamentHandler.RegisterPublic(rg)
			eventHandler.RegisterPublic(rg)
			participantHandler.RegisterPublic(rg)
			matchHandler.RegisterPublic(rg)
			scheduleHandler.RegisterPublic(rg)
			realtimeHandler.Register(rg)
		},
		RegisterOrganizerRoutes: func(rg *gin.RouterGroup) {
			tournamentHandler.RegisterOrganizer(rg)
			eventHandler.RegisterOrganizer(rg)
			participantHandler.RegisterOrganizer(rg)
			matchHandler.RegisterOrganizer(rg)
			scheduleHandler.RegisterOrganizer(rg)
		},
		RegisterAdminRoutes: func(rg *gin.RouterGroup) {
			platformHandler.RegisterAdmin(rg)
			auditHandler.RegisterAdmin(rg)
		},
	}

	engine := server.New(deps)
	return server.Run(ctx, engine, cfg.Port, log)
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if cfg.IsProduction() {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

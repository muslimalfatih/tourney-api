// Package server assembles the Gin engine, its middleware chain, and the
// /api/v1 route tree. Deps is the explicit dependency set the router wires;
// main.go constructs it. There is no service-locator or DI container — wiring
// is plain constructor calls, which keeps the dependency graph readable.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/laga-api/internal/config"
	"github.com/muslimalfatih/laga-api/internal/server/middleware"
)

// Deps carries everything the router needs to mount routes. It is populated in
// main.go so this package stays free of construction logic.
type Deps struct {
	Config *config.Config
	Log    *slog.Logger
	Pool   *pgxpool.Pool

	// Verifier backs the Auth middleware.
	Verifier middleware.TokenVerifier

	// Route registrars. Each module exposes Register* methods that mount onto
	// a provided group. Using function fields keeps server independent of the
	// concrete handler types and avoids import cycles.
	RegisterAuthRoutes      func(rg *gin.RouterGroup, v middleware.TokenVerifier)
	RegisterPublicRoutes    func(rg *gin.RouterGroup)
	RegisterOrganizerRoutes func(rg *gin.RouterGroup)
	RegisterAdminRoutes     func(rg *gin.RouterGroup)
}

// New builds the configured Gin engine.
func New(deps Deps) *gin.Engine {
	if deps.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(
		middleware.RequestID(),
		middleware.Logger(deps.Log),
		middleware.Recover(deps.Log),
		middleware.CORS(deps.Config.CORSOrigins),
	)

	registerHealth(r, deps.Pool)
	registerAPIv1(r, deps)

	return r
}

// registerHealth mounts liveness and readiness endpoints outside /api/v1 so
// platform probes hit stable paths.
func registerHealth(r *gin.Engine, pool *pgxpool.Pool) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "reason": "database"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
}

// registerAPIv1 builds the versioned route tree. Authorization boundaries are
// applied at the group level: public is open, organizer requires the organizer
// or super_admin role, admin requires super_admin.
func registerAPIv1(r *gin.Engine, deps Deps) {
	v1 := r.Group("/api/v1")

	if deps.RegisterAuthRoutes != nil {
		deps.RegisterAuthRoutes(v1, deps.Verifier)
	}

	// Public read routes — no auth.
	public := v1.Group("/public")
	if deps.RegisterPublicRoutes != nil {
		deps.RegisterPublicRoutes(public)
	}

	// Organizer routes — authenticated, organizer or super admin.
	organizer := v1.Group("")
	organizer.Use(
		middleware.Auth(deps.Verifier),
		middleware.RequireRole(middleware.RoleOrganizer, middleware.RoleSuperAdmin),
	)
	if deps.RegisterOrganizerRoutes != nil {
		deps.RegisterOrganizerRoutes(organizer)
	}

	// Super-admin routes.
	admin := v1.Group("")
	admin.Use(
		middleware.Auth(deps.Verifier),
		middleware.RequireRole(middleware.RoleSuperAdmin),
	)
	if deps.RegisterAdminRoutes != nil {
		deps.RegisterAdminRoutes(admin)
	}
}

// Run starts the HTTP server and blocks until ctx is cancelled, then shuts down
// gracefully within a timeout.
func Run(ctx context.Context, engine *gin.Engine, port int, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              announcedAddr(port),
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.Int("port", port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		log.Info("shutting down http server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func announcedAddr(port int) string { return fmt.Sprintf(":%d", port) }

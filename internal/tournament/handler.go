// Package tournament owns tournament CRUD, publish state, and the public
// read models. This is a skeleton: handlers are wired and return typed shapes
// so the API contract is exercisable end-to-end; the service/repository bodies
// are stubbed pending the milestone-1 implementation.
package tournament

import (
	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/server"
	"github.com/muslimalfatih/laga-api/internal/server/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterPublic mounts unauthenticated read routes consumed by the web SSR
// layer. No auth middleware — these are public by design.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:slug", h.getPublicBySlug)
	rg.GET("/tournaments/:slug/participants", h.listPublicParticipants)
	rg.GET("/tournaments/:slug/schedule", h.getPublicSchedule)
}

// RegisterOrganizer mounts the authenticated organizer routes. The caller wraps
// this group with Auth + RequireRole(organizer, super_admin).
func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments", h.list)
	rg.POST("/tournaments", h.create)
	rg.GET("/tournaments/:id", h.get)
	rg.PATCH("/tournaments/:id", h.update)
	rg.POST("/tournaments/:id/publish", h.publish)
	rg.POST("/tournaments/:id/unpublish", h.unpublish)
}

// --- Public read handlers (stubbed shapes) ---

func (h *Handler) getPublicBySlug(c *gin.Context) {
	slug := c.Param("slug")
	server.OK(c, gin.H{
		"slug":     slug,
		"name":     "Sample Tournament",
		"sport":    "tennis",
		"status":   "published",
		"branding": gin.H{},
		"events":   []any{},
	})
}

func (h *Handler) listPublicParticipants(c *gin.Context) {
	server.List(c, []any{}, server.Meta{Page: 1, PerPage: 20, Total: 0})
}

func (h *Handler) getPublicSchedule(c *gin.Context) {
	server.OK(c, gin.H{"slots": []any{}})
}

// --- Organizer handlers (stubbed) ---

func (h *Handler) list(c *gin.Context) {
	// Organizer sees only their org's tournaments; super admin sees all.
	_ = middleware.OrgID(c)
	server.List(c, []any{}, server.Meta{Page: 1, PerPage: 20, Total: 0})
}

func (h *Handler) create(c *gin.Context) {
	var req CreateTournamentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid tournament payload"))
		return
	}
	server.Created(c, gin.H{"id": "00000000-0000-0000-0000-000000000000", "name": req.Name, "status": "draft"})
}

func (h *Handler) get(c *gin.Context) {
	server.OK(c, gin.H{"id": c.Param("id"), "status": "draft"})
}

func (h *Handler) update(c *gin.Context) {
	server.OK(c, gin.H{"id": c.Param("id")})
}

func (h *Handler) publish(c *gin.Context) {
	server.OK(c, gin.H{"id": c.Param("id"), "status": "published"})
}

func (h *Handler) unpublish(c *gin.Context) {
	server.OK(c, gin.H{"id": c.Param("id"), "status": "draft"})
}

// Package tournament owns tournament CRUD, publish state, and the public
// read models. Handlers are thin: parse + authorize scope + delegate to the
// service, then render via the shared JSON envelope.
package tournament

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/server"
	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterPublic mounts unauthenticated read routes consumed by the web SSR
// layer.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:slug", h.getPublicBySlug)
}

// RegisterOrganizer mounts authenticated organizer routes. The caller wraps
// this group with Auth + RequireRole(organizer, super_admin).
func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments", h.list)
	rg.POST("/tournaments", h.create)
	rg.GET("/tournaments/:id", h.get)
	rg.PATCH("/tournaments/:id", h.update)
	rg.POST("/tournaments/:id/publish", h.publish)
	rg.POST("/tournaments/:id/unpublish", h.unpublish)
}

// orgScope returns the org filter for the caller: a super admin sees all
// (nil), an organizer is restricted to their org.
func orgScope(c *gin.Context) *uuid.UUID {
	if middleware.Role(c) == middleware.RoleSuperAdmin {
		return nil
	}
	id := middleware.OrgID(c)
	return &id
}

// --- Public ---

func (h *Handler) getPublicBySlug(c *gin.Context) {
	t, err := h.svc.GetPublic(c.Request.Context(), c.Param("slug"))
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	// Only expose published tournaments publicly.
	if t.Status != "published" {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	server.OK(c, t)
}

// --- Organizer ---

func (h *Handler) list(c *gin.Context) {
	page, perPage, offset := server.Pagination(c)
	orgID := middleware.OrgID(c)
	// Super admin without an org sees nothing in this org-scoped list; they use
	// the admin endpoints instead. Organizers see their org.
	items, total, err := h.svc.List(c.Request.Context(), orgID, perPage, offset)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	if items == nil {
		items = []Tournament{}
	}
	server.List(c, items, server.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) create(c *gin.Context) {
	var req CreateTournamentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid tournament payload"))
		return
	}
	orgID := middleware.OrgID(c)
	if orgID == uuid.Nil {
		server.Error(c, server.ErrForbidden("only organizers can create tournaments"))
		return
	}
	t, err := h.svc.Create(c.Request.Context(), orgID, req)
	if errors.Is(err, ErrSlugTaken) {
		server.Error(c, server.ErrConflict("that slug is already in use"))
		return
	}
	if handleTimezoneErr(c, err) {
		return
	}
	if err != nil {
		server.Error(c, server.ErrValidation(err.Error()))
		return
	}
	server.Created(c, t)
}

func (h *Handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	t, err := h.svc.Get(c.Request.Context(), id, orgScope(c))
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, t)
}

func (h *Handler) update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	var req UpdateTournamentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid update payload"))
		return
	}
	t, err := h.svc.Update(c.Request.Context(), id, orgScope(c), req)
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if handleTimezoneErr(c, err) {
		return
	}
	if err != nil {
		server.Error(c, server.ErrValidation(err.Error()))
		return
	}
	server.OK(c, t)
}

// handleTimezoneErr renders the Phase 3.6 invalid-timezone contract:
// 422 {"error":{"code":"invalid_timezone","details":{"field":"timezone","value":...}}}
func handleTimezoneErr(c *gin.Context, err error) bool {
	var tzErr *InvalidTimezoneError
	if !errors.As(err, &tzErr) {
		return false
	}
	server.Error(c, (&server.AppError{
		Status: http.StatusUnprocessableEntity, Code: "invalid_timezone",
		Message: "timezone must be a valid IANA zone name (e.g. Asia/Makassar)",
	}).WithDetails(gin.H{"field": "timezone", "value": tzErr.Value}))
	return true
}

func (h *Handler) publish(c *gin.Context) {
	h.setStatus(c, true)
}

func (h *Handler) unpublish(c *gin.Context) {
	h.setStatus(c, false)
}

func (h *Handler) setStatus(c *gin.Context, publish bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	var t *Tournament
	if publish {
		t, err = h.svc.Publish(c.Request.Context(), middleware.UserID(c), id, orgScope(c))
	} else {
		t, err = h.svc.Unpublish(c.Request.Context(), middleware.UserID(c), id, orgScope(c))
	}
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, t)
}

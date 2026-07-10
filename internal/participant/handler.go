// Package participant owns players, teams, and participant entries. A
// participant is the unifying "entry" concept: singles → a player, doubles → a
// team. Seeds drive draw generation.
package participant

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/laga-api/internal/server"
	"github.com/muslimalfatih/laga-api/internal/server/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	// Public roster for an event, only if its tournament is published.
	rg.GET("/events/:id/participants", h.listPublic)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/events/:id/participants", h.list)
	rg.POST("/events/:id/participants", h.add)
	rg.PATCH("/participants/:id/seed", h.setSeed)
	rg.PATCH("/participants/:id", h.rename)
	rg.DELETE("/participants/:id", h.remove)
}

func orgScope(c *gin.Context) *uuid.UUID {
	if middleware.Role(c) == middleware.RoleSuperAdmin {
		return nil
	}
	id := middleware.OrgID(c)
	return &id
}

func (h *Handler) list(c *gin.Context) {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	items, err := h.svc.List(c.Request.Context(), eventID, orgScope(c))
	if handled := respondErr(c, err); handled {
		return
	}
	if items == nil {
		items = []Participant{}
	}
	server.OK(c, items)
}

// listPublic serves the roster for a published tournament's event (no auth).
func (h *Handler) listPublic(c *gin.Context) {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	items, err := h.svc.ListPublic(c.Request.Context(), eventID)
	if handled := respondErr(c, err); handled {
		return
	}
	if items == nil {
		items = []Participant{}
	}
	server.OK(c, items)
}

func (h *Handler) add(c *gin.Context) {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	var req AddParticipantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("a display name is required"))
		return
	}
	p, err := h.svc.Add(c.Request.Context(), eventID, orgScope(c), req)
	if handled := respondErr(c, err); handled {
		return
	}
	server.Created(c, p)
}

func (h *Handler) setSeed(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid participant id"))
		return
	}
	var req SetSeedRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid seed payload"))
		return
	}
	p, err := h.svc.SetSeed(c.Request.Context(), id, orgScope(c), req)
	if handled := respondErr(c, err); handled {
		return
	}
	server.OK(c, p)
}

func (h *Handler) rename(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid participant id"))
		return
	}
	var req RenameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("a display name is required"))
		return
	}
	p, err := h.svc.Rename(c.Request.Context(), id, orgScope(c), req)
	if handled := respondErr(c, err); handled {
		return
	}
	server.OK(c, p)
}

func (h *Handler) remove(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid participant id"))
		return
	}
	err = h.svc.Delete(c.Request.Context(), id, orgScope(c))
	if handled := respondErr(c, err); handled {
		return
	}
	server.NoContent(c)
}

// respondErr maps service errors to responses; returns true if it wrote one.
func respondErr(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		server.Error(c, server.ErrNotFound("not found"))
	case errors.Is(err, ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
	case errors.Is(err, ErrValidation):
		server.Error(c, server.ErrValidation("a display name is required"))
	default:
		server.Error(c, server.ErrInternal(""))
	}
	return true
}

// Package schedule owns courts and schedule slots (assigning matches to a court
// at a time). The public read exposes a published tournament's schedule.
package schedule

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
	rg.GET("/tournaments/:slug/schedule", h.listPublic)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:id/courts", h.listCourts)
	rg.POST("/tournaments/:id/courts", h.createCourt)
	rg.GET("/tournaments/:id/schedule", h.listSlots)
	rg.POST("/schedule/slots", h.createSlot)
	rg.DELETE("/schedule/slots/:id", h.deleteSlot)
}

func orgScope(c *gin.Context) *uuid.UUID {
	if middleware.Role(c) == middleware.RoleSuperAdmin {
		return nil
	}
	id := middleware.OrgID(c)
	return &id
}

func handle(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		server.Error(c, server.ErrNotFound("not found"))
	case errors.Is(err, ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
	default:
		server.Error(c, server.ErrValidation(err.Error()))
	}
	return true
}

func (h *Handler) listCourts(c *gin.Context) {
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	courts, err := h.svc.ListCourts(c.Request.Context(), tid, orgScope(c))
	if handle(c, err) {
		return
	}
	server.OK(c, courts)
}

func (h *Handler) createCourt(c *gin.Context) {
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	var req CreateCourtRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("a court name is required"))
		return
	}
	court, err := h.svc.CreateCourt(c.Request.Context(), tid, orgScope(c), req)
	if handle(c, err) {
		return
	}
	server.Created(c, court)
}

func (h *Handler) listSlots(c *gin.Context) {
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	slots, err := h.svc.ListSlots(c.Request.Context(), tid, orgScope(c))
	if handle(c, err) {
		return
	}
	server.OK(c, slots)
}

func (h *Handler) createSlot(c *gin.Context) {
	var req CreateSlotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid slot payload"))
		return
	}
	slot, err := h.svc.CreateSlot(c.Request.Context(), orgScope(c), req)
	if handle(c, err) {
		return
	}
	server.Created(c, slot)
}

func (h *Handler) deleteSlot(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid slot id"))
		return
	}
	if handle(c, h.svc.DeleteSlot(c.Request.Context(), id, orgScope(c))) {
		return
	}
	server.NoContent(c)
}

func (h *Handler) listPublic(c *gin.Context) {
	slots, err := h.svc.ListPublicSlots(c.Request.Context(), c.Param("slug"))
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, slots)
}

// Package schedule owns courts and schedule slots (assigning matches to a court
// at a time). The public read exposes a published tournament's schedule.
package schedule

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/realtime"
	"github.com/muslimalfatih/tourney-api/internal/server"
	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

type Handler struct {
	svc *Service
	hub *realtime.Hub
}

func NewHandler(svc *Service, hub *realtime.Hub) *Handler {
	return &Handler{svc: svc, hub: hub}
}

// publish broadcasts a schedule change on the tournament's public stream.
// Empty slug = draft tournament; persist but never broadcast.
func (h *Handler) publish(slug string, data any) {
	if slug != "" {
		h.hub.Publish(slug, realtime.Event{Name: "schedule.updated", Data: data})
	}
}

func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:slug/schedule", h.listPublic)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:id/courts", h.listCourts)
	rg.POST("/tournaments/:id/courts", h.createCourt)
	rg.GET("/tournaments/:id/schedule", h.listSlots)
	rg.POST("/schedule/slots", h.createSlot)
	rg.PATCH("/schedule/slots/:id", h.updateSlot)
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
	var ce *ConflictError
	switch {
	case err == nil:
		return false
	case errors.As(err, &ce):
		if len(ce.Hard) > 0 {
			// Hard conflicts always block; any rest warnings ride along so the
			// organizer sees the full picture in one response.
			server.Error(c, (&server.AppError{
				Status: http.StatusUnprocessableEntity, Code: "schedule_conflict",
				Message: "the requested time conflicts with existing schedule slots",
			}).WithDetails(gin.H{"conflicts": ce.Hard, "warnings": ce.Rest}))
		} else {
			server.Error(c, (&server.AppError{
				Status: http.StatusUnprocessableEntity, Code: "insufficient_rest",
				Message: "participants would get less than the minimum rest between matches; resubmit with override_rest_buffer to schedule anyway",
			}).WithDetails(gin.H{"warnings": ce.Rest}))
		}
	case errors.Is(err, ErrStateConflict):
		server.Error(c, &server.AppError{
			Status: http.StatusConflict, Code: "schedule_state_conflict",
			Message: "the schedule changed while saving; reload and retry",
		})
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
	slot, slug, err := h.svc.CreateSlot(c.Request.Context(), middleware.UserID(c), orgScope(c), req)
	if handle(c, err) {
		return
	}
	h.publish(slug, slot)
	server.Created(c, slot)
}

func (h *Handler) updateSlot(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid slot id"))
		return
	}
	var req UpdateSlotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid slot payload"))
		return
	}
	slot, slug, err := h.svc.UpdateSlot(c.Request.Context(), id, middleware.UserID(c), orgScope(c), req)
	if handle(c, err) {
		return
	}
	h.publish(slug, slot)
	server.OK(c, slot)
}

func (h *Handler) deleteSlot(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid slot id"))
		return
	}
	slug, err := h.svc.DeleteSlot(c.Request.Context(), id, middleware.UserID(c), orgScope(c))
	if handle(c, err) {
		return
	}
	h.publish(slug, gin.H{"deleted_slot_id": id})
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

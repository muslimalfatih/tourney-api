// Package match owns matches, match scores, winner advancement, and SSE
// broadcast. Score entry is a normal HTTP call; after persisting, it publishes
// a realtime event to the tournament's topic so public viewers see live updates.
package match

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/laga-api/internal/realtime"
	"github.com/muslimalfatih/laga-api/internal/server"
	"github.com/muslimalfatih/laga-api/internal/server/middleware"
)

type Handler struct {
	svc *Service
	hub *realtime.Hub
}

func NewHandler(svc *Service, hub *realtime.Hub) *Handler {
	return &Handler{svc: svc, hub: hub}
}

func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/matches/:id", h.getPublic)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/events/:id/matches", h.listForEvent)
	rg.PATCH("/matches/:id/score", h.updateScore)
	rg.PATCH("/matches/:id/status", h.updateStatus)
}

func orgScope(c *gin.Context) *uuid.UUID {
	if middleware.Role(c) == middleware.RoleSuperAdmin {
		return nil
	}
	id := middleware.OrgID(c)
	return &id
}

func (h *Handler) getPublic(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid match id"))
		return
	}
	m, err := h.svc.GetPublic(c.Request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("match not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, m)
}

func (h *Handler) listForEvent(c *gin.Context) {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	items, err := h.svc.ListForEvent(c.Request.Context(), eventID)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	if items == nil {
		items = []Match{}
	}
	server.OK(c, items)
}

// updateScore persists a score and, if completing, advances the bracket; then
// broadcasts over SSE so the public bracket/match pages update live.
func (h *Handler) updateScore(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid match id"))
		return
	}
	var req ScoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid score payload"))
		return
	}
	m, slug, err := h.svc.SubmitScore(c.Request.Context(), id, middleware.UserID(c), orgScope(c), req)
	if handled := respondErr(c, err); handled {
		return
	}
	// Empty slug = draft tournament or hidden division; persist but never
	// broadcast on the public stream (see Service.SubmitScore).
	if slug != "" {
		h.hub.Publish(slug, realtime.Event{Name: "match.score", Data: m})
	}
	server.OK(c, m)
}

func (h *Handler) updateStatus(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid match id"))
		return
	}
	var req StatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid status payload"))
		return
	}
	m, slug, err := h.svc.SetStatus(c.Request.Context(), id, orgScope(c), req)
	if handled := respondErr(c, err); handled {
		return
	}
	if slug != "" {
		h.hub.Publish(slug, realtime.Event{Name: "match.status", Data: m})
	}
	server.OK(c, m)
}

func respondErr(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		server.Error(c, server.ErrNotFound("match not found"))
	case errors.Is(err, ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
	case func() bool { var iv *InvalidScoreError; return errors.As(err, &iv) }():
		var iv *InvalidScoreError
		errors.As(err, &iv)
		server.Error(c, (&server.AppError{
			Status: http.StatusUnprocessableEntity, Code: "invalid_score",
			Message: "the submitted score is not a legal result for this division",
		}).WithDetails(map[string]any{"violations": iv.Violations}))
	case errors.Is(err, ErrCompletedImmutable):
		server.Error(c, &server.AppError{
			Status: http.StatusConflict, Code: "completed_immutable",
			Message: ErrCompletedImmutable.Error(),
		})
	case func() bool { var dl *DownstreamLockedError; return errors.As(err, &dl) }():
		var dl *DownstreamLockedError
		errors.As(err, &dl)
		server.Error(c, (&server.AppError{
			Status: http.StatusConflict, Code: "downstream_phase_locked",
			Message: "downstream matches have started; correct or reset them before changing this result",
		}).WithDetails(map[string]any{"locked": dl.Locked}))
	default:
		server.Error(c, server.ErrInternal(""))
	}
	return true
}

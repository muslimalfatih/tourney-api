// Package event owns events (divisions), stages and groups within a tournament,
// and (via the draw module) draw generation for an event.
package event

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/laga-api/internal/draw"
	"github.com/muslimalfatih/laga-api/internal/server"
	"github.com/muslimalfatih/laga-api/internal/server/middleware"
)

type Handler struct {
	svc  *Service
	draw *draw.Service
}

func NewHandler(svc *Service, drawSvc *draw.Service) *Handler {
	return &Handler{svc: svc, draw: drawSvc}
}

func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/events/:id/bracket", h.getBracket)
	rg.GET("/events/:id/standings", h.getPublicStandings)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:id/events", h.list)
	rg.POST("/tournaments/:id/events", h.create)
	rg.GET("/events/:id", h.get)
	rg.DELETE("/events/:id", h.remove)
	rg.POST("/events/:id/draw", h.generateDraw)
}

func orgScope(c *gin.Context) *uuid.UUID {
	if middleware.Role(c) == middleware.RoleSuperAdmin {
		return nil
	}
	id := middleware.OrgID(c)
	return &id
}

// --- Organizer ---

func (h *Handler) list(c *gin.Context) {
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	items, err := h.svc.ListForTournament(c.Request.Context(), tid, orgScope(c))
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	if items == nil {
		items = []Event{}
	}
	server.OK(c, items)
}

func (h *Handler) create(c *gin.Context) {
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	var req CreateEventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid event payload"))
		return
	}
	ev, err := h.svc.Create(c.Request.Context(), tid, orgScope(c), req)
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.Created(c, ev)
}

func (h *Handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	ev, err := h.svc.Get(c.Request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, ev)
}

func (h *Handler) remove(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	err = h.svc.Delete(c.Request.Context(), id, orgScope(c))
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.NoContent(c)
}

// generateDraw runs the format generator for an event and persists the bracket.
func (h *Handler) generateDraw(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	count, err := h.draw.Generate(c.Request.Context(), id, orgScope(c))
	switch {
	case errors.Is(err, draw.ErrNotFound):
		server.Error(c, server.ErrNotFound("event not found"))
		return
	case errors.Is(err, draw.ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
		return
	case errors.Is(err, draw.ErrNotEnough):
		server.Error(c, server.ErrValidation("need at least 2 participants"))
		return
	case errors.Is(err, draw.ErrUnsupportedForm):
		server.Error(c, server.ErrValidation("only single elimination is supported for now"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": id, "matches": count, "generated": true})
}

// --- Public ---

func (h *Handler) getBracket(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	b, err := h.draw.GetBracket(c.Request.Context(), id)
	if errors.Is(err, draw.ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, b)
}

func (h *Handler) getPublicStandings(c *gin.Context) {
	// Round-robin/group standings land with the draw slice; contract stub for now.
	server.OK(c, gin.H{"event_id": c.Param("id"), "groups": []any{}})
}

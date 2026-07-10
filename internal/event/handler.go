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
	rg.GET("/events/:id/groups", h.getGroupKnockout)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:id/events", h.list)
	rg.POST("/tournaments/:id/events", h.create)
	rg.GET("/events/:id", h.get)
	rg.DELETE("/events/:id", h.remove)
	rg.POST("/events/:id/draw", h.generateDraw)
	rg.POST("/events/:id/bracket/build", h.buildBracket)
	rg.POST("/events/:id/resolve-groups", h.resolveGroups)
}

// buildBracketRequest is the Match-builder payload. mode is "auto" or "manual";
// for manual, matches carries the round-1 pairings (team ids, either optional
// for a bye). Court/time are intentionally not accepted here — scheduling is
// done per-match from the Bracket tab (M1 scope).
type buildBracketRequest struct {
	PairingMode string      `json:"pairing_mode" binding:"required,oneof=auto manual"`
	Matches     []draw.Pair `json:"matches"`
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

// buildBracket runs the Match builder: builds a single-elim bracket from either
// random pairs (auto) or the organizer's explicit round-1 pairings (manual),
// overwriting any existing draw.
func (h *Handler) buildBracket(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	var req buildBracketRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid build payload"))
		return
	}
	count, err := h.draw.Build(c.Request.Context(), id, orgScope(c), draw.BuildInput{
		Mode:  req.PairingMode,
		Pairs: req.Matches,
	})
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
	case errors.Is(err, draw.ErrInvalidPairs):
		server.Error(c, server.ErrValidation("pairings are invalid: a team is used twice, a match is half-filled, or an unknown team was supplied"))
		return
	case errors.Is(err, draw.ErrUnsupportedForm):
		server.Error(c, server.ErrValidation("the match builder supports single elimination only"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": id, "matches": count, "pairing_mode": req.PairingMode, "generated": true})
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
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	standings, err := h.draw.GetStandings(c.Request.Context(), id)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": id, "standings": standings})
}

// resolveGroups fills the knockout placeholders with the group finishers.
func (h *Handler) resolveGroups(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	filled, err := h.draw.ResolveGroups(c.Request.Context(), id, orgScope(c))
	switch {
	case errors.Is(err, draw.ErrNotFound):
		server.Error(c, server.ErrNotFound("event not found"))
		return
	case errors.Is(err, draw.ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": id, "filled": filled})
}

func (h *Handler) getGroupKnockout(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	gk, err := h.draw.GetGroupKnockout(c.Request.Context(), id)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gk)
}

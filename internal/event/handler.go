// Package event owns events (divisions), stages and groups within a tournament,
// and draw generation for an event. Skeleton: routes wired, bodies land in
// milestone 1.
package event

import (
	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/server"
)

type Handler struct{}

func NewHandler() *Handler { return &Handler{} }

func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/events/:id/bracket", h.getPublicBracket)
	rg.GET("/events/:id/standings", h.getPublicStandings)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:id/events", h.list)
	rg.POST("/tournaments/:id/events", h.create)
	rg.POST("/events/:id/draw", h.generateDraw)
}

// getPublicBracket returns the canonical bracket tree the custom Svelte renderer
// consumes. The shape here is the contract; the frontend types are generated
// from it.
func (h *Handler) getPublicBracket(c *gin.Context) {
	server.OK(c, gin.H{
		"event_id": c.Param("id"),
		"format":   "single_elim",
		"rounds":   []any{},
	})
}

func (h *Handler) getPublicStandings(c *gin.Context) {
	server.OK(c, gin.H{"event_id": c.Param("id"), "groups": []any{}})
}

func (h *Handler) list(c *gin.Context) {
	server.List(c, []any{}, server.Meta{Page: 1, PerPage: 20, Total: 0})
}

func (h *Handler) create(c *gin.Context) {
	server.Created(c, gin.H{"id": "00000000-0000-0000-0000-000000000000"})
}

// generateDraw kicks off format-specific draw generation (draw package).
func (h *Handler) generateDraw(c *gin.Context) {
	server.OK(c, gin.H{"event_id": c.Param("id"), "generated": true})
}

// Package participant owns players, teams, registrations and seedings. A
// participant is the unifying "entry" concept: singles points at a player,
// doubles at a team. Skeleton: routes wired, bodies land in milestone 1.
package participant

import (
	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/server"
)

type Handler struct{}

func NewHandler() *Handler { return &Handler{} }

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/events/:id/participants", h.list)
	rg.POST("/events/:id/participants", h.create)
	rg.DELETE("/participants/:id", h.remove)
}

func (h *Handler) list(c *gin.Context) {
	server.List(c, []any{}, server.Meta{Page: 1, PerPage: 20, Total: 0})
}

func (h *Handler) create(c *gin.Context) {
	server.Created(c, gin.H{"id": "00000000-0000-0000-0000-000000000000"})
}

func (h *Handler) remove(c *gin.Context) {
	server.NoContent(c)
}

// Package schedule owns venues, courts and schedule slots. Skeleton: routes
// wired, bodies land in milestone 2.
package schedule

import (
	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/server"
)

type Handler struct{}

func NewHandler() *Handler { return &Handler{} }

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:id/courts", h.listCourts)
	rg.POST("/tournaments/:id/courts", h.createCourt)
	rg.POST("/schedule/slots", h.createSlot)
	rg.PATCH("/schedule/slots/:id", h.updateSlot)
}

func (h *Handler) listCourts(c *gin.Context) {
	server.List(c, []any{}, server.Meta{Page: 1, PerPage: 20, Total: 0})
}

func (h *Handler) createCourt(c *gin.Context) {
	server.Created(c, gin.H{"id": "00000000-0000-0000-0000-000000000000"})
}

func (h *Handler) createSlot(c *gin.Context) {
	server.Created(c, gin.H{"id": "00000000-0000-0000-0000-000000000000"})
}

func (h *Handler) updateSlot(c *gin.Context) {
	server.OK(c, gin.H{"id": c.Param("id")})
}

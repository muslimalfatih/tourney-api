// Package match owns matches, match participants, scores and winner
// advancement. Score entry publishes a realtime event so public viewers see
// live updates over SSE. Skeleton: routes wired, bodies land in milestone 2.
package match

import (
	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/realtime"
	"github.com/muslimalfatih/laga-api/internal/server"
)

type Handler struct {
	hub *realtime.Hub
}

func NewHandler(hub *realtime.Hub) *Handler {
	return &Handler{hub: hub}
}

func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/matches/:id", h.getPublic)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.PATCH("/matches/:id/score", h.updateScore)
	rg.PATCH("/matches/:id/status", h.updateStatus)
}

func (h *Handler) getPublic(c *gin.Context) {
	server.OK(c, gin.H{"id": c.Param("id"), "status": "pending", "sets": []any{}})
}

// updateScore is a standard HTTP call (NOT websocket). After persisting, it
// publishes to the tournament topic so SSE subscribers get the live update.
func (h *Handler) updateScore(c *gin.Context) {
	id := c.Param("id")
	// TODO(m2): persist score, recompute winner, advance bracket.
	h.hub.Publish("sample-slug", realtime.Event{
		Name: "match.score",
		Data: gin.H{"match_id": id},
	})
	server.OK(c, gin.H{"id": id})
}

func (h *Handler) updateStatus(c *gin.Context) {
	server.OK(c, gin.H{"id": c.Param("id")})
}

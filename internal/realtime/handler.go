package realtime

import (
	"io"

	"github.com/gin-gonic/gin"
)

// Handler serves the public SSE stream for a tournament.
type Handler struct {
	hub *Hub
}

func NewHandler(hub *Hub) *Handler {
	return &Handler{hub: hub}
}

// Register mounts the public stream route. It is intentionally under the public
// group: live scores are public data and require no auth.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:slug/stream", h.stream)
}

// stream holds an SSE connection open, relaying events published to the
// tournament's topic. Gin's c.Stream drives the write loop and returns when the
// client disconnects, at which point the deferred unsubscribe runs.
func (h *Handler) stream(c *gin.Context) {
	slug := c.Param("slug")

	events, unsubscribe := h.hub.Subscribe(slug)
	defer unsubscribe()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // disable proxy buffering

	// Nudge the client so EventSource fires onopen immediately.
	c.SSEvent("connected", gin.H{"topic": slug})
	c.Writer.Flush()

	c.Stream(func(w io.Writer) bool {
		event, ok := <-events
		if !ok {
			return false // channel closed → stop streaming
		}
		payload, err := event.Encode()
		if err != nil {
			return true // skip a bad event, keep the stream alive
		}
		c.SSEvent(event.Name, string(payload))
		return true
	})
}

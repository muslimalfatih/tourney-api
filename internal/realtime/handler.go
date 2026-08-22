package realtime

import (
	"context"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// PublishedFunc reports whether the tournament behind a slug is published.
// Injected (rather than giving this package a DB) so the stream can refuse
// connections for draft/archived tournaments — without it, anyone holding a
// draft slug could watch its live scores before publication.
type PublishedFunc func(ctx context.Context, slug string) (bool, error)

// Handler serves the public SSE stream for a tournament.
type Handler struct {
	hub         *Hub
	isPublished PublishedFunc
}

func NewHandler(hub *Hub, isPublished PublishedFunc) *Handler {
	return &Handler{hub: hub, isPublished: isPublished}
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

	// Gate on publication before subscribing. Defense in depth: the publishers
	// in internal/match also skip broadcasting for unpublished tournaments and
	// hidden divisions, so a stream that was open when a tournament got
	// unpublished goes silent rather than leaking.
	ok, err := h.isPublished(c.Request.Context(), slug)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "internal_error", "message": "an unexpected error occurred"},
		})
		return
	}
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": gin.H{"code": "not_found", "message": "tournament not found"},
		})
		return
	}

	events, unsubscribe := h.hub.Subscribe(slug)
	defer unsubscribe()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // disable proxy buffering

	// Nudge the client so EventSource fires onopen immediately.
	c.SSEvent("connected", gin.H{"topic": slug})
	c.Writer.Flush()

	// Select on the request context as well as the event channel: without it,
	// a disconnected client's handler goroutine stays blocked on the channel
	// until the NEXT event for this tournament happens to arrive — leaking the
	// subscription and the connection in the meantime (and deadlocking
	// graceful shutdown, which waits for in-flight handlers). Surfaced by the
	// integration suite, whose httptest server Close hung on exactly this.
	ctx := c.Request.Context()
	c.Stream(func(w io.Writer) bool {
		select {
		case <-ctx.Done():
			return false // client went away → release the subscription
		case event, ok := <-events:
			if !ok {
				return false // channel closed → stop streaming
			}
			payload, err := event.Encode()
			if err != nil {
				return true // skip a bad event, keep the stream alive
			}
			c.SSEvent(event.Name, string(payload))
			return true
		}
	})
}

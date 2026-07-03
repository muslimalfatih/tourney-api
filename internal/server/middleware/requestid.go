package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestID attaches a unique id to every request, echoing an inbound
// X-Request-Id if the client supplied one. The id is stored on the context and
// returned in the response header so logs and clients can correlate.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(ctxRequestID, id)
		c.Header("X-Request-Id", id)
		c.Next()
	}
}

// RequestIDFrom returns the request id stored on the context, if any.
func RequestIDFrom(c *gin.Context) string {
	if v, ok := c.Get(ctxRequestID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

package middleware

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Recover converts any panic in a handler into a 500 JSON error envelope,
// logging the panic with its request id. Without this a panic would leak a
// stack trace to the client and kill the connection.
func Recover(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered",
					slog.Any("panic", r),
					slog.String("request_id", RequestIDFrom(c)),
					slog.String("path", c.Request.URL.Path),
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"code":    "internal_error",
						"message": "an unexpected error occurred",
					},
				})
			}
		}()
		c.Next()
	}
}

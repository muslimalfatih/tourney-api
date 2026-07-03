package middleware

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"
)

// CORS applies cross-origin headers for browser clients.
//
// Semantics matter here: CORS is a browser enforcement mechanism, not server
// authorization. We must NOT hard-reject a request just because its Origin is
// unknown — doing so also breaks legitimate server-to-server calls (e.g.
// SvelteKit SSR load functions, whose internal fetch may send an https Origin).
// Instead we echo the allow headers only for allowed origins and let the
// browser block disallowed ones; non-browser callers proceed unaffected.
//
// Preflight (OPTIONS) requests are answered directly.
func CORS(origins []string) gin.HandlerFunc {
	allowAll := slices.Contains(origins, "*")

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		if origin != "" && (allowAll || slices.Contains(origins, origin)) {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, X-Request-Id")
			c.Header("Access-Control-Expose-Headers", "X-Request-Id")
			c.Header("Access-Control-Max-Age", "43200")
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

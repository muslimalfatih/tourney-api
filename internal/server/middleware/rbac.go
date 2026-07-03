package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Roles. Kept as plain strings so they round-trip cleanly through JWT claims
// and the users.role column without an enum-mapping layer.
const (
	RoleSuperAdmin = "super_admin"
	RoleOrganizer  = "organizer"
)

// RequireRole guards a route to one of the given roles. It must run after Auth.
// Authorization is enforced HERE, in the API — the frontend's role checks are
// UX only and are never trusted.
func RequireRole(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(c *gin.Context) {
		if _, ok := allowed[Role(c)]; !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"code":    "forbidden",
					"message": "you do not have permission to perform this action",
				},
			})
			return
		}
		c.Next()
	}
}

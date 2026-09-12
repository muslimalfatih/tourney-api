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

// RequireSuperAdmin guards the platform surface. Two checks, both needed:
// the effective role must be super_admin, AND the request must not be an
// impersonation. The second is belt-and-braces — an impersonated request's
// effective role is organizer, so the first check already fails — but stating
// it here means the rule survives a future change to how roles are resolved.
func RequireSuperAdmin() gin.HandlerFunc {
	requireRole := RequireRole(RoleSuperAdmin)
	return func(c *gin.Context) {
		if IsImpersonating(c) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"code":    "forbidden",
					"message": "platform controls are unavailable while impersonating",
				},
			})
			return
		}
		requireRole(c)
	}
}

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

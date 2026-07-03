package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Claims is the authenticated identity extracted from a verified access token.
// OrgID is nil for super admins, who are not scoped to an organization.
type Claims struct {
	UserID uuid.UUID
	Role   string
	OrgID  *uuid.UUID
}

// TokenVerifier verifies a raw access token and returns its claims. The auth
// package implements this; middleware depends on the interface so it does not
// import the auth package (which would create an import cycle).
type TokenVerifier interface {
	VerifyAccessToken(raw string) (*Claims, error)
}

// abortUnauthorized writes a 401 JSON envelope and stops the chain.
func abortUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{"code": "unauthorized", "message": msg},
	})
}

// Auth requires a valid Bearer access token. On success it stores the identity
// on the context for downstream handlers and RBAC. The web app forwards the
// token from its httpOnly cookie on server-side fetches, so this middleware is
// the single enforcement point regardless of how the client stores it.
func Auth(verifier TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			abortUnauthorized(c, "missing authorization header")
			return
		}
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			abortUnauthorized(c, "malformed authorization header")
			return
		}

		claims, err := verifier.VerifyAccessToken(parts[1])
		if err != nil {
			abortUnauthorized(c, "invalid or expired token")
			return
		}

		c.Set(ctxUserID, claims.UserID)
		c.Set(ctxUserRole, claims.Role)
		if claims.OrgID != nil {
			c.Set(ctxOrgID, *claims.OrgID)
		}
		c.Next()
	}
}

// Identity accessors for handlers. These assume Auth ran first.

func UserID(c *gin.Context) uuid.UUID {
	if v, ok := c.Get(ctxUserID); ok {
		if id, ok := v.(uuid.UUID); ok {
			return id
		}
	}
	return uuid.Nil
}

func Role(c *gin.Context) string {
	if v, ok := c.Get(ctxUserRole); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// OrgID returns the caller's organization id, or uuid.Nil for super admins.
func OrgID(c *gin.Context) uuid.UUID {
	if v, ok := c.Get(ctxOrgID); ok {
		if id, ok := v.(uuid.UUID); ok {
			return id
		}
	}
	return uuid.Nil
}

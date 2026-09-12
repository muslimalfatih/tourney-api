package middleware

import (
	"context"
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
	// SessionID is the auth_sessions row backing this token. Since 00013 a
	// token is only as good as its session: the verifier resolves this on
	// every request, which is what makes logout and suspension take effect
	// immediately instead of whenever the access token happens to expire.
	SessionID uuid.UUID
	// ActorUserID is set only for impersonation sessions: the super admin who
	// is really acting, while UserID/Role/OrgID describe the organizer the
	// request runs AS. Nil for every normal session.
	ActorUserID *uuid.UUID
}

// IsImpersonation reports whether this request runs under a borrowed identity.
func (c *Claims) IsImpersonation() bool { return c.ActorUserID != nil }

// TokenVerifier verifies a raw access token and returns its claims. The auth
// package implements this; middleware depends on the interface so it does not
// import the auth package (which would create an import cycle).
// It takes a context because verification now touches the database to resolve
// the session, not just the token's signature.
type TokenVerifier interface {
	VerifyAccessToken(ctx context.Context, raw string) (*Claims, error)
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

		claims, err := verifier.VerifyAccessToken(c.Request.Context(), parts[1])
		if err != nil {
			abortUnauthorized(c, "invalid or expired token")
			return
		}

		c.Set(ctxUserID, claims.UserID)
		c.Set(ctxUserRole, claims.Role)
		c.Set(ctxSessionID, claims.SessionID)
		if claims.ActorUserID != nil {
			c.Set(ctxActorUserID, *claims.ActorUserID)
		}
		if claims.OrgID != nil {
			c.Set(ctxOrgID, *claims.OrgID)
		}
		// Attribution rides on the request context so audit writes deep in a
		// service — which only ever see ctx, not gin — still know who really
		// acted. This is what makes "every mutation during impersonation
		// records both people" a property of the system rather than a
		// discipline every handler has to remember.
		actor, effective, session := Attribution(c)
		c.Request = c.Request.WithContext(WithAttribution(c.Request.Context(),
			RequestAttribution{Actor: actor, Effective: effective, Session: session}))
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

// SessionID returns the caller's auth_sessions id, or uuid.Nil if Auth did not
// run. Handlers use it to revoke the current session on logout.
func SessionID(c *gin.Context) uuid.UUID {
	if v, ok := c.Get(ctxSessionID); ok {
		if id, ok := v.(uuid.UUID); ok {
			return id
		}
	}
	return uuid.Nil
}

// ActorUserID returns the real human behind an impersonated request, or
// uuid.Nil when nobody is impersonating.
func ActorUserID(c *gin.Context) uuid.UUID {
	if v, ok := c.Get(ctxActorUserID); ok {
		if id, ok := v.(uuid.UUID); ok {
			return id
		}
	}
	return uuid.Nil
}

// IsImpersonating reports whether the request runs under a borrowed identity.
func IsImpersonating(c *gin.Context) bool { return ActorUserID(c) != uuid.Nil }

// attributionKey carries request attribution inside context.Context, so it
// travels with c.Request.Context() into every service and repository without
// a signature change.
type attributionKey struct{}

// RequestAttribution is the identity pair behind a request.
type RequestAttribution struct {
	Actor     uuid.UUID  // who really acted
	Effective *uuid.UUID // whom the request ran as, during impersonation
	Session   *uuid.UUID // the impersonation session, during impersonation
}

// WithAttribution stores attribution on a context.
func WithAttribution(ctx context.Context, a RequestAttribution) context.Context {
	return context.WithValue(ctx, attributionKey{}, a)
}

// AttributionFrom reads attribution back, reporting whether any was set.
func AttributionFrom(ctx context.Context) (RequestAttribution, bool) {
	a, ok := ctx.Value(attributionKey{}).(RequestAttribution)
	return a, ok
}

// Attribution is what every audit writer needs, in one call:
//
//	actor     — who really did this (the super admin during impersonation)
//	effective — the identity it ran as (the organizer), nil otherwise
//	session   — the impersonation session, nil otherwise
//
// Having one helper rather than three accessors means a mutation cannot record
// the actor and forget the effective user, which is the mistake this exists to
// make impossible.
func Attribution(c *gin.Context) (actor uuid.UUID, effective *uuid.UUID, session *uuid.UUID) {
	user := UserID(c)
	if a := ActorUserID(c); a != uuid.Nil {
		sid := SessionID(c)
		return a, &user, &sid
	}
	return user, nil, nil
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

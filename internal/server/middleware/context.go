// Package middleware holds the Gin middleware chain: request identity, logging,
// panic recovery, CORS, authentication and role-based authorization.
package middleware

// Keys used to store values in the gin.Context. Handlers read these via the
// typed accessors in auth.go rather than reaching for raw string keys.
const (
	ctxRequestID = "request_id"
	ctxUserID    = "user_id"
	ctxUserRole  = "user_role"
	ctxOrgID     = "org_id"
)

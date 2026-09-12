// Package testhooks exists for exactly one purpose: letting the browser-driven
// e2e suite read back a sign-in code it cannot obtain any other way.
//
// otp_challenges never stores a plaintext code, only an irreversible
// HMAC-SHA256 hash (internal/auth/otp.go) -- that is a deliberate security
// property, not an oversight, so a database query can never recover one. A
// human reads the code from an email; a Playwright test has no inbox to read,
// so this hands back exactly what a FakeSender already recorded in this
// process's memory for the automated run.
//
// This package must never run in production. Register mounts nothing unless
// the caller explicitly opts in, and cmd/api/main.go only calls Register when
// E2E_TEST_MODE=true, which config.Load() itself refuses to accept alongside
// APP_ENV=production -- two independent places would have to agree to get
// this wrong.
package testhooks

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/tourney-api/internal/email"
)

// Register mounts the test-only routes directly on the engine, deliberately
// outside /api/v1 and outside every auth middleware group -- this is
// infrastructure for the test harness, not a feature with an authorization
// story, and putting it anywhere near the real route tree would blur that.
func Register(r *gin.Engine, sender *email.FakeSender) {
	r.GET("/internal/test/last-otp", func(c *gin.Context) {
		to := c.Query("email")
		if to == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "email query param required"})
			return
		}
		code := sender.LastCodeFor(to)
		if code == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": "no code recorded for that address"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": code})
	})
}

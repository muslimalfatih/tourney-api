// Package platform owns super-admin operations: organizations, org users, and
// global tournament oversight (suspend/archive). Skeleton: routes wired, bodies
// land in milestone 4.
package platform

import (
	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/server"
)

type Handler struct{}

func NewHandler() *Handler { return &Handler{} }

// RegisterAdmin mounts super-admin-only routes. The caller wraps this group
// with Auth + RequireRole(super_admin).
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	rg.GET("/admin/organizations", h.listOrgs)
	rg.POST("/admin/organizations", h.createOrg)
	rg.GET("/admin/tournaments", h.listTournaments)
	rg.POST("/admin/tournaments/:id/suspend", h.suspendTournament)
}

func (h *Handler) listOrgs(c *gin.Context) {
	server.List(c, []any{}, server.Meta{Page: 1, PerPage: 20, Total: 0})
}

func (h *Handler) createOrg(c *gin.Context) {
	server.Created(c, gin.H{"id": "00000000-0000-0000-0000-000000000000"})
}

func (h *Handler) listTournaments(c *gin.Context) {
	server.List(c, []any{}, server.Meta{Page: 1, PerPage: 20, Total: 0})
}

func (h *Handler) suspendTournament(c *gin.Context) {
	server.OK(c, gin.H{"id": c.Param("id"), "status": "suspended"})
}

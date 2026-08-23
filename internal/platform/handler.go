// Package platform owns super-admin operations: organizations, org users, and
// global tournament oversight (suspend/archive/restore).
package platform

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/server"
	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterAdmin mounts super-admin-only routes. The caller wraps this group
// with Auth + RequireRole(super_admin).
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	rg.GET("/admin/organizations", h.listOrgs)
	rg.POST("/admin/organizations", h.createOrg)
	rg.GET("/admin/tournaments", h.listTournaments)
	rg.POST("/admin/tournaments/:id/status", h.setTournamentStatus)
}

func (h *Handler) listOrgs(c *gin.Context) {
	page, perPage, offset := server.Pagination(c)
	items, total, err := h.svc.ListOrgs(c.Request.Context(), perPage, offset)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.List(c, items, server.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) createOrg(c *gin.Context) {
	var req CreateOrgRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid organization payload"))
		return
	}
	org, err := h.svc.CreateOrg(c.Request.Context(), middleware.UserID(c), req)
	switch {
	case errors.Is(err, ErrSlugUsed):
		server.Error(c, server.ErrConflict("that slug is already in use"))
		return
	case errors.Is(err, ErrEmailUsed):
		server.Error(c, server.ErrConflict("that email is already in use"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.Created(c, org)
}

func (h *Handler) listTournaments(c *gin.Context) {
	page, perPage, offset := server.Pagination(c)
	items, total, err := h.svc.ListAllTournaments(c.Request.Context(), perPage, offset)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.List(c, items, server.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) setTournamentStatus(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	var req SuspendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid action"))
		return
	}
	t, err := h.svc.SetTournamentStatus(c.Request.Context(), middleware.UserID(c), id, req.Action)
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, t)
}

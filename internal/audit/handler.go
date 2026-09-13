package audit

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/server"
)

func parseUUID(s string) *uuid.UUID {
	if s == "" {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterAdmin mounts the super-admin audit query. The caller wraps this group
// with Auth + RequireRole(super_admin).
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	rg.GET("/admin/audit-logs", h.list)
}

// list accepts the filters the platform audit page offers. Unknown or
// malformed filter values are ignored rather than rejected: an audit view that
// errors on a bad query string is less useful than one that shows everything.
func (h *Handler) list(c *gin.Context) {
	page, perPage, offset := server.Pagination(c)
	f := ListFilter{
		Action:           c.Query("action"),
		ActorUserID:      parseUUID(c.Query("actor")),
		EffectiveUserID:  parseUUID(c.Query("effective_user")),
		TournamentID:     parseUUID(c.Query("tournament")),
		OrgID:            parseUUID(c.Query("organization")),
		ImpersonatedOnly: c.Query("impersonated") == "true",
		From:             parseTime(c.Query("from")),
		To:               parseTime(c.Query("to")),
	}
	items, total, err := h.svc.List(c.Request.Context(), f, perPage, offset)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.List(c, items, server.Meta{Page: page, PerPage: perPage, Total: total})
}

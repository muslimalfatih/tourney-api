package audit

import (
	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/server"
)

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

func (h *Handler) list(c *gin.Context) {
	page, perPage, offset := server.Pagination(c)
	items, total, err := h.svc.List(c.Request.Context(), perPage, offset)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.List(c, items, server.Meta{Page: page, PerPage: perPage, Total: total})
}

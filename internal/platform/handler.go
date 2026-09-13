// Package platform owns super-admin operations: organizations, users,
// invitations, impersonation, global tournament oversight and the read-only
// settings view. Every route here sits behind Auth + RequireSuperAdmin.
package platform

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/auth"
	"github.com/muslimalfatih/tourney-api/internal/email"
	"github.com/muslimalfatih/tourney-api/internal/server"
	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

type Handler struct {
	svc      *Service
	auth     *auth.Service
	otp      *auth.OTPService
	sender   email.Sender
	settings SettingsSource
}

// HandlerDeps is wired in main.go.
type HandlerDeps struct {
	Service  *Service
	Auth     *auth.Service
	OTP      *auth.OTPService
	Sender   email.Sender
	Settings SettingsSource
}

func NewHandler(d HandlerDeps) *Handler {
	return &Handler{svc: d.Service, auth: d.Auth, otp: d.OTP, sender: d.Sender, settings: d.Settings}
}

// RegisterAdmin mounts super-admin-only routes. The caller wraps this group
// with Auth + RequireSuperAdmin.
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	rg.GET("/admin/overview", h.overview)
	rg.GET("/admin/settings", h.getSettings)
	rg.POST("/admin/settings/plunk-test", h.plunkTest)

	rg.GET("/admin/organizations", h.listOrgs)
	rg.POST("/admin/organizations", h.createOrg)

	rg.GET("/admin/tournaments", h.listTournaments)
	rg.POST("/admin/tournaments/:id/status", h.setTournamentStatus)

	rg.GET("/admin/invitations", h.listInvitations)
	rg.POST("/admin/invitations", h.createInvitation)
	rg.PATCH("/admin/invitations/:id", h.revokeInvitation)
	rg.POST("/admin/invitations/:id/resend", h.resendInvitation)

	rg.GET("/admin/users", h.listUsers)
	rg.GET("/admin/users/:id", h.getUser)
	rg.PATCH("/admin/users/:id", h.patchUser)
	rg.POST("/admin/users/:id/impersonate", h.impersonate)
}

// by builds the attribution for the current request in one place.
func by(c *gin.Context) Attribution {
	actor, eff, sess := middleware.Attribution(c)
	return Attribution{Actor: actor, Effective: eff, Session: sess}
}

func pathID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid id"))
		return uuid.Nil, false
	}
	return id, true
}

func optStr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// ---- overview / settings ---------------------------------------------------

func (h *Handler) overview(c *gin.Context) {
	o, err := h.svc.Overview(c.Request.Context())
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, o)
}

func (h *Handler) getSettings(c *gin.Context) {
	server.OK(c, h.svc.Settings(h.settings))
}

// plunkTest sends a sign-in code to the caller's own address. No recipient is
// accepted from the request.
func (h *Handler) plunkTest(c *gin.Context) {
	err := h.svc.SendTestOTP(c.Request.Context(), by(c), h.otp, c.ClientIP(), c.GetHeader("User-Agent"))
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		server.Error(c, &server.AppError{Status: http.StatusTooManyRequests,
			Code: "otp_rate_limited", Message: "Too many test sends. Please wait a few minutes."})
		return
	case errors.Is(err, auth.ErrDeliveryFailed):
		server.Error(c, &server.AppError{Status: http.StatusBadGateway,
			Code: "delivery_failed", Message: "The email provider refused the message."})
		return
	case errors.Is(err, auth.ErrNotInvited), errors.Is(err, auth.ErrInvitationExpired):
		server.Error(c, &server.AppError{Status: http.StatusConflict,
			Code: "not_invited", Message: "Your own account has no active invitation; the test cannot run."})
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"sent": true})
}

// ---- organizations (existing) ---------------------------------------------

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

// ---- tournaments (existing, extended) --------------------------------------

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
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req SuspendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid action"))
		return
	}
	t, err := h.svc.SetTournamentStatus(c.Request.Context(), by(c), id, req.Action, optStr(req.Reason))
	switch {
	case errors.Is(err, ErrReasonRequired):
		server.Error(c, server.ErrValidation("a reason is required for this action"))
		return
	case errors.Is(err, ErrNotFound):
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, t)
}

// ---- invitations -----------------------------------------------------------

type createInvitationRequest struct {
	Email          string  `json:"email" binding:"required"`
	Role           string  `json:"role" binding:"required"`
	OrganizationID *string `json:"organization_id"`
	Note           *string `json:"note"`
}

func (h *Handler) listInvitations(c *gin.Context) {
	page, perPage, offset := server.Pagination(c)
	items, total, err := h.svc.ListInvitations(c.Request.Context(), InvitationFilter{
		Search: c.Query("search"), Status: c.Query("status"), Role: c.Query("role"),
	}, perPage, offset)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.List(c, items, server.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) createInvitation(c *gin.Context) {
	var req createInvitationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid invitation payload"))
		return
	}
	var org *uuid.UUID
	if req.OrganizationID != nil && *req.OrganizationID != "" {
		id, err := uuid.Parse(*req.OrganizationID)
		if err != nil {
			server.Error(c, server.ErrValidation("invalid organization_id"))
			return
		}
		org = &id
	}
	inv, err := h.svc.CreateInvitation(c.Request.Context(), by(c), auth.CreateInvitationParams{
		Email: req.Email, Role: req.Role, Organization: org, Note: req.Note,
	}, h.sender)
	switch {
	case errors.Is(err, ErrBadRole), errors.Is(err, ErrOrgRequired), errors.Is(err, ErrOrgForbidden):
		server.Error(c, server.ErrValidation(err.Error()))
		return
	case errors.Is(err, auth.ErrInvitationExists):
		server.Error(c, server.ErrConflict("an active invitation already exists for this email"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.Created(c, inv)
}

type revokeInvitationRequest struct {
	Status string  `json:"status" binding:"required,oneof=revoked"`
	Reason *string `json:"reason"`
}

func (h *Handler) revokeInvitation(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req revokeInvitationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("only {\"status\":\"revoked\"} is supported"))
		return
	}
	inv, err := h.svc.RevokeInvitation(c.Request.Context(), by(c), id, req.Reason)
	switch {
	case errors.Is(err, auth.ErrNotInvited):
		server.Error(c, server.ErrNotFound("invitation not found"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, inv)
}

func (h *Handler) resendInvitation(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	inv, err := h.svc.ResendInvitation(c.Request.Context(), by(c), id, h.sender)
	switch {
	case errors.Is(err, auth.ErrNotInvited):
		server.Error(c, server.ErrNotFound("invitation not found"))
		return
	case errors.Is(err, auth.ErrInvitationExpired):
		server.Error(c, server.ErrConflict("this invitation is revoked or expired; create a new one"))
		return
	case errors.Is(err, auth.ErrDeliveryFailed):
		server.Error(c, &server.AppError{Status: http.StatusBadGateway,
			Code: "delivery_failed", Message: "The email provider refused the message."})
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, inv)
}

// ---- users -----------------------------------------------------------------

func (h *Handler) listUsers(c *gin.Context) {
	page, perPage, offset := server.Pagination(c)
	items, total, err := h.svc.ListUsers(c.Request.Context(), UserFilter{
		Search: c.Query("search"), Role: c.Query("role"), Status: c.Query("status"),
	}, perPage, offset)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.List(c, items, server.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) getUser(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	u, err := h.svc.GetUser(c.Request.Context(), id)
	if errors.Is(err, ErrUserNotFound) {
		server.Error(c, server.ErrNotFound("user not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, u)
}

// patchUserRequest changes role OR status. Exactly one per call keeps each
// change its own audit row with its own reason.
type patchUserRequest struct {
	Role           *string `json:"role"`
	OrganizationID *string `json:"organization_id"`
	Status         *string `json:"status"`
	Reason         *string `json:"reason"`
}

func (h *Handler) patchUser(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req patchUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid user payload"))
		return
	}
	if (req.Role == nil) == (req.Status == nil) {
		server.Error(c, server.ErrValidation("send exactly one of role or status"))
		return
	}

	var (
		u   *AdminUser
		err error
	)
	if req.Role != nil {
		var org *uuid.UUID
		if req.OrganizationID != nil && *req.OrganizationID != "" {
			oid, perr := uuid.Parse(*req.OrganizationID)
			if perr != nil {
				server.Error(c, server.ErrValidation("invalid organization_id"))
				return
			}
			org = &oid
		}
		u, err = h.svc.ChangeRole(c.Request.Context(), by(c), id, *req.Role, org, req.Reason)
	} else {
		u, err = h.svc.SetUserStatus(c.Request.Context(), by(c), id, *req.Status, req.Reason)
	}

	switch {
	case errors.Is(err, ErrLastSuperAdmin):
		server.Error(c, &server.AppError{Status: http.StatusConflict,
			Code: "last_super_admin", Message: "This is the last active super admin and cannot be demoted or suspended."})
		return
	case errors.Is(err, ErrUserNotFound):
		server.Error(c, server.ErrNotFound("user not found"))
		return
	case errors.Is(err, ErrBadRole), errors.Is(err, ErrOrgRequired), errors.Is(err, ErrOrgForbidden):
		server.Error(c, server.ErrValidation(err.Error()))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, u)
}

// ---- impersonation ---------------------------------------------------------

type impersonateRequest struct {
	Reason *string `json:"reason"`
}

// impersonate opens an organizer session for the caller. Every rule is checked
// in auth.StartImpersonation from the caller's own session row; this handler
// only shapes the response.
func (h *Handler) impersonate(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req impersonateRequest
	_ = c.ShouldBindJSON(&req)

	pair, target, err := h.auth.StartImpersonation(c.Request.Context(), h.svc.audit, middleware.SessionID(c), id, req.Reason)
	switch {
	case errors.Is(err, auth.ErrImpersonationDenied):
		server.Error(c, &server.AppError{Status: http.StatusForbidden,
			Code: "impersonation_denied", Message: "This user cannot be impersonated."})
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{
		"access_token":  pair.AccessToken,
		"refresh_token": pair.RefreshToken,
		"user": gin.H{
			"id": target.ID.String(), "email": target.Email, "name": target.Name, "role": target.Role,
		},
	})
}

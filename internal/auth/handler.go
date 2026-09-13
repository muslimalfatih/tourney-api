package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/tourney-api/internal/audit"
	"github.com/muslimalfatih/tourney-api/internal/server"
	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

// Handler exposes the auth endpoints. It returns tokens in the JSON body; the
// web app is responsible for storing them in its httpOnly cookie and forwarding
// the access token as a Bearer header on server-side API calls.
type Handler struct {
	svc      *Service
	otp      *OTPService
	audit    *audit.Service
	verifier middleware.TokenVerifier
}

func NewHandler(svc *Service, otp *OTPService, auditSvc *audit.Service, verifier middleware.TokenVerifier) *Handler {
	return &Handler{svc: svc, otp: otp, audit: auditSvc, verifier: verifier}
}

// Register mounts the auth routes. verifier is passed so /me can sit behind the
// Auth middleware using the same token service.
func (h *Handler) Register(rg *gin.RouterGroup, verifier middleware.TokenVerifier) {
	rg.POST("/auth/otp/request", h.otpRequest)
	rg.POST("/auth/otp/verify", h.otpVerify)
	// Retained behind AUTH_PASSWORD_LOGIN_ENABLED (default false) as the
	// rollback path while OTP beds in. No separate /auth/otp/resend: repeating
	// the request is the resend, and it shares the same rate limit.
	rg.POST("/auth/login", h.login)
	rg.POST("/auth/refresh", h.refresh)
	rg.POST("/auth/logout", h.logout)
	rg.GET("/me", middleware.Auth(verifier), h.me)
	// Exit is authorized by the SESSION (it must be an impersonation), not by
	// role: the caller's effective role is organizer, so the super-admin group
	// would rightly refuse them. It therefore lives here, behind Auth alone.
	rg.POST("/admin/impersonation/exit", middleware.Auth(verifier), h.exitImpersonation)
}

type loginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type tokenResponse struct {
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	User         userResponse `json:"user"`
}

type userResponse struct {
	ID            string             `json:"id"`
	Email         string             `json:"email"`
	Name          string             `json:"name"`
	Role          string             `json:"role"`
	OrgID         *string            `json:"org_id"`
	Status        string             `json:"status,omitempty"`
	Impersonation *ImpersonationInfo `json:"impersonation,omitempty"`
}

func toUserResponse(u *User) userResponse {
	var org *string
	if u.OrgID != nil {
		v := u.OrgID.String()
		org = &v
	}
	return userResponse{ID: u.ID.String(), Email: u.Email, Name: u.Name, Role: u.Role, OrgID: org, Status: u.Status}
}

func (h *Handler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid login payload"))
		return
	}

	pair, user, err := h.svc.Login(c.Request.Context(), req.Email, req.Password)
	switch {
	case errors.Is(err, ErrPasswordLoginDisabled):
		server.Error(c, &server.AppError{
			Status:  http.StatusForbidden,
			Code:    "password_login_disabled",
			Message: "Password sign-in is disabled. Use the sign-in code sent to your email.",
		})
		return
	case errors.Is(err, ErrAccountSuspended):
		server.Error(c, &server.AppError{
			Status:  http.StatusForbidden,
			Code:    "account_suspended",
			Message: "This account is currently suspended.",
		})
		return
	case errors.Is(err, ErrInvalidCredentials):
		server.Error(c, server.ErrUnauthorized("invalid email or password"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}

	server.OK(c, tokenResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		User:         toUserResponse(user),
	})
}

type otpRequestBody struct {
	Email string `json:"email" binding:"required"`
}

type otpVerifyBody struct {
	Email string `json:"email" binding:"required"`
	Code  string `json:"code"  binding:"required"`
}

// otpErr maps a flow error to its response. Every branch is a fixed, stable
// code the web app switches on; the messages are ours to reword freely.
func otpErr(err error) *server.AppError {
	switch {
	case errors.Is(err, ErrInvalidEmail):
		return &server.AppError{Status: http.StatusUnprocessableEntity,
			Code: "invalid_email", Message: "Enter a valid email address."}
	case errors.Is(err, ErrNotInvited):
		return &server.AppError{Status: http.StatusForbidden,
			Code: "not_invited", Message: "This email has not been invited to Tourney.social."}
	case errors.Is(err, ErrInvitationExpired):
		return &server.AppError{Status: http.StatusForbidden,
			Code: "invitation_expired", Message: "This invitation has expired. Contact the platform administrator."}
	case errors.Is(err, ErrAccountSuspended):
		return &server.AppError{Status: http.StatusForbidden,
			Code: "account_suspended", Message: "This account is currently suspended."}
	case errors.Is(err, ErrRateLimited), errors.Is(err, ErrOTPLocked):
		return &server.AppError{Status: http.StatusTooManyRequests,
			Code: "otp_rate_limited", Message: "Too many attempts. Please try again later."}
	case errors.Is(err, ErrOTPExpired), errors.Is(err, ErrOTPNotFound):
		return &server.AppError{Status: http.StatusUnauthorized,
			Code: "code_expired", Message: "This verification code has expired. Request a new code."}
	case errors.Is(err, ErrOTPInvalid):
		return &server.AppError{Status: http.StatusUnauthorized,
			Code: "invalid_code", Message: "The verification code is invalid."}
	case errors.Is(err, ErrDeliveryFailed):
		return &server.AppError{Status: http.StatusBadGateway,
			Code: "delivery_failed", Message: "We could not send the code right now. Please try again."}
	default:
		return server.ErrInternal("")
	}
}

// otpRequest sends a sign-in code to an invited address.
//
// The success response is deliberately empty and identical in every accepted
// case, so it reveals nothing beyond what the explicit rejections already say.
func (h *Handler) otpRequest(c *gin.Context) {
	var req otpRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, otpErr(ErrInvalidEmail))
		return
	}
	if err := h.otp.Request(c.Request.Context(), RequestParams{
		Email:     req.Email,
		IP:        c.ClientIP(),
		UserAgent: c.GetHeader("User-Agent"),
	}); err != nil {
		server.Error(c, otpErr(err))
		return
	}
	server.OK(c, gin.H{"sent": true})
}

// otpVerify exchanges a correct code for a session.
func (h *Handler) otpVerify(c *gin.Context) {
	var req otpVerifyBody
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, otpErr(ErrOTPInvalid))
		return
	}
	pair, user, err := h.otp.VerifyAndSignIn(
		c.Request.Context(), req.Email, req.Code, c.ClientIP(), c.GetHeader("User-Agent"))
	if err != nil {
		server.Error(c, otpErr(err))
		return
	}
	server.OK(c, tokenResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		User:         toUserResponse(user),
	})
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

func (h *Handler) refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid refresh payload"))
		return
	}
	pair, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		server.Error(c, server.ErrUnauthorized("invalid or expired refresh token"))
		return
	}
	server.OK(c, gin.H{
		"access_token":  pair.AccessToken,
		"refresh_token": pair.RefreshToken,
	})
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// logout revokes the caller's session server-side, so the tokens it issued stop
// working at once rather than lingering for the rest of their TTL.
//
// It accepts either credential the caller might still hold: a refresh token in
// the body, or a Bearer access token. The access token lasts 15 minutes while
// the refresh cookie lasts 30 days, so requiring the former would leave anyone
// idle for a quarter of an hour unable to sign out.
//
// It always returns 204. A sign-out that reports failure is a sign-out the user
// will retry or, worse, assume worked; the button must never appear to fail.
func (h *Handler) logout(c *gin.Context) {
	var req logoutRequest
	_ = c.ShouldBindJSON(&req)

	if req.RefreshToken != "" {
		if err := h.svc.LogoutByRefreshToken(c.Request.Context(), req.RefreshToken); err == nil {
			server.NoContent(c)
			return
		}
	}
	// Fall back to the access token, if the caller sent one and it still
	// resolves to a live session.
	if claims, err := h.verifier.VerifyAccessToken(c.Request.Context(), bearerToken(c)); err == nil {
		_ = h.svc.Logout(c.Request.Context(), claims.SessionID)
	}
	server.NoContent(c)
}

// bearerToken pulls the raw token out of an Authorization header, or "" if
// there is not a well-formed one.
func bearerToken(c *gin.Context) string {
	parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// me returns the effective identity plus, during impersonation, a safe block
// naming the real actor. The banner renders from this and nothing else.
func (h *Handler) me(c *gin.Context) {
	user, err := h.svc.Me(c.Request.Context(), middleware.UserID(c))
	if err != nil {
		server.Error(c, server.ErrNotFound("user not found"))
		return
	}
	res := toUserResponse(user)
	if middleware.IsImpersonating(c) {
		if info, err := h.svc.ImpersonationFor(c.Request.Context(), middleware.SessionID(c)); err == nil {
			res.Impersonation = info
		}
	}
	server.OK(c, res)
}

// exitImpersonation ends the borrowed identity and hands back the super admin's
// own session. No user id is accepted from the client; the session row decides.
func (h *Handler) exitImpersonation(c *gin.Context) {
	pair, admin, err := h.svc.ExitImpersonation(c.Request.Context(), h.audit, middleware.SessionID(c))
	switch {
	case errors.Is(err, ErrNotImpersonating):
		server.Error(c, server.ErrBadRequest("this session is not an impersonation"))
		return
	case errors.Is(err, ErrSessionNotFound):
		// The parent is gone: nothing to return to. The client should treat
		// this exactly like a sign-out.
		server.Error(c, server.ErrUnauthorized("the original session is no longer valid; sign in again"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, tokenResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		User:         toUserResponse(admin),
	})
}

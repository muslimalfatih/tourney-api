package auth

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/tourney-api/internal/server"
	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

// Handler exposes the auth endpoints. It returns tokens in the JSON body; the
// web app is responsible for storing them in its httpOnly cookie and forwarding
// the access token as a Bearer header on server-side API calls.
type Handler struct {
	svc      *Service
	verifier middleware.TokenVerifier
}

func NewHandler(svc *Service, verifier middleware.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// Register mounts the auth routes. verifier is passed so /me can sit behind the
// Auth middleware using the same token service.
func (h *Handler) Register(rg *gin.RouterGroup, verifier middleware.TokenVerifier) {
	rg.POST("/auth/login", h.login)
	rg.POST("/auth/refresh", h.refresh)
	rg.POST("/auth/logout", h.logout)
	rg.GET("/me", middleware.Auth(verifier), h.me)
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
	ID    string  `json:"id"`
	Email string  `json:"email"`
	Name  string  `json:"name"`
	Role  string  `json:"role"`
	OrgID *string `json:"org_id"`
}

func toUserResponse(u *User) userResponse {
	var org *string
	if u.OrgID != nil {
		v := u.OrgID.String()
		org = &v
	}
	return userResponse{ID: u.ID.String(), Email: u.Email, Name: u.Name, Role: u.Role, OrgID: org}
}

func (h *Handler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid login payload"))
		return
	}

	pair, user, err := h.svc.Login(c.Request.Context(), req.Email, req.Password)
	if errors.Is(err, ErrInvalidCredentials) {
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

func (h *Handler) me(c *gin.Context) {
	user, err := h.svc.Me(c.Request.Context(), middleware.UserID(c))
	if err != nil {
		server.Error(c, server.ErrNotFound("user not found"))
		return
	}
	server.OK(c, toUserResponse(user))
}

package auth

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/muslimalfatih/laga-api/internal/server"
	"github.com/muslimalfatih/laga-api/internal/server/middleware"
)

// Handler exposes the auth endpoints. It returns tokens in the JSON body; the
// web app is responsible for storing them in its httpOnly cookie and forwarding
// the access token as a Bearer header on server-side API calls.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
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

// logout is a no-op server-side because tokens are stateless. The web app clears
// its cookie. Kept as an endpoint so the contract has a logical logout call and
// so a future token-denylist can hook in here without a frontend change.
func (h *Handler) logout(c *gin.Context) {
	server.NoContent(c)
}

func (h *Handler) me(c *gin.Context) {
	user, err := h.svc.Me(c.Request.Context(), middleware.UserID(c))
	if err != nil {
		server.Error(c, server.ErrNotFound("user not found"))
		return
	}
	server.OK(c, toUserResponse(user))
}

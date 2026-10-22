package auth

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register mounts the public auth routes and the authenticated logout routes.
func (h *Handler) Register(public, authed *gin.RouterGroup) {
	g := public.Group("/auth")
	g.POST("/send-otp", h.sendOTP)
	g.POST("/verify-otp", h.verifyOTP)
	g.POST("/refresh", h.refresh)

	a := authed.Group("/auth")
	a.POST("/logout", h.logout)
	a.POST("/logout-all", h.logoutAll)
}

type sendOTPRequest struct {
	PhoneNumber string `json:"phone_number"`
}

type verifyOTPRequest struct {
	PhoneNumber string `json:"phone_number"`
	OTP         string `json:"otp"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) sendOTP(c *gin.Context) {
	var req sendOTPRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, err := h.svc.SendOTP(c.Request.Context(), req.PhoneNumber, Meta(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "OTP sent successfully", res)
}

func (h *Handler) verifyOTP(c *gin.Context) {
	var req verifyOTPRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, err := h.svc.VerifyOTP(c.Request.Context(), req.PhoneNumber, strings.TrimSpace(req.OTP), Meta(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "OTP verified successfully", res)
}

func (h *Handler) refresh(c *gin.Context) {
	var req refreshRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	pair, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Token refreshed", pair)
}

func (h *Handler) logout(c *gin.Context) {
	if err := h.svc.Logout(c.Request.Context(), httpx.MustPrincipal(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Logged out", nil)
}

func (h *Handler) logoutAll(c *gin.Context) {
	n, err := h.svc.LogoutAll(c.Request.Context(), httpx.MustPrincipal(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Logged out of all sessions", gin.H{"revoked_sessions": n})
}

// Middleware requires a valid Bearer access token for an active session.
func (h *Handler) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := BearerToken(c.GetHeader("Authorization"))
		if !ok {
			httpx.Fail(c, apperr.ErrUnauthorized)
			return
		}
		p, _, err := h.svc.Authenticate(c.Request.Context(), token)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.SetPrincipal(c, p)
		c.Next()
	}
}

// BearerToken extracts the token from an "Authorization: Bearer <token>" header.
func BearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// Meta extracts client metadata. ClientIP honours X-Forwarded-For only from the
// trusted proxies configured on the engine.
func Meta(c *gin.Context) ClientMeta {
	m := ClientMeta{UserAgent: c.Request.UserAgent()}
	if ip, err := netip.ParseAddr(c.ClientIP()); err == nil {
		ip = ip.Unmap()
		m.IP = &ip
	}
	return m
}

package onboarding

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
	"github.com/sahilkarwasra/moepay/internal/ratelimit"
	"github.com/sahilkarwasra/moepay/internal/username"
)

type Handler struct {
	svc     *Service
	names   *username.Checker
	limiter *ratelimit.Limiter
}

func NewHandler(svc *Service, names *username.Checker, limiter *ratelimit.Limiter) *Handler {
	return &Handler{svc: svc, names: names, limiter: limiter}
}

// Register mounts routes on a group that already requires authentication.
func (h *Handler) Register(authed *gin.RouterGroup) {
	g := authed.Group("/onboarding")
	g.GET("", h.get)
	g.PUT("/profile", h.saveProfile)
	g.PUT("/username", h.saveUsername)
	g.POST("/complete", h.complete)
	authed.GET("/usernames/availability", h.availability)
}

func (h *Handler) get(c *gin.Context) {
	state, err := h.svc.Get(c.Request.Context(), httpx.MustPrincipal(c).UserID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Onboarding state", state)
}

func (h *Handler) saveProfile(c *gin.Context) {
	var in ProfileInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	state, err := h.svc.SaveProfile(c.Request.Context(), httpx.MustPrincipal(c).UserID, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Profile saved", state)
}

func (h *Handler) saveUsername(c *gin.Context) {
	var in UsernameInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	state, err := h.svc.SaveUsername(c.Request.Context(), httpx.MustPrincipal(c).UserID, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Username saved", state)
}

func (h *Handler) complete(c *gin.Context) {
	state, err := h.svc.Complete(c.Request.Context(), httpx.MustPrincipal(c).UserID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Onboarding completed", state)
}

var availabilityRule = ratelimit.Rule{Name: "username_check", Limit: 60, Window: time.Minute}

func (h *Handler) availability(c *gin.Context) {
	p := httpx.MustPrincipal(c)
	if res := h.limiter.Allow(c.Request.Context(), availabilityRule, p.UserID.String()); !res.Allowed {
		httpx.Fail(c, apperr.ErrRateLimited.WithDetails(map[string]any{"retry_after_seconds": int(res.RetryAfter.Seconds()) + 1}))
		return
	}
	raw := c.Query("username")
	if raw == "" {
		httpx.Fail(c, apperr.Validation("username query parameter is required", map[string]string{"username": "required"}))
		return
	}
	res, err := h.names.Check(c.Request.Context(), raw)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Username availability", res)
}

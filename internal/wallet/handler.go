package wallet

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/auth"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
	"github.com/sahilkarwasra/moepay/internal/ratelimit"
)

type Handler struct {
	svc     *Service
	limiter *ratelimit.Limiter
}

func NewHandler(svc *Service, limiter *ratelimit.Limiter) *Handler {
	return &Handler{svc: svc, limiter: limiter}
}

var pinRule = ratelimit.Rule{Name: "wallet_pin", Limit: 10, Window: time.Minute}

func (h *Handler) Register(authed *gin.RouterGroup) {
	g := authed.Group("/wallet")
	g.POST("/pin", h.limited(h.setPIN))
	g.PUT("/pin", h.limited(h.changePIN))
	g.POST("/pin/reset/otp", h.limited(h.sendResetOTP))
	g.POST("/pin/reset", h.limited(h.resetPIN))
	g.GET("/qr", h.qr)
}

func (h *Handler) limited(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if res := h.limiter.Allow(c.Request.Context(), pinRule, httpx.MustPrincipal(c).UserID.String()); !res.Allowed {
			httpx.Fail(c, apperr.ErrRateLimited.WithDetails(map[string]any{"retry_after_seconds": int(res.RetryAfter.Seconds()) + 1}))
			return
		}
		next(c)
	}
}

func (h *Handler) setPIN(c *gin.Context) {
	var in SetPINInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	st, err := h.svc.SetPIN(c.Request.Context(), httpx.MustPrincipal(c).UserID, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusCreated, "Wallet PIN set", gin.H{"pin": st})
}

func (h *Handler) changePIN(c *gin.Context) {
	var in ChangePINInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	st, err := h.svc.ChangePIN(c.Request.Context(), httpx.MustPrincipal(c).UserID, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Wallet PIN changed", gin.H{"pin": st})
}

func (h *Handler) sendResetOTP(c *gin.Context) {
	res, err := h.svc.SendPINResetOTP(c.Request.Context(), httpx.MustPrincipal(c).UserID, auth.Meta(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "OTP sent to your registered phone", res)
}

func (h *Handler) resetPIN(c *gin.Context) {
	var in ResetPINInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	st, err := h.svc.ResetPIN(c.Request.Context(), httpx.MustPrincipal(c).UserID, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Wallet PIN reset", gin.H{"pin": st})
}

func (h *Handler) qr(c *gin.Context) {
	var amount int64
	if raw := c.Query("amount_pence"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			httpx.Fail(c, apperr.ErrAmountInvalid)
			return
		}
		amount = v
	}
	qr, err := h.svc.QR(c.Request.Context(), httpx.MustPrincipal(c).UserID, amount, c.Query("note"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Payment QR", qr)
}

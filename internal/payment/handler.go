package payment

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/idempotency"
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

var (
	resolveRule = ratelimit.Rule{Name: "recipient_resolve", Limit: 30, Window: time.Minute}
	paymentRule = ratelimit.Rule{Name: "payments", Limit: 30, Window: time.Minute}
)

func (h *Handler) Register(authed *gin.RouterGroup) {
	authed.GET("/recipients/resolve", h.limit(resolveRule, h.resolve))
	authed.GET("/recipients/recent", h.recent)
	authed.POST("/transfers", h.limit(paymentRule, h.transfer))
	authed.POST("/wallet/add-money", h.limit(paymentRule, h.addMoney))
	authed.POST("/wallet/withdraw", h.limit(paymentRule, h.withdraw))
}

func (h *Handler) limit(rule ratelimit.Rule, next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if res := h.limiter.Allow(c.Request.Context(), rule, httpx.MustPrincipal(c).UserID.String()); !res.Allowed {
			httpx.Fail(c, apperr.ErrRateLimited.WithDetails(map[string]any{"retry_after_seconds": int(res.RetryAfter.Seconds()) + 1}))
			return
		}
		next(c)
	}
}

func (h *Handler) resolve(c *gin.Context) {
	ctx, me := c.Request.Context(), httpx.MustPrincipal(c).UserID
	if qr := c.Query("qr"); qr != "" {
		res, err := h.svc.ResolveQR(ctx, me, qr)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, http.StatusOK, "Recipient", res)
		return
	}
	r, err := h.svc.Resolve(ctx, h.svc.pool, me, RecipientRef{
		UserID: c.Query("user_id"), Username: c.Query("username"), Phone: c.Query("phone"),
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Recipient", gin.H{"recipient": r})
}

func (h *Handler) recent(c *gin.Context) {
	ctx := c.Request.Context()
	w, err := h.svc.wallets.ForUser(ctx, h.svc.pool, httpx.MustPrincipal(c).UserID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	list, err := h.svc.Recent(ctx, h.svc.pool, w.ID, 10)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Recent recipients", gin.H{"recipients": list})
}

func (h *Handler) transfer(c *gin.Context) {
	var in TransferInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, replayed, err := h.svc.Transfer(c.Request.Context(), httpx.MustPrincipal(c).UserID, c.GetHeader(idempotency.Header), in)
	Respond(c, "Money sent", res, replayed, err)
}

func (h *Handler) addMoney(c *gin.Context) {
	var in BankTransferInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, replayed, err := h.svc.AddMoney(c.Request.Context(), httpx.MustPrincipal(c).UserID, c.GetHeader(idempotency.Header), in)
	Respond(c, "Money added", res, replayed, err)
}

func (h *Handler) withdraw(c *gin.Context) {
	var in BankTransferInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, replayed, err := h.svc.Withdraw(c.Request.Context(), httpx.MustPrincipal(c).UserID, c.GetHeader(idempotency.Header), in)
	Respond(c, "Money withdrawn", res, replayed, err)
}

// Respond renders a money-moving result; replays are flagged so clients and logs can
// tell them apart from the original request.
func Respond(c *gin.Context, message string, res any, replayed bool, err error) {
	if replayed {
		c.Header("Idempotent-Replayed", "true")
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusCreated, message, res)
}

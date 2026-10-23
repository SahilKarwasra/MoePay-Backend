package moneyrequest

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/idempotency"
	"github.com/sahilkarwasra/moepay/internal/payment"
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

var createRule = ratelimit.Rule{Name: "money_request_create", Limit: 20, Window: time.Hour}

func (h *Handler) Register(authed *gin.RouterGroup) {
	g := authed.Group("/money-requests")
	g.POST("", h.create)
	g.GET("", h.list)
	g.GET("/:id", h.get)
	g.POST("/:id/pay", h.pay)
	g.POST("/:id/decline", h.decline)
	g.POST("/:id/cancel", h.cancel)
}

func (h *Handler) create(c *gin.Context) {
	me := httpx.MustPrincipal(c).UserID
	if res := h.limiter.Allow(c.Request.Context(), createRule, me.String()); !res.Allowed {
		httpx.Fail(c, apperr.ErrRateLimited.WithDetails(map[string]any{"retry_after_seconds": int(res.RetryAfter.Seconds()) + 1}))
		return
	}
	var in CreateInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	v, err := h.svc.Create(c.Request.Context(), me, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusCreated, "Money request sent", v)
}

func (h *Handler) list(c *gin.Context) {
	f := ListFilter{Direction: c.Query("direction"), Status: c.Query("status"), Cursor: c.Query("cursor")}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpx.Fail(c, apperr.Validation("Invalid limit", map[string]string{"limit": "positive integer"}))
			return
		}
		f.Limit = n
	}
	page, err := h.svc.List(c.Request.Context(), httpx.MustPrincipal(c).UserID, f)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Money requests", page)
}

func (h *Handler) get(c *gin.Context) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	v, err := h.svc.Get(c.Request.Context(), httpx.MustPrincipal(c).UserID, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Money request", v)
}

func (h *Handler) pay(c *gin.Context) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	var in PayInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, replayed, err := h.svc.Pay(c.Request.Context(), httpx.MustPrincipal(c).UserID, id, c.GetHeader(idempotency.Header), in)
	payment.Respond(c, "Request paid", res, replayed, err)
}

func (h *Handler) decline(c *gin.Context) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	v, err := h.svc.Decline(c.Request.Context(), httpx.MustPrincipal(c).UserID, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Request declined", v)
}

func (h *Handler) cancel(c *gin.Context) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	v, err := h.svc.Cancel(c.Request.Context(), httpx.MustPrincipal(c).UserID, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Request cancelled", v)
}

func requestID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Fail(c, apperr.ErrMoneyRequestNotFound)
		return uuid.Nil, false
	}
	return id, true
}

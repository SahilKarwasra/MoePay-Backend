package chat

import (
	"context"
	"encoding/json"
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

var (
	sendRule   = ratelimit.Rule{Name: "chat_send", Limit: 60, Window: time.Minute}
	typingRule = ratelimit.Rule{Name: "chat_typing", Limit: 30, Window: time.Minute}
)

func (h *Handler) Register(authed *gin.RouterGroup) {
	authed.GET("/chat/sync", h.sync)
	g := authed.Group("/conversations")
	g.POST("", h.open)
	g.GET("", h.list)
	g.GET("/:id", h.get)
	g.GET("/:id/messages", h.messages)
	g.POST("/:id/messages", h.limit(sendRule, h.send))
	g.PATCH("/:id/messages/:messageId", h.limit(sendRule, h.edit))
	g.DELETE("/:id/messages/:messageId", h.delete)
	g.POST("/:id/read", h.read)
	g.POST("/:id/payments", h.limit(sendRule, h.pay))
	g.POST("/:id/requests", h.limit(sendRule, h.request))
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

func (h *Handler) sync(c *gin.Context) {
	after, err := intQuery(c, "after", 0)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	limit, err := limitQuery(c, 100, 500)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	page, err := h.svc.Sync(c.Request.Context(), httpx.MustPrincipal(c).UserID, after, limit)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Chat changes", page)
}

type openInput struct {
	With payment.RecipientRef `json:"with"`
}

func (h *Handler) open(c *gin.Context) {
	var in openInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	conv, err := h.svc.Open(c.Request.Context(), httpx.MustPrincipal(c).UserID, in.With)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Conversation", conv)
}

func (h *Handler) list(c *gin.Context) {
	limit, err := limitQuery(c, 20, 50)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	page, err := h.svc.List(c.Request.Context(), httpx.MustPrincipal(c).UserID, c.Query("cursor"), limit)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Conversations", page)
}

func (h *Handler) get(c *gin.Context) {
	id, ok := pathUUID(c, "id", apperr.ErrConversationNotFound)
	if !ok {
		return
	}
	conv, err := h.svc.Get(c.Request.Context(), httpx.MustPrincipal(c).UserID, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Conversation", conv)
}

func (h *Handler) messages(c *gin.Context) {
	id, ok := pathUUID(c, "id", apperr.ErrConversationNotFound)
	if !ok {
		return
	}
	before, err := intQuery(c, "before_seq", 0)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	limit, err := limitQuery(c, 50, 100)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	page, err := h.svc.Messages(c.Request.Context(), httpx.MustPrincipal(c).UserID, id, before, limit)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Messages", page)
}

func (h *Handler) send(c *gin.Context) {
	id, ok := pathUUID(c, "id", apperr.ErrConversationNotFound)
	if !ok {
		return
	}
	var in SendInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	m, duplicate, err := h.svc.Send(c.Request.Context(), httpx.MustPrincipal(c).UserID, id, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
		c.Header("Idempotent-Replayed", "true")
	}
	httpx.OK(c, status, "Message sent", m)
}

func (h *Handler) edit(c *gin.Context) {
	convID, msgID, ok := messagePath(c)
	if !ok {
		return
	}
	var in EditInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	m, err := h.svc.Edit(c.Request.Context(), httpx.MustPrincipal(c).UserID, convID, msgID, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Message edited", m)
}

func (h *Handler) delete(c *gin.Context) {
	convID, msgID, ok := messagePath(c)
	if !ok {
		return
	}
	m, err := h.svc.Delete(c.Request.Context(), httpx.MustPrincipal(c).UserID, convID, msgID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Message deleted", m)
}

type readInput struct {
	UpToSeq int64 `json:"up_to_seq"`
}

func (h *Handler) read(c *gin.Context) {
	id, ok := pathUUID(c, "id", apperr.ErrConversationNotFound)
	if !ok {
		return
	}
	var in readInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	conv, err := h.svc.MarkRead(c.Request.Context(), httpx.MustPrincipal(c).UserID, id, in.UpToSeq)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Marked as read", conv)
}

func (h *Handler) pay(c *gin.Context) {
	id, ok := pathUUID(c, "id", apperr.ErrConversationNotFound)
	if !ok {
		return
	}
	var in PayInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	// Offline clients may queue a payment with only its client_message_id; it then
	// doubles as the idempotency key.
	key := c.GetHeader(idempotency.Header)
	if key == "" {
		key = in.ClientMessageID
	}
	res, replayed, err := h.svc.Pay(c.Request.Context(), httpx.MustPrincipal(c).UserID, id, key, in)
	payment.Respond(c, "Money sent", res, replayed, err)
}

func (h *Handler) request(c *gin.Context) {
	id, ok := pathUUID(c, "id", apperr.ErrConversationNotFound)
	if !ok {
		return
	}
	var in RequestInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, err := h.svc.Request(c.Request.Context(), httpx.MustPrincipal(c).UserID, id, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusCreated, "Money request sent", res)
}

type typingInput struct {
	ConversationID string `json:"conversation_id"`
	Typing         bool   `json:"typing"`
}

// HandleTyping handles the "typing" WebSocket message from a client.
func (h *Handler) HandleTyping(ctx context.Context, userID uuid.UUID, data json.RawMessage) error {
	var in typingInput
	if err := json.Unmarshal(data, &in); err != nil {
		return apperr.ErrInvalidRequest
	}
	convID, err := uuid.Parse(in.ConversationID)
	if err != nil {
		return apperr.ErrConversationNotFound
	}
	if res := h.limiter.Allow(ctx, typingRule, userID.String()); !res.Allowed {
		return apperr.ErrRateLimited
	}
	return h.svc.Typing(ctx, userID, convID, in.Typing)
}

func pathUUID(c *gin.Context, name string, notFound *apperr.Error) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		httpx.Fail(c, notFound)
		return uuid.Nil, false
	}
	return id, true
}

func messagePath(c *gin.Context) (uuid.UUID, uuid.UUID, bool) {
	convID, ok := pathUUID(c, "id", apperr.ErrConversationNotFound)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	msgID, ok := pathUUID(c, "messageId", apperr.ErrMessageNotFound)
	return convID, msgID, ok
}

func intQuery(c *gin.Context, name string, def int64) (int64, error) {
	raw := c.Query(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, apperr.Validation("Invalid "+name, map[string]string{name: "non-negative integer"})
	}
	return v, nil
}

func limitQuery(c *gin.Context, def, max int) (int, error) {
	raw := c.Query("limit")
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 || v > max {
		return 0, apperr.Validation("Invalid limit", map[string]string{"limit": "1-" + strconv.Itoa(max)})
	}
	return v, nil
}

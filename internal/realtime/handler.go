package realtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
)

// Authenticator validates access tokens and session liveness (implemented by auth.Service).
type Authenticator interface {
	Authenticate(ctx context.Context, accessToken string) (httpx.Principal, time.Time, error)
	SessionActive(ctx context.Context, sessionID uuid.UUID) (bool, error)
}

type Handler struct {
	hub      *Hub
	auth     Authenticator
	bearer   func(string) (string, bool)
	rdb      *redis.Client
	cfg      config.WSConfig
	logger   *slog.Logger
	upgrader websocket.Upgrader
}

func NewHandler(hub *Hub, auth Authenticator, bearer func(string) (string, bool), rdb *redis.Client, cfg config.WSConfig, logger *slog.Logger) *Handler {
	h := &Handler{hub: hub, auth: auth, bearer: bearer, rdb: rdb, cfg: cfg, logger: logger}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}
	return h
}

func (h *Handler) Register(public, authed *gin.RouterGroup) {
	authed.POST("/realtime/ticket", h.issueTicket)
	public.GET("/ws", h.serve)
}

// checkOrigin allows native clients (no Origin header) and configured browser origins.
func (h *Handler) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || slices.Contains(h.cfg.AllowedOrigins, "*") || slices.Contains(h.cfg.AllowedOrigins, origin)
}

type ticketPayload struct {
	UserID    uuid.UUID `json:"user_id"`
	SessionID uuid.UUID `json:"session_id"`
}

func ticketKey(t string) string { return "ws:ticket:" + t }

// issueTicket returns a single-use, short-lived ticket for clients (browsers) that
// cannot send an Authorization header on the WebSocket handshake. Tickets avoid
// putting long-lived access tokens in URLs and logs.
func (h *Handler) issueTicket(c *gin.Context) {
	p := httpx.MustPrincipal(c)
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		httpx.Fail(c, err)
		return
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf[:])
	payload, _ := json.Marshal(ticketPayload{UserID: p.UserID, SessionID: p.SessionID})
	if err := h.rdb.Set(c.Request.Context(), ticketKey(ticket), payload, h.cfg.TicketTTL).Err(); err != nil {
		httpx.Fail(c, apperr.ErrUnavailable.Wrap(err))
		return
	}
	httpx.OK(c, http.StatusOK, "Realtime ticket issued", gin.H{
		"ticket":             ticket,
		"expires_in_seconds": int(h.cfg.TicketTTL.Seconds()),
	})
}

func (h *Handler) authenticate(c *gin.Context) (httpx.Principal, error) {
	ctx := c.Request.Context()
	if token, ok := h.bearer(c.GetHeader("Authorization")); ok {
		p, _, err := h.auth.Authenticate(ctx, token)
		return p, err
	}
	ticket := c.Query("ticket")
	if ticket == "" || len(ticket) > 64 {
		return httpx.Principal{}, apperr.ErrUnauthorized
	}
	raw, err := h.rdb.GetDel(ctx, ticketKey(ticket)).Bytes()
	if errors.Is(err, redis.Nil) {
		return httpx.Principal{}, apperr.ErrTokenInvalid
	}
	if err != nil {
		return httpx.Principal{}, apperr.ErrUnavailable.Wrap(err)
	}
	var tp ticketPayload
	if err := json.Unmarshal(raw, &tp); err != nil {
		return httpx.Principal{}, apperr.ErrTokenInvalid
	}
	active, err := h.auth.SessionActive(ctx, tp.SessionID)
	if err != nil {
		return httpx.Principal{}, err
	}
	if !active {
		return httpx.Principal{}, apperr.ErrSessionRevoked
	}
	return httpx.Principal{UserID: tp.UserID, SessionID: tp.SessionID}, nil
}

func (h *Handler) serve(c *gin.Context) {
	p, err := h.authenticate(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.SetPrincipal(c, p)
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade already wrote an HTTP error.
		return
	}
	logger := h.logger.With("user_id", p.UserID, "session_id", p.SessionID)
	cl := newClient(h.hub, conn, p.UserID, p.SessionID, clientOptions{
		maxMessageBytes: h.cfg.MaxMessageBytes,
		pingInterval:    h.cfg.PingInterval,
		lifetime:        h.cfg.MaxConnLifetime,
		sendBuffer:      h.cfg.SendBufferMessages,
	}, logger)

	if !h.hub.register(cl) {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(closeTooManyConns, "too many connections"), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	logger.Info("websocket connected")
	cl.sendEvent("connected", gin.H{"user_id": p.UserID, "session_id": p.SessionID})

	go cl.writePump()
	cl.readPump()
	<-cl.closed
	h.hub.unregister(cl)
	logger.Info("websocket disconnected")
}

package realtime

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Application close codes (4000-4999 are reserved for applications).
const (
	closeGoingAway      = websocket.CloseGoingAway
	closePolicy         = websocket.ClosePolicyViolation
	closeSessionRevoked = 4001
	closeReauthRequired = 4002
	closeTooManyConns   = 4003
	closeSlowConsumer   = 4004
)

type clientOptions struct {
	maxMessageBytes int64
	pingInterval    time.Duration
	lifetime        time.Duration
	sendBuffer      int
}

type client struct {
	hub       *Hub
	conn      *websocket.Conn
	userID    uuid.UUID
	sessionID uuid.UUID
	opts      clientOptions
	logger    *slog.Logger

	send      chan []byte
	closeOnce sync.Once
	closed    chan struct{}
	closeMsg  []byte
}

func newClient(hub *Hub, conn *websocket.Conn, userID, sessionID uuid.UUID, opts clientOptions, logger *slog.Logger) *client {
	return &client{
		hub: hub, conn: conn, userID: userID, sessionID: sessionID, opts: opts, logger: logger,
		send:   make(chan []byte, opts.sendBuffer),
		closed: make(chan struct{}),
	}
}

// enqueue queues a message; a client that cannot keep up is disconnected rather
// than allowed to grow memory without bound.
func (c *client) enqueue(msg []byte) {
	select {
	case <-c.closed:
	case c.send <- msg:
	default:
		c.closeWith(closeSlowConsumer, "client too slow")
	}
}

func (c *client) closeWith(code int, reason string) {
	c.closeOnce.Do(func() {
		c.closeMsg = websocket.FormatCloseMessage(code, reason)
		close(c.closed)
	})
}

func (c *client) sendEvent(typ string, data any) {
	b, err := json.Marshal(Event{Type: typ, Data: data, TS: time.Now().UTC()})
	if err == nil {
		c.enqueue(b)
	}
}

// inbound is a client→server message. Only whitelisted types are accepted.
type inbound struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

func (c *client) readPump() {
	defer c.closeWith(websocket.CloseNormalClosure, "")
	pongWait := c.opts.pingInterval * 2
	c.conn.SetReadLimit(c.opts.maxMessageBytes)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	invalid := 0
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.logger.Debug("websocket read ended", "err", err)
			}
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		var msg inbound
		if err := json.Unmarshal(data, &msg); err != nil || msg.Type == "" {
			invalid++
			if invalid >= 5 {
				c.closeWith(closePolicy, "too many invalid messages")
				return
			}
			c.sendEvent("error", map[string]string{"code": "INVALID_MESSAGE"})
			continue
		}
		c.route(msg)
	}
}

// route handles client messages. Add new client→server message types here.
func (c *client) route(msg inbound) {
	switch msg.Type {
	case "ping":
		c.sendEvent("pong", map[string]string{"id": msg.ID})
	default:
		c.sendEvent("error", map[string]string{"code": "UNKNOWN_TYPE", "type": msg.Type})
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(c.opts.pingInterval)
	lifetime := time.NewTimer(c.opts.lifetime)
	defer func() {
		ticker.Stop()
		lifetime.Stop()
		_ = c.conn.Close()
	}()
	const writeWait = 10 * time.Second
	for {
		select {
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				c.closeWith(websocket.CloseAbnormalClosure, "")
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.closeWith(websocket.CloseAbnormalClosure, "")
				return
			}
		case <-lifetime.C:
			c.closeWith(closeReauthRequired, "connection lifetime exceeded; reconnect with a fresh token")
		case <-c.closed:
			if c.closeMsg != nil {
				_ = c.conn.WriteControl(websocket.CloseMessage, c.closeMsg, time.Now().Add(time.Second))
			}
			return
		}
	}
}

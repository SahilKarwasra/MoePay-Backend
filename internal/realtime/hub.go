package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const channel = "ws:events"

// Event is the envelope sent to clients.
type Event struct {
	Type string    `json:"type"`
	Data any       `json:"data,omitempty"`
	TS   time.Time `json:"ts"`
}

// busMessage travels over Redis between instances.
type busMessage struct {
	Kind       string          `json:"kind"` // "event" or "revoke"
	UserID     uuid.UUID       `json:"user_id"`
	SessionIDs []uuid.UUID     `json:"session_ids,omitempty"`
	Event      json.RawMessage `json:"event,omitempty"`
}

// Hub tracks local connections per user.
type Hub struct {
	rdb    *redis.Client
	logger *slog.Logger
	maxPer int

	mu    sync.RWMutex
	users map[uuid.UUID]map[*client]struct{}
}

func NewHub(rdb *redis.Client, maxConnsPerUser int, logger *slog.Logger) *Hub {
	return &Hub{rdb: rdb, logger: logger, maxPer: maxConnsPerUser, users: map[uuid.UUID]map[*client]struct{}{}}
}

func (h *Hub) register(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.users[c.userID]
	if len(set) >= h.maxPer {
		return false
	}
	if set == nil {
		set = map[*client]struct{}{}
		h.users[c.userID] = set
	}
	set[c] = struct{}{}
	return true
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.users[c.userID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.users, c.userID)
		}
	}
}

func (h *Hub) clientsOf(userID uuid.UUID) []*client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*client, 0, len(h.users[userID]))
	for c := range h.users[userID] {
		out = append(out, c)
	}
	return out
}

// ConnectionCount returns the number of local connections (for tests/metrics).
func (h *Hub) ConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, set := range h.users {
		n += len(set)
	}
	return n
}

// PublishToUser implements onboarding.Publisher.
func (h *Hub) PublishToUser(ctx context.Context, userID uuid.UUID, eventType string, data any) {
	ev, err := json.Marshal(Event{Type: eventType, Data: data, TS: time.Now().UTC()})
	if err != nil {
		h.logger.ErrorContext(ctx, "marshal realtime event", "err", err)
		return
	}
	h.publish(ctx, busMessage{Kind: "event", UserID: userID, Event: ev})
}

// SessionsRevoked implements auth.SessionEvents.
func (h *Hub) SessionsRevoked(ctx context.Context, userID uuid.UUID, sessionIDs []uuid.UUID) {
	if len(sessionIDs) == 0 {
		return
	}
	h.publish(ctx, busMessage{Kind: "revoke", UserID: userID, SessionIDs: sessionIDs})
}

// publish sends through Redis; if Redis is down it delivers locally so a
// single-instance deployment keeps working.
func (h *Hub) publish(ctx context.Context, m busMessage) {
	payload, _ := json.Marshal(m)
	if err := h.rdb.Publish(ctx, channel, payload).Err(); err != nil {
		h.logger.WarnContext(ctx, "redis publish failed; delivering locally only", "err", err)
		h.dispatch(m)
	}
}

func (h *Hub) dispatch(m busMessage) {
	switch m.Kind {
	case "event":
		for _, c := range h.clientsOf(m.UserID) {
			c.enqueue(m.Event)
		}
	case "revoke":
		revoked := make(map[uuid.UUID]bool, len(m.SessionIDs))
		for _, id := range m.SessionIDs {
			revoked[id] = true
		}
		for _, c := range h.clientsOf(m.UserID) {
			if revoked[c.sessionID] {
				c.closeWith(closeSessionRevoked, "session revoked")
			}
		}
	}
}

// Run subscribes to the Redis channel and dispatches messages until ctx ends,
// resubscribing with backoff if the connection drops.
func (h *Hub) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		sub := h.rdb.Subscribe(ctx, channel)
		if _, err := sub.Receive(ctx); err != nil {
			_ = sub.Close()
			if ctx.Err() != nil {
				return
			}
			h.logger.Warn("realtime subscribe failed; retrying", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		ch := sub.Channel()
	loop:
		for {
			select {
			case <-ctx.Done():
				_ = sub.Close()
				return
			case msg, ok := <-ch:
				if !ok {
					break loop
				}
				var m busMessage
				if err := json.Unmarshal([]byte(msg.Payload), &m); err != nil {
					h.logger.Warn("invalid realtime bus message", "err", err)
					continue
				}
				h.dispatch(m)
			}
		}
		_ = sub.Close()
	}
}

// CloseAll closes every local connection (graceful shutdown).
func (h *Hub) CloseAll() {
	h.mu.RLock()
	var all []*client
	for _, set := range h.users {
		for c := range set {
			all = append(all, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range all {
		c.closeWith(closeGoingAway, "server shutting down")
	}
}

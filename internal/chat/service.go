package chat

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/moneyrequest"
	"github.com/sahilkarwasra/moepay/internal/payment"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/wallet"
)

// Publisher pushes realtime events to a user's connected devices.
type Publisher interface {
	PublishToUser(ctx context.Context, userID uuid.UUID, eventType string, data any)
}

type Service struct {
	pool     *pgxpool.Pool
	repo     *Repository
	wallets  *wallet.Service
	payments *payment.Service
	requests *moneyrequest.Service
	events   Publisher
	cfg      config.ChatConfig
	logger   *slog.Logger
	now      func() time.Time
}

type Deps struct {
	Pool     *pgxpool.Pool
	Repo     *Repository
	Wallets  *wallet.Service
	Payments *payment.Service
	Requests *moneyrequest.Service
	Events   Publisher
	Config   config.ChatConfig
	Logger   *slog.Logger
}

// NewService builds the chat service and registers hooks so that every P2P payment
// and money request between two users also appears in their conversation, in the
// same database transaction as the money movement.
func NewService(d Deps) *Service {
	s := &Service{pool: d.Pool, repo: d.Repo, wallets: d.Wallets, payments: d.Payments, requests: d.Requests,
		events: d.Events, cfg: d.Config, logger: d.Logger, now: time.Now}
	d.Payments.OnTransfer(s.onTransfer)
	d.Requests.OnCreated(s.onRequestCreated)
	d.Requests.OnResolved(s.onRequestResolved)
	return s
}

// notification is what to push to members after a write commits: each member's
// feed position before the write, so they receive exactly the new changes.
type notification map[uuid.UUID]int64

func (s *Service) notify(ctx context.Context, n notification) {
	for uid, first := range n {
		page, err := s.Sync(ctx, uid, first-1, 50)
		if err != nil {
			s.logger.WarnContext(ctx, "build chat event", "err", err)
			continue
		}
		s.events.PublishToUser(ctx, uid, "chat.changes", page)
	}
}

// write runs fn in a transaction and pushes the resulting feed changes after commit.
func (s *Service) write(ctx context.Context, fn func(tx pgx.Tx) (notification, error)) error {
	var n notification
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		n, err = fn(tx)
		return err
	})
	if err != nil {
		return err
	}
	s.notify(ctx, n)
	return nil
}

// recordMessage publishes a message (and its conversation) to both members' feeds.
func (s *Service) recordMessage(ctx context.Context, tx pgx.Tx, c lockedConversation, msgID uuid.UUID) (notification, error) {
	return s.repo.RecordChanges(ctx, tx, c.id, c.members, entityRef{"message", msgID}, entityRef{"conversation", c.id})
}

// ensureReady checks that the user has finished onboarding (and so has a wallet).
func (s *Service) ensureReady(ctx context.Context, me uuid.UUID) error {
	_, err := s.wallets.ForUser(ctx, s.pool, me)
	return err
}

// Open returns the direct conversation with another user, creating it if needed.
func (s *Service) Open(ctx context.Context, me uuid.UUID, with payment.RecipientRef) (Conversation, error) {
	if err := s.ensureReady(ctx, me); err != nil {
		return Conversation{}, err
	}
	peer, err := s.payments.Resolve(ctx, s.pool, me, with)
	if err != nil {
		return Conversation{}, err
	}
	peerID := uuid.MustParse(peer.UserID)
	var id uuid.UUID
	err = s.write(ctx, func(tx pgx.Tx) (notification, error) {
		var created bool
		var err error
		if id, created, err = s.repo.EnsureDirect(ctx, tx, me, peerID); err != nil || !created {
			return nil, err
		}
		return s.repo.RecordChanges(ctx, tx, id, []uuid.UUID{me, peerID}, entityRef{"conversation", id})
	})
	if err != nil {
		return Conversation{}, err
	}
	return s.repo.Conversation(ctx, s.pool, me, id)
}

type SendInput struct {
	ClientMessageID string `json:"client_message_id"`
	Body            string `json:"body"`
	ReplyToID       string `json:"reply_to_id"`
}

// Send appends a text message. Retrying with the same client_message_id returns the
// original message instead of creating a duplicate.
func (s *Service) Send(ctx context.Context, me, conversationID uuid.UUID, in SendInput) (Message, bool, error) {
	clientID, err := parseClientID(in.ClientMessageID)
	if err != nil {
		return Message{}, false, err
	}
	body, err := s.cleanBody(in.Body)
	if err != nil {
		return Message{}, false, err
	}
	var replyTo *uuid.UUID
	if in.ReplyToID != "" {
		id, err := uuid.Parse(in.ReplyToID)
		if err != nil {
			return Message{}, false, apperr.ErrMessageNotFound
		}
		replyTo = &id
	}
	if existing, ok, err := s.existing(ctx, me, conversationID, clientID); err != nil || ok {
		return existing, ok, err
	}

	var msgID uuid.UUID
	err = s.write(ctx, func(tx pgx.Tx) (notification, error) {
		c, err := s.repo.LockConversation(ctx, tx, me, conversationID)
		if err != nil {
			return nil, err
		}
		if replyTo != nil {
			ok, err := s.repo.MessageInConversation(ctx, tx, conversationID, *replyTo)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, apperr.ErrMessageNotFound.WithDetails(map[string]any{"field": "reply_to_id"})
			}
		}
		msgID, err = s.repo.InsertMessage(ctx, tx, &c, newMessage{
			conversationID: conversationID, senderID: me, kind: KindText, body: &body,
			clientMessageID: &clientID, replyToID: replyTo,
		})
		if err != nil {
			return nil, err
		}
		return s.recordMessage(ctx, tx, c, msgID)
	})
	if errors.Is(err, apperr.ErrClientIDReused) {
		// A concurrent retry won the race; return its result.
		if existing, ok, err := s.existing(ctx, me, conversationID, clientID); err != nil || ok {
			return existing, ok, err
		}
	}
	if err != nil {
		return Message{}, false, err
	}
	m, err := s.repo.Message(ctx, s.pool, me, msgID)
	return m, false, err
}

// existing returns the sender's message with clientID, if it was already stored.
func (s *Service) existing(ctx context.Context, me, conversationID, clientID uuid.UUID) (Message, bool, error) {
	id, conv, ok, err := s.repo.FindByClientID(ctx, s.pool, me, clientID)
	if err != nil || !ok {
		return Message{}, false, err
	}
	if conv != conversationID {
		return Message{}, false, apperr.ErrClientIDReused
	}
	m, err := s.repo.Message(ctx, s.pool, me, id)
	return m, err == nil, err
}

type EditInput struct {
	Body        string `json:"body"`
	BaseVersion *int   `json:"base_version"`
}

// Edit changes the body of the caller's own text message within the edit window.
// If base_version is given and the message has changed since, the edit is rejected
// with the current message so the client can resolve the conflict.
func (s *Service) Edit(ctx context.Context, me, conversationID, messageID uuid.UUID, in EditInput) (Message, error) {
	body, err := s.cleanBody(in.Body)
	if err != nil {
		return Message{}, err
	}
	err = s.write(ctx, func(tx pgx.Tx) (notification, error) {
		c, err := s.repo.LockConversation(ctx, tx, me, conversationID)
		if err != nil {
			return nil, err
		}
		m, err := s.repo.LockMessage(ctx, tx, conversationID, messageID)
		if err != nil {
			return nil, err
		}
		if err := s.checkMutable(m, me, s.cfg.EditWindow); err != nil {
			return nil, err
		}
		if in.BaseVersion != nil && *in.BaseVersion != m.version {
			current, err := s.repo.Message(ctx, tx, me, messageID)
			if err != nil {
				return nil, err
			}
			return nil, apperr.ErrMessageConflict.WithDetails(map[string]any{"current": current})
		}
		if m.body != nil && *m.body == body {
			return nil, nil
		}
		if err := s.repo.EditMessage(ctx, tx, m, body); err != nil {
			return nil, err
		}
		return s.recordMessage(ctx, tx, c, messageID)
	})
	if err != nil {
		return Message{}, err
	}
	return s.repo.Message(ctx, s.pool, me, messageID)
}

// Delete removes the caller's own text message for everyone. Deleting twice is a no-op.
func (s *Service) Delete(ctx context.Context, me, conversationID, messageID uuid.UUID) (Message, error) {
	err := s.write(ctx, func(tx pgx.Tx) (notification, error) {
		c, err := s.repo.LockConversation(ctx, tx, me, conversationID)
		if err != nil {
			return nil, err
		}
		m, err := s.repo.LockMessage(ctx, tx, conversationID, messageID)
		if err != nil {
			return nil, err
		}
		if m.deletedAt != nil && m.senderID == me {
			return nil, nil
		}
		if err := s.checkMutable(m, me, s.cfg.DeleteWindow); err != nil {
			return nil, err
		}
		if err := s.repo.DeleteMessage(ctx, tx, messageID); err != nil {
			return nil, err
		}
		return s.recordMessage(ctx, tx, c, messageID)
	})
	if err != nil {
		return Message{}, err
	}
	return s.repo.Message(ctx, s.pool, me, messageID)
}

func (s *Service) checkMutable(m messageRow, me uuid.UUID, window time.Duration) error {
	switch {
	case m.senderID != me:
		return apperr.ErrForbidden
	case m.kind != KindText:
		return apperr.ErrMessageNotEditable.WithDetails(map[string]any{"reason": "only text messages can be changed"})
	case m.deletedAt != nil:
		return apperr.ErrMessageNotEditable.WithDetails(map[string]any{"reason": "message was deleted"})
	case s.now().Sub(m.createdAt) > window:
		return apperr.ErrMessageNotEditable.WithDetails(map[string]any{"reason": "too late to change this message", "window_seconds": int(window.Seconds())})
	}
	return nil
}

// MarkRead records that the caller has read up to seq; the peer sees it as a read receipt.
func (s *Service) MarkRead(ctx context.Context, me, conversationID uuid.UUID, upTo int64) (Conversation, error) {
	if upTo < 0 {
		return Conversation{}, apperr.Validation("Invalid seq", map[string]string{"up_to_seq": "must be >= 0"})
	}
	err := s.write(ctx, func(tx pgx.Tx) (notification, error) {
		c, err := s.repo.LockConversation(ctx, tx, me, conversationID)
		if err != nil {
			return nil, err
		}
		changed, err := s.repo.MarkRead(ctx, tx, c, me, upTo)
		if err != nil || !changed {
			return nil, err
		}
		return s.repo.RecordChanges(ctx, tx, c.id, c.members, entityRef{"conversation", c.id})
	})
	if err != nil {
		return Conversation{}, err
	}
	return s.repo.Conversation(ctx, s.pool, me, conversationID)
}

// Sync returns the caller's changes after the given feed position, oldest first.
// Clients store next_after and call again until has_more is false.
func (s *Service) Sync(ctx context.Context, me uuid.UUID, after int64, limit int) (SyncPage, error) {
	if after < 0 {
		return SyncPage{}, apperr.Validation("Invalid cursor", map[string]string{"after": "must be >= 0"})
	}
	latest, err := s.repo.LatestSeq(ctx, s.pool, me)
	if err != nil {
		return SyncPage{}, err
	}
	changes, err := s.repo.Changes(ctx, s.pool, me, after, limit+1)
	if err != nil {
		return SyncPage{}, err
	}
	page := SyncPage{Items: []SyncItem{}, NextAfter: after, LatestSeq: latest}
	if len(changes) > limit {
		changes, page.HasMore = changes[:limit], true
	}
	var msgIDs, convIDs []uuid.UUID
	for _, c := range changes {
		if c.kind == "message" {
			msgIDs = append(msgIDs, c.entityID)
		} else {
			convIDs = append(convIDs, c.entityID)
		}
	}
	msgs, err := s.repo.MessagesByID(ctx, s.pool, me, msgIDs)
	if err != nil {
		return SyncPage{}, err
	}
	convs, err := s.repo.ConversationsByID(ctx, s.pool, me, convIDs)
	if err != nil {
		return SyncPage{}, err
	}
	for _, c := range changes {
		item := SyncItem{Seq: c.seq, Type: c.kind}
		if c.kind == "message" {
			if m, ok := msgs[c.entityID.String()]; ok {
				item.Message = &m
			}
		} else if conv, ok := convs[c.entityID.String()]; ok {
			item.Conversation = &conv
		}
		if item.Message != nil || item.Conversation != nil {
			page.Items = append(page.Items, item)
		}
		page.NextAfter = c.seq
	}
	return page, nil
}

type ConversationPage struct {
	Items      []Conversation `json:"items"`
	NextCursor *string        `json:"next_cursor"`
	HasMore    bool           `json:"has_more"`
}

func (s *Service) List(ctx context.Context, me uuid.UUID, cursor string, limit int) (ConversationPage, error) {
	var at *time.Time
	var id *uuid.UUID
	if cursor != "" {
		a, i, err := decodeCursor(cursor)
		if err != nil {
			return ConversationPage{}, err
		}
		at, id = &a, &i
	}
	items, err := s.repo.ListConversations(ctx, s.pool, me, at, id, limit+1)
	if err != nil {
		return ConversationPage{}, err
	}
	page := ConversationPage{Items: items}
	if page.Items == nil {
		page.Items = []Conversation{}
	}
	if len(items) > limit {
		page.Items, page.HasMore = items[:limit], true
		last := page.Items[limit-1]
		lastID := uuid.MustParse(last.ID)
		c := encodeCursor(last.LastMessageAt, lastID)
		page.NextCursor = &c
	}
	return page, nil
}

func (s *Service) Get(ctx context.Context, me, id uuid.UUID) (Conversation, error) {
	return s.repo.Conversation(ctx, s.pool, me, id)
}

type MessagePage struct {
	Items         []Message `json:"items"`
	NextBeforeSeq *int64    `json:"next_before_seq"`
	HasMore       bool      `json:"has_more"`
}

// Messages pages backwards through a conversation (newest first), for loading
// history on a new device without a full sync.
func (s *Service) Messages(ctx context.Context, me, conversationID uuid.UUID, beforeSeq int64, limit int) (MessagePage, error) {
	if _, err := s.repo.PeerOf(ctx, s.pool, me, conversationID); err != nil {
		return MessagePage{}, err
	}
	if beforeSeq <= 0 {
		beforeSeq = 1<<62 - 1
	}
	items, err := s.repo.History(ctx, s.pool, me, conversationID, beforeSeq, limit+1)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{Items: items}
	if page.Items == nil {
		page.Items = []Message{}
	}
	if len(items) > limit {
		page.Items, page.HasMore = items[:limit], true
		next := page.Items[limit-1].Seq
		page.NextBeforeSeq = &next
	}
	return page, nil
}

// Typing relays a typing indicator to the peer. It is ephemeral and never stored.
func (s *Service) Typing(ctx context.Context, me, conversationID uuid.UUID, typing bool) error {
	peer, err := s.repo.PeerOf(ctx, s.pool, me, conversationID)
	if err != nil {
		return err
	}
	s.events.PublishToUser(ctx, peer, "chat.typing", map[string]any{
		"conversation_id": conversationID, "user_id": me, "typing": typing,
	})
	return nil
}

func (s *Service) cleanBody(raw string) (string, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return "", apperr.ErrMessageInvalid.WithDetails(map[string]any{"body": "required"})
	}
	if n := utf8.RuneCountInString(body); n > s.cfg.MaxBodyRunes {
		return "", apperr.ErrMessageInvalid.WithDetails(map[string]any{"body": "too long", "max_length": s.cfg.MaxBodyRunes})
	}
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return "", apperr.ErrMessageInvalid.WithDetails(map[string]any{"body": "invalid characters"})
	}
	return body, nil
}

func parseClientID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || id == uuid.Nil {
		return uuid.Nil, apperr.Validation("client_message_id must be a UUID generated by the app",
			map[string]string{"client_message_id": "UUID"})
	}
	return id, nil
}

var errBadCursor = apperr.Validation("Invalid cursor", map[string]string{"cursor": "use next_cursor from the previous page"})

func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, errBadCursor
	}
	ts, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, errBadCursor
	}
	at, err1 := time.Parse(time.RFC3339Nano, ts)
	id, err2 := uuid.Parse(idStr)
	if errors.Join(err1, err2) != nil {
		return time.Time{}, uuid.Nil, errBadCursor
	}
	return at, id, nil
}

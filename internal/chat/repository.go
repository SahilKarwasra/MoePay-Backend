package chat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/user"
)

type Repository struct{ now func() time.Time }

func NewRepository() *Repository { return &Repository{now: time.Now} }

// directKey identifies the conversation between two users regardless of order.
func directKey(a, b uuid.UUID) string {
	if strings.Compare(a.String(), b.String()) > 0 {
		a, b = b, a
	}
	return a.String() + ":" + b.String()
}

// sortedMembers returns ids in a fixed order; every write locks per-user cursor rows
// in this order so concurrent writers cannot deadlock.
func sortedMembers(ids ...uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// EnsureDirect returns the conversation between a and b, creating it if needed.
func (r *Repository) EnsureDirect(ctx context.Context, tx pgx.Tx, a, b uuid.UUID) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO conversations (kind, direct_key) VALUES ('direct', $1)
		ON CONFLICT (direct_key) DO NOTHING RETURNING id`, directKey(a, b)).Scan(&id)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, id, a, b)
		if err != nil {
			return uuid.Nil, false, fmt.Errorf("add members: %w", err)
		}
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, fmt.Errorf("create conversation: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE direct_key = $1`, directKey(a, b)).Scan(&id); err != nil {
		return uuid.Nil, false, fmt.Errorf("load conversation: %w", err)
	}
	return id, false, nil
}

type lockedConversation struct {
	id      uuid.UUID
	lastSeq int64
	members []uuid.UUID // sorted
	peer    uuid.UUID
}

// LockConversation locks a conversation the viewer belongs to. Holding this lock
// serializes message sequence numbers within the conversation.
func (r *Repository) LockConversation(ctx context.Context, tx pgx.Tx, viewer, id uuid.UUID) (lockedConversation, error) {
	c := lockedConversation{id: id}
	err := tx.QueryRow(ctx, `
		SELECT c.last_seq, p.user_id FROM conversations c
		JOIN conversation_members m ON m.conversation_id = c.id AND m.user_id = $2
		JOIN conversation_members p ON p.conversation_id = c.id AND p.user_id <> $2
		WHERE c.id = $1
		FOR UPDATE OF c`, id, viewer).Scan(&c.lastSeq, &c.peer)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedConversation{}, apperr.ErrConversationNotFound
	}
	if err != nil {
		return lockedConversation{}, fmt.Errorf("lock conversation: %w", err)
	}
	c.members = sortedMembers(viewer, c.peer)
	return c, nil
}

// PeerOf returns the other member of a conversation the viewer belongs to.
func (r *Repository) PeerOf(ctx context.Context, q postgres.DBTX, viewer, id uuid.UUID) (uuid.UUID, error) {
	var peer uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT p.user_id FROM conversation_members m
		JOIN conversation_members p ON p.conversation_id = m.conversation_id AND p.user_id <> m.user_id
		WHERE m.conversation_id = $1 AND m.user_id = $2`, id, viewer).Scan(&peer)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, apperr.ErrConversationNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("load peer: %w", err)
	}
	return peer, nil
}

type newMessage struct {
	conversationID  uuid.UUID
	senderID        uuid.UUID
	kind            string
	body            *string
	clientMessageID *uuid.UUID
	replyToID       *uuid.UUID
	transactionID   *uuid.UUID
	moneyRequestID  *uuid.UUID
}

// InsertMessage appends a message at the next sequence number. The caller must hold
// the conversation lock.
func (r *Repository) InsertMessage(ctx context.Context, tx pgx.Tx, c *lockedConversation, m newMessage) (uuid.UUID, error) {
	seq := c.lastSeq + 1
	var id uuid.UUID
	err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
		return sp.QueryRow(ctx, `
			INSERT INTO messages (conversation_id, sender_id, seq, kind, body, client_message_id, reply_to_id, transaction_id, money_request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
			m.conversationID, m.senderID, seq, m.kind, m.body, m.clientMessageID, m.replyToID, m.transactionID, m.moneyRequestID).Scan(&id)
	})
	if name, ok := postgres.ConstraintError(err, postgres.CodeUniqueViolation); ok && name == "messages_client_id_key" {
		return uuid.Nil, apperr.ErrClientIDReused
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert message: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE conversations SET last_seq = $2, last_message_at = now() WHERE id = $1`, c.id, seq)
	if err != nil {
		return uuid.Nil, fmt.Errorf("advance conversation: %w", err)
	}
	// Senders have read their own message.
	_, err = tx.Exec(ctx, `
		UPDATE conversation_members SET last_read_seq = $3
		WHERE conversation_id = $1 AND user_id = $2 AND last_read_seq < $3`, c.id, m.senderID, seq)
	if err != nil {
		return uuid.Nil, fmt.Errorf("advance read pointer: %w", err)
	}
	c.lastSeq = seq
	return id, nil
}

// FindByClientID returns the id of the sender's message with this client id, if any.
func (r *Repository) FindByClientID(ctx context.Context, q postgres.DBTX, sender, clientID uuid.UUID) (uuid.UUID, uuid.UUID, bool, error) {
	var id, conv uuid.UUID
	err := q.QueryRow(ctx, `SELECT id, conversation_id FROM messages WHERE sender_id = $1 AND client_message_id = $2`,
		sender, clientID).Scan(&id, &conv)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("find message by client id: %w", err)
	}
	return id, conv, true, nil
}

type messageRow struct {
	id, conversationID, senderID uuid.UUID
	seq                          int64
	kind                         string
	body                         *string
	version                      int
	createdAt                    time.Time
	deletedAt                    *time.Time
}

// LockMessage locks a message in a conversation.
func (r *Repository) LockMessage(ctx context.Context, tx pgx.Tx, conversationID, id uuid.UUID) (messageRow, error) {
	var m messageRow
	err := tx.QueryRow(ctx, `
		SELECT id, conversation_id, sender_id, seq, kind, body, version, created_at, deleted_at
		FROM messages WHERE id = $1 AND conversation_id = $2 FOR UPDATE`, id, conversationID).
		Scan(&m.id, &m.conversationID, &m.senderID, &m.seq, &m.kind, &m.body, &m.version, &m.createdAt, &m.deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return messageRow{}, apperr.ErrMessageNotFound
	}
	if err != nil {
		return messageRow{}, fmt.Errorf("lock message: %w", err)
	}
	return m, nil
}

func (r *Repository) MessageInConversation(ctx context.Context, q postgres.DBTX, conversationID, id uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1 AND conversation_id = $2)`, id, conversationID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check reply target: %w", err)
	}
	return ok, nil
}

func (r *Repository) EditMessage(ctx context.Context, tx pgx.Tx, m messageRow, body string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO message_edits (message_id, previous_body, version) VALUES ($1, $2, $3)`,
		m.id, *m.body, m.version); err != nil {
		return fmt.Errorf("record edit: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE messages SET body = $2, version = version + 1, edited_at = now() WHERE id = $1`,
		m.id, body); err != nil {
		return fmt.Errorf("edit message: %w", err)
	}
	return nil
}

func (r *Repository) DeleteMessage(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE messages SET body = NULL, version = version + 1, deleted_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	return nil
}

// TouchMessage marks a message as changed (e.g. its money request was paid).
func (r *Repository) TouchMessage(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE messages SET updated_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("touch message: %w", err)
	}
	return nil
}

// MessageForRequest returns the message carrying a money request, if any.
func (r *Repository) MessageForRequest(ctx context.Context, q postgres.DBTX, requestID uuid.UUID) (uuid.UUID, uuid.UUID, bool, error) {
	var id, conv uuid.UUID
	err := q.QueryRow(ctx, `SELECT id, conversation_id FROM messages WHERE money_request_id = $1`, requestID).Scan(&id, &conv)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("find request message: %w", err)
	}
	return id, conv, true, nil
}

// MessageIDFor returns the message for a transaction or money request.
func (r *Repository) MessageIDFor(ctx context.Context, q postgres.DBTX, column string, id uuid.UUID) (uuid.UUID, error) {
	if column != "transaction_id" && column != "money_request_id" {
		return uuid.Nil, fmt.Errorf("unsupported column %q", column)
	}
	var msgID uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM messages WHERE `+column+` = $1`, id).Scan(&msgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, apperr.ErrMessageNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("find message: %w", err)
	}
	return msgID, nil
}

// MarkRead advances the viewer's read pointer, never past the newest message and
// never backwards. It reports whether anything changed.
func (r *Repository) MarkRead(ctx context.Context, tx pgx.Tx, c lockedConversation, viewer uuid.UUID, upTo int64) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE conversation_members SET last_read_seq = LEAST($3::bigint, $4::bigint)
		WHERE conversation_id = $1 AND user_id = $2 AND last_read_seq < LEAST($3::bigint, $4::bigint)`,
		c.id, viewer, upTo, c.lastSeq)
	if err != nil {
		return false, fmt.Errorf("mark read: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

type entityRef struct {
	kind string // "conversation" or "message"
	id   uuid.UUID
}

// RecordChanges publishes entity changes to each member's change feed. Each member's
// cursor row is locked (in sorted order) until commit, so feed positions become
// visible in order. It returns the first position allocated per member.
func (r *Repository) RecordChanges(ctx context.Context, tx pgx.Tx, conversationID uuid.UUID, members []uuid.UUID, entities ...entityRef) (map[uuid.UUID]int64, error) {
	first := make(map[uuid.UUID]int64, len(members))
	n := int64(len(entities))
	for _, uid := range sortedMembers(members...) {
		var last int64
		err := tx.QueryRow(ctx, `
			INSERT INTO chat_sync_cursors (user_id, last_seq) VALUES ($1, $2)
			ON CONFLICT (user_id) DO UPDATE SET last_seq = chat_sync_cursors.last_seq + $2
			RETURNING last_seq`, uid, n).Scan(&last)
		if err != nil {
			return nil, fmt.Errorf("advance sync cursor: %w", err)
		}
		first[uid] = last - n + 1
		for i, e := range entities {
			_, err := tx.Exec(ctx, `
				INSERT INTO chat_changes (user_id, entity_type, entity_id, conversation_id, seq)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (user_id, entity_type, entity_id) DO UPDATE SET seq = EXCLUDED.seq`,
				uid, e.kind, e.id, conversationID, first[uid]+int64(i))
			if err != nil {
				return nil, fmt.Errorf("record change: %w", err)
			}
		}
	}
	return first, nil
}

// LatestSeq returns the newest feed position for a user.
func (r *Repository) LatestSeq(ctx context.Context, q postgres.DBTX, viewer uuid.UUID) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `SELECT COALESCE((SELECT last_seq FROM chat_sync_cursors WHERE user_id = $1), 0)`, viewer).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("latest sync seq: %w", err)
	}
	return seq, nil
}

type change struct {
	seq      int64
	kind     string
	entityID uuid.UUID
}

func (r *Repository) Changes(ctx context.Context, q postgres.DBTX, viewer uuid.UUID, after int64, limit int) ([]change, error) {
	rows, err := q.Query(ctx, `
		SELECT seq, entity_type, entity_id FROM chat_changes
		WHERE user_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, viewer, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list changes: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (change, error) {
		var c change
		return c, row.Scan(&c.seq, &c.kind, &c.entityID)
	})
}

// idList encodes ids for `= ANY(string_to_array($n, ',')::uuid[])`, which works with
// pgx's exec query mode (needed behind connection poolers).
func idList(ids []uuid.UUID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = id.String()
	}
	return strings.Join(s, ",")
}

const messageSelect = `
	SELECT m.id, m.conversation_id, m.seq, m.client_message_id, m.sender_id, m.kind, m.body, m.reply_to_id,
	       m.version, m.edited_at, m.deleted_at, m.created_at, m.updated_at,
	       t.id, t.reference, t.amount_pence, t.status,
	       r.id, r.amount_pence, r.status, r.expires_at, r.requester_user_id
	FROM messages m
	LEFT JOIN transactions t ON t.id = m.transaction_id
	LEFT JOIN money_requests r ON r.id = m.money_request_id`

func (r *Repository) scanMessages(rows pgx.Rows, viewer uuid.UUID) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			m                       Message
			id, conv, sender        uuid.UUID
			clientID, replyTo       *uuid.UUID
			txnID, reqID, requester *uuid.UUID
			txnRef, txnStatus       *string
			txnAmount, reqAmount    *int64
			reqStatus               *string
			reqExpires              *time.Time
		)
		if err := rows.Scan(&id, &conv, &m.Seq, &clientID, &sender, &m.Kind, &m.Body, &replyTo,
			&m.Version, &m.EditedAt, &m.DeletedAt, &m.CreatedAt, &m.UpdatedAt,
			&txnID, &txnRef, &txnAmount, &txnStatus,
			&reqID, &reqAmount, &reqStatus, &reqExpires, &requester); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.ID, m.ConversationID, m.SenderID = id.String(), conv.String(), sender.String()
		m.Mine = sender == viewer
		if clientID != nil && m.Mine {
			s := clientID.String()
			m.ClientMessageID = &s
		}
		if replyTo != nil {
			s := replyTo.String()
			m.ReplyToID = &s
		}
		m.Edited, m.Deleted = m.EditedAt != nil, m.DeletedAt != nil
		if txnID != nil {
			dir := "in"
			if m.Mine {
				dir = "out"
			}
			m.Payment = &PaymentInfo{TransactionID: txnID.String(), Reference: *txnRef, Amount: money.New(*txnAmount), Direction: dir, Status: *txnStatus}
		}
		if reqID != nil {
			info := &RequestInfo{MoneyRequestID: reqID.String(), Amount: money.New(*reqAmount), Status: *reqStatus, ExpiresAt: *reqExpires, Actions: []string{}}
			if info.Status == "pending" && !r.now().Before(info.ExpiresAt) {
				info.Status = "expired"
			}
			if *requester == viewer {
				info.Direction = "outgoing"
				if info.Status == "pending" {
					info.Actions = []string{"cancel"}
				}
			} else {
				info.Direction = "incoming"
				if info.Status == "pending" {
					info.Actions = []string{"pay", "decline"}
				}
			}
			m.Request = info
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MessagesByID loads messages visible to viewer, keyed by id.
func (r *Repository) MessagesByID(ctx context.Context, q postgres.DBTX, viewer uuid.UUID, ids []uuid.UUID) (map[string]Message, error) {
	out := map[string]Message{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, messageSelect+`
		JOIN conversation_members cm ON cm.conversation_id = m.conversation_id AND cm.user_id = $1
		WHERE m.id = ANY(string_to_array($2, ',')::uuid[])`, viewer, idList(ids))
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	msgs, err := r.scanMessages(rows, viewer)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		out[m.ID] = m
	}
	return out, nil
}

func (r *Repository) Message(ctx context.Context, q postgres.DBTX, viewer, id uuid.UUID) (Message, error) {
	msgs, err := r.MessagesByID(ctx, q, viewer, []uuid.UUID{id})
	if err != nil {
		return Message{}, err
	}
	m, ok := msgs[id.String()]
	if !ok {
		return Message{}, apperr.ErrMessageNotFound
	}
	return m, nil
}

// History returns messages older than beforeSeq (newest first).
func (r *Repository) History(ctx context.Context, q postgres.DBTX, viewer, conversationID uuid.UUID, beforeSeq int64, limit int) ([]Message, error) {
	rows, err := q.Query(ctx, messageSelect+`
		WHERE m.conversation_id = $1 AND m.seq < $2
		ORDER BY m.seq DESC LIMIT $3`, conversationID, beforeSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	return r.scanMessages(rows, viewer)
}

const conversationSelect = `
	SELECT c.id, c.kind, c.last_seq, c.last_message_at, c.created_at, c.updated_at,
	       me.last_read_seq, peer.last_read_seq,
	       pu.id, pu.username, pp.first_name, pp.last_name, pp.avatar_url,
	       (SELECT count(*) FROM messages um
	        WHERE um.conversation_id = c.id AND um.seq > me.last_read_seq
	          AND um.sender_id <> $1 AND um.deleted_at IS NULL),
	       lm.id
	FROM conversations c
	JOIN conversation_members me ON me.conversation_id = c.id AND me.user_id = $1
	JOIN conversation_members peer ON peer.conversation_id = c.id AND peer.user_id <> $1
	JOIN users pu ON pu.id = peer.user_id
	LEFT JOIN profiles pp ON pp.user_id = pu.id
	LEFT JOIN messages lm ON lm.conversation_id = c.id AND lm.seq = c.last_seq`

func (r *Repository) scanConversations(ctx context.Context, q postgres.DBTX, viewer uuid.UUID, rows pgx.Rows) ([]Conversation, error) {
	var (
		out     []Conversation
		lastIDs []uuid.UUID
		lastFor = map[int]uuid.UUID{}
	)
	for rows.Next() {
		var (
			c                   Conversation
			id, peerID          uuid.UUID
			username            *string
			first, last, avatar *string
			lastMsg             *uuid.UUID
		)
		if err := rows.Scan(&id, &c.Kind, &c.LastSeq, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt,
			&c.MyLastReadSeq, &c.PeerLastReadSeq, &peerID, &username, &first, &last, &avatar,
			&c.UnreadCount, &lastMsg); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		c.ID = id.String()
		c.Peer = Peer{UserID: peerID.String(), Username: username, AvatarURL: avatar,
			DisplayName: user.DisplayName(user.User{Username: username}, user.Profile{FirstName: first, LastName: last})}
		if lastMsg != nil {
			lastFor[len(out)] = *lastMsg
			lastIDs = append(lastIDs, *lastMsg)
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	msgs, err := r.MessagesByID(ctx, q, viewer, lastIDs)
	if err != nil {
		return nil, err
	}
	for i, id := range lastFor {
		if m, ok := msgs[id.String()]; ok {
			out[i].LastMessage = &m
		}
	}
	return out, nil
}

func (r *Repository) ConversationsByID(ctx context.Context, q postgres.DBTX, viewer uuid.UUID, ids []uuid.UUID) (map[string]Conversation, error) {
	out := map[string]Conversation{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, conversationSelect+` WHERE c.id = ANY(string_to_array($2, ',')::uuid[])`, viewer, idList(ids))
	if err != nil {
		return nil, fmt.Errorf("load conversations: %w", err)
	}
	convs, err := r.scanConversations(ctx, q, viewer, rows)
	if err != nil {
		return nil, err
	}
	for _, c := range convs {
		out[c.ID] = c
	}
	return out, nil
}

func (r *Repository) Conversation(ctx context.Context, q postgres.DBTX, viewer, id uuid.UUID) (Conversation, error) {
	convs, err := r.ConversationsByID(ctx, q, viewer, []uuid.UUID{id})
	if err != nil {
		return Conversation{}, err
	}
	c, ok := convs[id.String()]
	if !ok {
		return Conversation{}, apperr.ErrConversationNotFound
	}
	return c, nil
}

// ListConversations returns the viewer's conversations, most recently active first.
func (r *Repository) ListConversations(ctx context.Context, q postgres.DBTX, viewer uuid.UUID, cursorAt *time.Time, cursorID *uuid.UUID, limit int) ([]Conversation, error) {
	where, args := "", []any{viewer, limit}
	if cursorAt != nil {
		where = ` WHERE (c.last_message_at, c.id) < ($3::timestamptz, $4::uuid)`
		args = append(args, *cursorAt, *cursorID)
	}
	rows, err := q.Query(ctx, conversationSelect+where+` ORDER BY c.last_message_at DESC, c.id DESC LIMIT $2`, args...)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	return r.scanConversations(ctx, q, viewer, rows)
}

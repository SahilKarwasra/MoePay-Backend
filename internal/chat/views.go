package chat

import (
	"time"

	"github.com/sahilkarwasra/moepay/internal/money"
)

const (
	KindText    = "text"
	KindPayment = "payment"
	KindRequest = "request"
)

// Peer is the other member of a direct conversation.
type Peer struct {
	UserID      string  `json:"user_id"`
	Username    *string `json:"username"`
	DisplayName string  `json:"display_name"`
	AvatarURL   *string `json:"avatar_url"`
}

// PaymentInfo is attached to payment messages, from the viewer's perspective.
type PaymentInfo struct {
	TransactionID string       `json:"transaction_id"`
	Reference     string       `json:"reference"`
	Amount        money.Amount `json:"amount"`
	Direction     string       `json:"direction"` // "out" if the viewer paid, else "in"
	Status        string       `json:"status"`
}

// RequestInfo is attached to request messages, from the viewer's perspective.
type RequestInfo struct {
	MoneyRequestID string       `json:"money_request_id"`
	Amount         money.Amount `json:"amount"`
	Direction      string       `json:"direction"` // "outgoing" if the viewer asked, else "incoming"
	Status         string       `json:"status"`
	Actions        []string     `json:"actions"`
	ExpiresAt      time.Time    `json:"expires_at"`
}

// Message is one chat message as seen by one user. The shape is identical in the
// sync feed, list endpoints, write responses and realtime events, so a client can
// upsert it into its local database by id.
type Message struct {
	ID              string       `json:"id"`
	ConversationID  string       `json:"conversation_id"`
	Seq             int64        `json:"seq"`
	ClientMessageID *string      `json:"client_message_id"`
	SenderID        string       `json:"sender_id"`
	Mine            bool         `json:"mine"`
	Kind            string       `json:"kind"`
	Body            *string      `json:"body"`
	ReplyToID       *string      `json:"reply_to_id"`
	Payment         *PaymentInfo `json:"payment"`
	Request         *RequestInfo `json:"request"`
	Version         int          `json:"version"`
	Edited          bool         `json:"edited"`
	EditedAt        *time.Time   `json:"edited_at"`
	Deleted         bool         `json:"deleted"`
	DeletedAt       *time.Time   `json:"deleted_at"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

// Conversation is a conversation as seen by one member.
type Conversation struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Peer            Peer      `json:"peer"`
	LastSeq         int64     `json:"last_seq"`
	LastMessage     *Message  `json:"last_message"`
	LastMessageAt   time.Time `json:"last_message_at"`
	UnreadCount     int       `json:"unread_count"`
	MyLastReadSeq   int64     `json:"my_last_read_seq"`
	PeerLastReadSeq int64     `json:"peer_last_read_seq"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// SyncItem is one entry of the change feed. Exactly one of Conversation or Message is set.
type SyncItem struct {
	Seq          int64         `json:"seq"`
	Type         string        `json:"type"` // "conversation" or "message"
	Conversation *Conversation `json:"conversation,omitempty"`
	Message      *Message      `json:"message,omitempty"`
}

type SyncPage struct {
	Items     []SyncItem `json:"items"`
	NextAfter int64      `json:"next_after"`
	HasMore   bool       `json:"has_more"`
	LatestSeq int64      `json:"latest_seq"`
}

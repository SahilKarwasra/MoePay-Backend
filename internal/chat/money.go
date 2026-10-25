package chat

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/moneyrequest"
	"github.com/sahilkarwasra/moepay/internal/payment"
)

type clientIDKey struct{}

// withClientID carries the app's client_message_id into the payment hooks, so the
// money message created inside the payment transaction can be matched to the
// pending message in the app's local database.
func withClientID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, clientIDKey{}, id)
}

func clientIDFrom(ctx context.Context) *uuid.UUID {
	if id, ok := ctx.Value(clientIDKey{}).(uuid.UUID); ok {
		return &id
	}
	return nil
}

// onTransfer posts a payment message into the payer/payee conversation, inside the
// transfer's transaction.
func (s *Service) onTransfer(ctx context.Context, tx pgx.Tx, ev payment.TransferEvent) (func(), error) {
	n, err := s.insertMoneyMessage(ctx, tx, ev.FromUserID, ev.ToUserID, newMessage{
		senderID: ev.FromUserID, kind: KindPayment, body: ev.Note, transactionID: &ev.TransactionID,
	})
	if err != nil {
		return nil, err
	}
	return func() { s.notify(context.WithoutCancel(ctx), n) }, nil
}

func (s *Service) onRequestCreated(ctx context.Context, tx pgx.Tx, ev moneyrequest.Event) (func(), error) {
	var note *string
	if err := tx.QueryRow(ctx, `SELECT note FROM money_requests WHERE id = $1`, ev.RequestID).Scan(&note); err != nil {
		return nil, err
	}
	n, err := s.insertMoneyMessage(ctx, tx, ev.RequesterID, ev.PayerID, newMessage{
		senderID: ev.RequesterID, kind: KindRequest, body: note, moneyRequestID: &ev.RequestID,
	})
	if err != nil {
		return nil, err
	}
	return func() { s.notify(context.WithoutCancel(ctx), n) }, nil
}

// onRequestResolved republishes the request message so both members see its new status.
func (s *Service) onRequestResolved(ctx context.Context, tx pgx.Tx, ev moneyrequest.Event) (func(), error) {
	msgID, convID, ok, err := s.repo.MessageForRequest(ctx, tx, ev.RequestID)
	if err != nil || !ok {
		return nil, err
	}
	c, err := s.repo.LockConversation(ctx, tx, ev.RequesterID, convID)
	if err != nil {
		return nil, err
	}
	if err := s.repo.TouchMessage(ctx, tx, msgID); err != nil {
		return nil, err
	}
	n, err := s.recordMessage(ctx, tx, c, msgID)
	if err != nil {
		return nil, err
	}
	return func() { s.notify(context.WithoutCancel(ctx), n) }, nil
}

func (s *Service) insertMoneyMessage(ctx context.Context, tx pgx.Tx, sender, peer uuid.UUID, m newMessage) (notification, error) {
	convID, _, err := s.repo.EnsureDirect(ctx, tx, sender, peer)
	if err != nil {
		return nil, err
	}
	c, err := s.repo.LockConversation(ctx, tx, sender, convID)
	if err != nil {
		return nil, err
	}
	m.conversationID = convID
	m.clientMessageID = clientIDFrom(ctx)
	msgID, err := s.repo.InsertMessage(ctx, tx, &c, m)
	if err != nil {
		return nil, err
	}
	return s.recordMessage(ctx, tx, c, msgID)
}

type PayInput struct {
	ClientMessageID string `json:"client_message_id"`
	AmountPence     int64  `json:"amount_pence"`
	Note            string `json:"note"`
	PIN             string `json:"pin"`
}

// PayResult is a completed payment plus the chat message that shows it.
type PayResult struct {
	payment.Result
	Message Message `json:"message"`
}

// Pay sends money to the other member of a conversation. It goes through the normal
// transfer path (PIN, limits, idempotency); the payment message is written in the
// same transaction by the transfer hook.
func (s *Service) Pay(ctx context.Context, me, conversationID uuid.UUID, key string, in PayInput) (PayResult, bool, error) {
	clientID, err := parseClientID(in.ClientMessageID)
	if err != nil {
		return PayResult{}, false, err
	}
	peer, err := s.repo.PeerOf(ctx, s.pool, me, conversationID)
	if err != nil {
		return PayResult{}, false, err
	}
	res, replayed, err := s.payments.Transfer(withClientID(ctx, clientID), me, key, payment.TransferInput{
		To: payment.RecipientRef{UserID: peer.String()}, AmountPence: in.AmountPence, Note: in.Note, PIN: in.PIN,
	})
	if err != nil {
		return PayResult{}, replayed, err
	}
	msgID, err := s.repo.MessageIDFor(ctx, s.pool, "transaction_id", uuid.MustParse(res.Transaction.ID))
	if err != nil {
		return PayResult{}, replayed, err
	}
	m, err := s.repo.Message(ctx, s.pool, me, msgID)
	return PayResult{Result: res, Message: m}, replayed, err
}

type RequestInput struct {
	ClientMessageID string `json:"client_message_id"`
	AmountPence     int64  `json:"amount_pence"`
	Note            string `json:"note"`
}

type RequestResult struct {
	Request moneyrequest.View `json:"request"`
	Message Message           `json:"message"`
}

// Request asks the other member of a conversation for money. Retrying with the same
// client_message_id returns the original request.
func (s *Service) Request(ctx context.Context, me, conversationID uuid.UUID, in RequestInput) (RequestResult, error) {
	clientID, err := parseClientID(in.ClientMessageID)
	if err != nil {
		return RequestResult{}, err
	}
	peer, err := s.repo.PeerOf(ctx, s.pool, me, conversationID)
	if err != nil {
		return RequestResult{}, err
	}
	if existing, ok, err := s.existing(ctx, me, conversationID, clientID); err != nil || ok {
		if err == nil && existing.Request == nil {
			err = apperr.ErrClientIDReused
		}
		if err != nil {
			return RequestResult{}, err
		}
		v, err := s.requests.Get(ctx, me, uuid.MustParse(existing.Request.MoneyRequestID))
		return RequestResult{Request: v, Message: existing}, err
	}
	v, err := s.requests.Create(withClientID(ctx, clientID), me, moneyrequest.CreateInput{
		From: payment.RecipientRef{UserID: peer.String()}, AmountPence: in.AmountPence, Note: in.Note,
	})
	if err != nil {
		return RequestResult{}, err
	}
	msgID, err := s.repo.MessageIDFor(ctx, s.pool, "money_request_id", uuid.MustParse(v.ID))
	if err != nil {
		return RequestResult{}, err
	}
	m, err := s.repo.Message(ctx, s.pool, me, msgID)
	return RequestResult{Request: v, Message: m}, err
}

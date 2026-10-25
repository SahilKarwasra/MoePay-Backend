package moneyrequest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/user"
)

const (
	StatusPending   = "pending"
	StatusPaid      = "paid"
	StatusDeclined  = "declined"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
)

type Party struct {
	UserID      string  `json:"user_id"`
	Username    *string `json:"username"`
	DisplayName string  `json:"display_name"`
	AvatarURL   *string `json:"avatar_url"`
}

// View is a money request from the viewer's perspective.
type View struct {
	ID                string       `json:"id"`
	Direction         string       `json:"direction"` // "incoming" (you are asked to pay) or "outgoing"
	Status            string       `json:"status"`
	Amount            money.Amount `json:"amount"`
	Note              *string      `json:"note"`
	Counterparty      Party        `json:"counterparty"`
	Requester         Party        `json:"requester"`
	Payer             Party        `json:"payer"`
	Actions           []string     `json:"actions"`
	PaidTransactionID *string      `json:"paid_transaction_id"`
	ExpiresAt         time.Time    `json:"expires_at"`
	ResolvedAt        *time.Time   `json:"resolved_at"`
	CreatedAt         time.Time    `json:"created_at"`

	requesterID uuid.UUID
	payerID     uuid.UUID
	amount      int64
}

type Page struct {
	Items      []View  `json:"items"`
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

type Repository struct{ now func() time.Time }

func NewRepository() *Repository { return &Repository{now: time.Now} }

const selectViews = `
	SELECT r.id, r.requester_user_id, r.payer_user_id, r.amount_pence, r.note, r.status, r.paid_transaction_id,
	       r.expires_at, r.resolved_at, r.created_at,
	       ru.username, rp.first_name, rp.last_name, rp.avatar_url,
	       pu.username, pp.first_name, pp.last_name, pp.avatar_url
	FROM money_requests r
	JOIN users ru ON ru.id = r.requester_user_id
	LEFT JOIN profiles rp ON rp.user_id = ru.id
	JOIN users pu ON pu.id = r.payer_user_id
	LEFT JOIN profiles pp ON pp.user_id = pu.id`

func (r *Repository) scan(rows pgx.Rows, viewer uuid.UUID) ([]View, error) {
	defer rows.Close()
	var out []View
	for rows.Next() {
		var (
			v              View
			id             uuid.UUID
			paid           *uuid.UUID
			ru, rf, rl, ra *string
			pu, pf, pl, pa *string
		)
		if err := rows.Scan(&id, &v.requesterID, &v.payerID, &v.amount, &v.Note, &v.Status, &paid,
			&v.ExpiresAt, &v.ResolvedAt, &v.CreatedAt, &ru, &rf, &rl, &ra, &pu, &pf, &pl, &pa); err != nil {
			return nil, fmt.Errorf("scan money request: %w", err)
		}
		v.ID = id.String()
		v.Amount = money.New(v.amount)
		if paid != nil {
			s := paid.String()
			v.PaidTransactionID = &s
		}
		v.Requester = party(v.requesterID, ru, rf, rl, ra)
		v.Payer = party(v.payerID, pu, pf, pl, pa)
		// Expiry is persisted by a background job; reflect it immediately in reads.
		if v.Status == StatusPending && !r.now().Before(v.ExpiresAt) {
			v.Status = StatusExpired
		}
		v.Actions = []string{}
		if viewer == v.payerID {
			v.Direction, v.Counterparty = "incoming", v.Requester
			if v.Status == StatusPending {
				v.Actions = []string{"pay", "decline"}
			}
		} else {
			v.Direction, v.Counterparty = "outgoing", v.Payer
			if v.Status == StatusPending {
				v.Actions = []string{"cancel"}
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func party(id uuid.UUID, username, first, last, avatar *string) Party {
	return Party{
		UserID: id.String(), Username: username, AvatarURL: avatar,
		DisplayName: user.DisplayName(user.User{Username: username}, user.Profile{FirstName: first, LastName: last}),
	}
}

func (r *Repository) Insert(ctx context.Context, q postgres.DBTX, requester, payer uuid.UUID, amount int64, note *string, ttl time.Duration) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO money_requests (requester_user_id, payer_user_id, amount_pence, note, expires_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5)) RETURNING id`,
		requester, payer, amount, note, ttl.Seconds()).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert money request: %w", err)
	}
	return id, nil
}

func (r *Repository) CountPendingBetween(ctx context.Context, q postgres.DBTX, requester, payer uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM money_requests
		WHERE requester_user_id = $1 AND payer_user_id = $2 AND status = 'pending' AND expires_at > now()`,
		requester, payer).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending requests: %w", err)
	}
	return n, nil
}

// Get returns a request visible to viewer (as requester or payer). With forUpdate the
// row is locked.
func (r *Repository) Get(ctx context.Context, q postgres.DBTX, viewer, id uuid.UUID, forUpdate bool) (View, error) {
	query := selectViews + ` WHERE r.id = $1 AND (r.requester_user_id = $2 OR r.payer_user_id = $2)`
	if forUpdate {
		query += ` FOR UPDATE OF r`
	}
	rows, err := q.Query(ctx, query, id, viewer)
	if err != nil {
		return View{}, fmt.Errorf("get money request: %w", err)
	}
	items, err := r.scan(rows, viewer)
	if err != nil {
		return View{}, err
	}
	if len(items) == 0 {
		return View{}, apperr.ErrMoneyRequestNotFound
	}
	return items[0], nil
}

func (r *Repository) Resolve(ctx context.Context, q postgres.DBTX, id uuid.UUID, status string, paidTxn *uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		UPDATE money_requests SET status = $2, paid_transaction_id = $3, resolved_at = now()
		WHERE id = $1 AND status = 'pending'`, id, status, paidTxn)
	if err != nil {
		return fmt.Errorf("resolve money request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.ErrRequestNotPending
	}
	return nil
}

type ListFilter struct {
	Direction string // incoming or outgoing
	Status    string
	Cursor    string
	Limit     int
}

func (r *Repository) List(ctx context.Context, q postgres.DBTX, viewer uuid.UUID, f ListFilter) (Page, error) {
	col := "r.payer_user_id"
	if f.Direction == "outgoing" {
		col = "r.requester_user_id"
	}
	args := []any{viewer}
	where := []string{col + " = $1"}
	switch f.Status {
	case "":
	case StatusPending:
		where = append(where, "r.status = 'pending' AND r.expires_at > now()")
	case StatusExpired:
		where = append(where, "(r.status = 'expired' OR (r.status = 'pending' AND r.expires_at <= now()))")
	default:
		args = append(args, f.Status)
		where = append(where, "r.status = $"+strconv.Itoa(len(args)))
	}
	if f.Cursor != "" {
		at, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, err
		}
		args = append(args, at, id)
		where = append(where, fmt.Sprintf("(r.created_at, r.id) < ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args)))
	}
	args = append(args, f.Limit+1)
	rows, err := q.Query(ctx, selectViews+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY r.created_at DESC, r.id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return Page{}, fmt.Errorf("list money requests: %w", err)
	}
	items, err := r.scan(rows, viewer)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: items}
	if page.Items == nil {
		page.Items = []View{}
	}
	if len(items) > f.Limit {
		page.Items, page.HasMore = items[:f.Limit], true
		last := page.Items[f.Limit-1]
		c := base64.RawURLEncoding.EncodeToString([]byte(last.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + last.ID))
		page.NextCursor = &c
	}
	return page, nil
}

type Counts struct {
	IncomingPending int `json:"incoming_pending"`
	OutgoingPending int `json:"outgoing_pending"`
}

func (r *Repository) Counts(ctx context.Context, q postgres.DBTX, viewer uuid.UUID) (Counts, error) {
	var c Counts
	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE payer_user_id = $1), count(*) FILTER (WHERE requester_user_id = $1)
		FROM money_requests
		WHERE (payer_user_id = $1 OR requester_user_id = $1) AND status = 'pending' AND expires_at > now()`,
		viewer).Scan(&c.IncomingPending, &c.OutgoingPending)
	if err != nil {
		return Counts{}, fmt.Errorf("count money requests: %w", err)
	}
	return c, nil
}

// ExpireDue marks overdue pending requests as expired and returns them.
func (r *Repository) ExpireDue(ctx context.Context, q postgres.DBTX) ([]Event, error) {
	rows, err := q.Query(ctx, `
		UPDATE money_requests SET status = 'expired', resolved_at = now()
		WHERE status = 'pending' AND expires_at <= now()
		RETURNING id, requester_user_id, payer_user_id`)
	if err != nil {
		return nil, fmt.Errorf("expire money requests: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		ev := Event{Status: StatusExpired}
		return ev, row.Scan(&ev.RequestID, &ev.RequesterID, &ev.PayerID)
	})
}

var errBadCursor = apperr.Validation("Invalid cursor", map[string]string{"cursor": "use next_cursor from the previous page"})

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

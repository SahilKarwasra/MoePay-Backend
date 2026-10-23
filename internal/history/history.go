package history

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
)

// Party is the other side of a transaction from the viewer's perspective.
type Party struct {
	Kind         string  `json:"kind"` // "user" or "bank"
	UserID       *string `json:"user_id,omitempty"`
	Username     *string `json:"username,omitempty"`
	DisplayName  string  `json:"display_name"`
	AvatarURL    *string `json:"avatar_url,omitempty"`
	BankName     *string `json:"bank_name,omitempty"`
	AccountLast4 *string `json:"account_last4,omitempty"`
}

type Failure struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// View is one transaction as seen by one user.
type View struct {
	ID             string        `json:"id"`
	Reference      string        `json:"reference"`
	Type           string        `json:"type"`
	Status         string        `json:"status"`
	Direction      string        `json:"direction"` // "in" or "out"
	Title          string        `json:"title"`
	Amount         money.Amount  `json:"amount"`
	SignedDisplay  string        `json:"signed_display"`
	BalanceAfter   *money.Amount `json:"balance_after"`
	Counterparty   Party         `json:"counterparty"`
	Note           *string       `json:"note"`
	Failure        *Failure      `json:"failure"`
	MoneyRequestID *string       `json:"money_request_id"`
	CreatedAt      time.Time     `json:"created_at"`
	CompletedAt    *time.Time    `json:"completed_at"`
}

type Page struct {
	Items      []View  `json:"items"`
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

type Filter struct {
	Types     []string
	Status    string
	Direction string
	From, To  *time.Time
	Cursor    string
	Limit     int
}

const (
	DefaultLimit = 20
	MaxLimit     = 50
)

var validTypes = map[string]bool{"p2p_transfer": true, "add_money": true, "withdraw": true, "request_payment": true}

type Reader struct{}

func NewReader() *Reader { return &Reader{} }

// List returns one page of a wallet's history, newest first. Pagination is keyset
// based on (created_at, id), so pages stay stable while new transactions arrive.
func (r *Reader) List(ctx context.Context, q postgres.DBTX, walletID uuid.UUID, f Filter) (Page, error) {
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	f.Limit = min(f.Limit, MaxLimit)

	args := []any{walletID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	var cond []string
	if len(f.Types) > 0 {
		cond = append(cond, "t.type = ANY(string_to_array("+arg(strings.Join(f.Types, ","))+", ','))")
	}
	if f.Status != "" {
		cond = append(cond, "t.status = "+arg(f.Status))
	}
	if f.From != nil {
		cond = append(cond, "t.created_at >= "+arg(*f.From)+"::timestamptz")
	}
	if f.To != nil {
		cond = append(cond, "t.created_at < "+arg(*f.To)+"::timestamptz")
	}
	if f.Cursor != "" {
		at, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, err
		}
		cond = append(cond, "(t.created_at, t.id) < ("+arg(at)+"::timestamptz, "+arg(id)+"::uuid)")
	}
	where := ""
	if len(cond) > 0 {
		where = " AND " + strings.Join(cond, " AND ")
	}
	limit := arg(f.Limit + 1)

	branch := func(column, dir string) string {
		return `(SELECT t.id, '` + dir + `' AS dir FROM transactions t WHERE t.` + column + ` = $1` + where +
			` ORDER BY t.created_at DESC, t.id DESC LIMIT ` + limit + `)`
	}
	var branches []string
	if f.Direction != "in" {
		branches = append(branches, branch("from_wallet_id", "out"))
	}
	if f.Direction != "out" {
		branches = append(branches, branch("to_wallet_id", "in"))
	}
	pageSQL := strings.Join(branches, " UNION ALL ")

	items, err := r.query(ctx, q, pageSQL, limit, args)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: items}
	if len(items) > f.Limit {
		page.Items, page.HasMore = items[:f.Limit], true
		last := page.Items[f.Limit-1]
		c := encodeCursor(last.CreatedAt, last.ID)
		page.NextCursor = &c
	}
	return page, nil
}

// Get returns one transaction if it belongs to walletID.
func (r *Reader) Get(ctx context.Context, q postgres.DBTX, walletID, txnID uuid.UUID) (View, error) {
	items, err := r.query(ctx, q, `
		SELECT t.id, CASE WHEN t.from_wallet_id = $1 THEN 'out' ELSE 'in' END AS dir
		FROM transactions t WHERE t.id = $2 AND (t.from_wallet_id = $1 OR t.to_wallet_id = $1)`, "1",
		[]any{walletID, txnID})
	if err != nil {
		return View{}, err
	}
	if len(items) == 0 {
		return View{}, apperr.ErrTransactionNotFound
	}
	return items[0], nil
}

func (r *Reader) query(ctx context.Context, q postgres.DBTX, pageSQL, limit string, args []any) ([]View, error) {
	rows, err := q.Query(ctx, `
		WITH page AS (`+pageSQL+`)
		SELECT t.id, t.reference, t.type, t.status, t.amount_pence, page.dir, t.note,
		       t.failure_code, t.failure_reason, t.money_request_id, t.created_at, t.completed_at,
		       cu.id, cu.username, cp.first_name, cp.last_name, cp.avatar_url,
		       b.bank_name, b.account_last4, le.balance_after_pence
		FROM page
		JOIN transactions t ON t.id = page.id
		LEFT JOIN wallets cw ON cw.id = CASE WHEN page.dir = 'out' THEN t.to_wallet_id ELSE t.from_wallet_id END
		LEFT JOIN users cu ON cu.id = cw.user_id
		LEFT JOIN profiles cp ON cp.user_id = cu.id
		LEFT JOIN bank_accounts b ON b.id = t.bank_account_id
		LEFT JOIN ledger_entries le ON le.transaction_id = t.id AND le.wallet_id = $1
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT `+limit, args...)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	defer rows.Close()
	var out []View
	for rows.Next() {
		var (
			v                       View
			id                      uuid.UUID
			amount                  int64
			failCode, failReason    *string
			requestID, cuID         *uuid.UUID
			username, first, last   *string
			avatar, bankName, last4 *string
			balanceAfter            *int64
		)
		if err := rows.Scan(&id, &v.Reference, &v.Type, &v.Status, &amount, &v.Direction, &v.Note,
			&failCode, &failReason, &requestID, &v.CreatedAt, &v.CompletedAt,
			&cuID, &username, &first, &last, &avatar, &bankName, &last4, &balanceAfter); err != nil {
			return nil, fmt.Errorf("scan transaction: %w", err)
		}
		v.ID = id.String()
		v.Amount = money.New(amount)
		sign := "+"
		if v.Direction == "out" {
			sign = "-"
		}
		v.SignedDisplay = sign + v.Amount.Display
		if balanceAfter != nil {
			b := money.New(*balanceAfter)
			v.BalanceAfter = &b
		}
		if failCode != nil {
			v.Failure = &Failure{Code: *failCode, Reason: deref(failReason)}
		}
		if requestID != nil {
			s := requestID.String()
			v.MoneyRequestID = &s
		}
		if bankName != nil && (v.Type == "add_money" || v.Type == "withdraw") {
			v.Counterparty = Party{Kind: "bank", DisplayName: *bankName + " ••" + deref(last4), BankName: bankName, AccountLast4: last4}
		} else if cuID != nil {
			uid := cuID.String()
			v.Counterparty = Party{Kind: "user", UserID: &uid, Username: username, AvatarURL: avatar,
				DisplayName: displayName(first, last, username)}
		}
		v.Title = title(v)
		out = append(out, v)
	}
	return out, rows.Err()
}

func title(v View) string {
	name := v.Counterparty.DisplayName
	switch v.Type {
	case "p2p_transfer":
		if v.Direction == "out" {
			return "Sent to " + name
		}
		return "Received from " + name
	case "request_payment":
		if v.Direction == "out" {
			return "Paid " + name + "'s request"
		}
		return "Request paid by " + name
	case "add_money":
		if v.Status == "failed" {
			return "Add money failed"
		}
		return "Added from " + name
	case "withdraw":
		if v.Status == "failed" {
			return "Withdrawal failed"
		}
		return "Withdrawn to " + name
	}
	return v.Type
}

func displayName(first, last, username *string) string {
	var parts []string
	if first != nil {
		parts = append(parts, *first)
	}
	if last != nil {
		parts = append(parts, *last)
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	if username != nil {
		return "@" + *username
	}
	return "MoePay user"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var errBadCursor = apperr.Validation("Invalid cursor", map[string]string{"cursor": "use next_cursor from the previous page"})

func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
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
	if err := errors.Join(err1, err2); err != nil {
		return time.Time{}, uuid.Nil, errBadCursor
	}
	return at, id, nil
}

// ParseFilter validates query parameters. Dates are calendar days in loc; "to" is inclusive.
func ParseFilter(get func(string) string, loc *time.Location) (Filter, error) {
	f := Filter{Cursor: get("cursor"), Status: get("status"), Direction: get("direction")}
	fields := map[string]string{}
	if raw := get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxLimit {
			fields["limit"] = fmt.Sprintf("must be 1-%d", MaxLimit)
		}
		f.Limit = n
	}
	if raw := get("type"); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); !validTypes[t] {
				fields["type"] = "one or more of p2p_transfer, add_money, withdraw, request_payment"
			}
			f.Types = append(f.Types, t)
		}
	}
	if f.Status != "" && f.Status != "succeeded" && f.Status != "failed" {
		fields["status"] = "succeeded or failed"
	}
	if f.Direction != "" && f.Direction != "in" && f.Direction != "out" {
		fields["direction"] = "in or out"
	}
	for _, k := range []string{"from", "to"} {
		raw := get(k)
		if raw == "" {
			continue
		}
		d, err := time.ParseInLocation(time.DateOnly, raw, loc)
		if err != nil {
			fields[k] = "YYYY-MM-DD"
			continue
		}
		if k == "to" {
			d = d.AddDate(0, 0, 1)
			f.To = &d
		} else {
			f.From = &d
		}
	}
	if len(fields) > 0 {
		return Filter{}, apperr.Validation("Invalid filters", fields)
	}
	return f, nil
}

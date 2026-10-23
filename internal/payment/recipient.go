package payment

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/auth"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/user"
	"github.com/sahilkarwasra/moepay/internal/username"
	"github.com/sahilkarwasra/moepay/internal/wallet"
)

// RecipientRef identifies who to pay or request from. Exactly one field is set.
type RecipientRef struct {
	UserID   string `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
	Phone    string `json:"phone,omitempty"`
}

// Recipient is what the app shows before confirming a payment. It never includes a
// balance or the full phone number.
type Recipient struct {
	UserID      string  `json:"user_id"`
	Username    *string `json:"username"`
	DisplayName string  `json:"display_name"`
	AvatarURL   *string `json:"avatar_url"`
	PhoneMasked string  `json:"phone_masked"`

	walletID uuid.UUID
}

// ResolvedQR is a recipient plus any amount or note embedded in the QR code.
type ResolvedQR struct {
	Recipient Recipient     `json:"recipient"`
	Amount    *money.Amount `json:"amount"`
	Note      *string       `json:"note"`
}

// Resolve finds a payable MoePay user. Users who have not finished onboarding (and so
// have no wallet) are reported as not found.
func (s *Service) Resolve(ctx context.Context, q postgres.DBTX, me uuid.UUID, ref RecipientRef) (Recipient, error) {
	set := 0
	for _, v := range []string{ref.UserID, ref.Username, ref.Phone} {
		if strings.TrimSpace(v) != "" {
			set++
		}
	}
	if set != 1 {
		return Recipient{}, apperr.Validation("Provide exactly one of user_id, username or phone", map[string]string{"recipient": "exactly one of user_id, username, phone"})
	}

	var (
		u   user.User
		err error
	)
	switch {
	case ref.UserID != "":
		id, perr := uuid.Parse(strings.TrimSpace(ref.UserID))
		if perr != nil {
			return Recipient{}, apperr.ErrRecipientNotFound
		}
		u, err = s.users.GetByID(ctx, q, id)
	case ref.Username != "":
		_, normalized, nerr := username.Normalize(ref.Username)
		if nerr != nil {
			return Recipient{}, apperr.ErrRecipientNotFound
		}
		u, err = s.users.FindByUsername(ctx, q, normalized)
	default:
		phone, perr := auth.NormalizePhone(ref.Phone, s.region)
		if perr != nil {
			return Recipient{}, perr
		}
		u, err = s.users.FindByPhone(ctx, q, phone)
	}
	if errors.Is(err, apperr.ErrNotFound) {
		return Recipient{}, apperr.ErrRecipientNotFound
	}
	if err != nil {
		return Recipient{}, err
	}
	if u.ID == me {
		return Recipient{}, apperr.ErrCannotPaySelf
	}
	if u.Status != user.StatusActive {
		return Recipient{}, apperr.ErrRecipientNotFound
	}
	w, err := s.wallets.ForUser(ctx, q, u.ID)
	if errors.Is(err, apperr.ErrWalletNotReady) {
		return Recipient{}, apperr.ErrRecipientNotFound
	}
	if err != nil {
		return Recipient{}, err
	}
	p, err := s.users.GetProfile(ctx, q, u.ID)
	if err != nil {
		return Recipient{}, err
	}
	return Recipient{
		UserID: u.ID.String(), Username: u.Username, DisplayName: user.DisplayName(u, p),
		AvatarURL: p.AvatarURL, PhoneMasked: auth.MaskPhone(u.PhoneE164), walletID: w.ID,
	}, nil
}

// ResolveQR parses a payload produced by GET /wallet/qr, e.g. moepay://pay?u=alice&a=500&n=lunch.
func (s *Service) ResolveQR(ctx context.Context, me uuid.UUID, payload string) (ResolvedQR, error) {
	bad := apperr.Validation("Not a MoePay QR code", map[string]string{"qr": "expected moepay://pay?u=<username>"})
	u, err := url.Parse(strings.TrimSpace(payload))
	if err != nil || u.Scheme != wallet.QRScheme || u.Host != "pay" {
		return ResolvedQR{}, bad
	}
	q := u.Query()
	if q.Get("u") == "" {
		return ResolvedQR{}, bad
	}
	r, err := s.Resolve(ctx, s.pool, me, RecipientRef{Username: q.Get("u")})
	if err != nil {
		return ResolvedQR{}, err
	}
	out := ResolvedQR{Recipient: r}
	if a := q.Get("a"); a != "" {
		pence, err := strconv.ParseInt(a, 10, 64)
		if err != nil || pence <= 0 {
			return ResolvedQR{}, bad
		}
		m := money.New(pence)
		out.Amount = &m
	}
	if n := strings.TrimSpace(q.Get("n")); n != "" {
		out.Note = &n
	}
	return out, nil
}

// RecentRecipient is a person the user recently paid or was paid by.
type RecentRecipient struct {
	Recipient
	LastPaidAt time.Time `json:"last_paid_at"`
}

// Recent returns the people the user most recently exchanged money with.
func (s *Service) Recent(ctx context.Context, q postgres.DBTX, walletID uuid.UUID, limit int) ([]RecentRecipient, error) {
	rows, err := q.Query(ctx, `
		SELECT cu.id, cu.username, cu.phone_e164, cp.first_name, cp.last_name, cp.avatar_url, cw.id, max(t.created_at)
		FROM transactions t
		JOIN wallets cw ON cw.id = CASE WHEN t.from_wallet_id = $1 THEN t.to_wallet_id ELSE t.from_wallet_id END
		JOIN users cu ON cu.id = cw.user_id
		LEFT JOIN profiles cp ON cp.user_id = cu.id
		WHERE (t.from_wallet_id = $1 OR t.to_wallet_id = $1)
		  AND t.type IN ('p2p_transfer', 'request_payment')
		  AND t.status = 'succeeded'
		  AND cu.status = 'active'
		GROUP BY cu.id, cp.user_id, cw.id
		ORDER BY max(t.created_at) DESC
		LIMIT $2`, walletID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent recipients: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RecentRecipient, error) {
		var (
			r           RecentRecipient
			id          uuid.UUID
			phone       string
			first, last *string
		)
		err := row.Scan(&id, &r.Username, &phone, &first, &last, &r.AvatarURL, &r.walletID, &r.LastPaidAt)
		r.UserID, r.PhoneMasked = id.String(), auth.MaskPhone(phone)
		r.DisplayName = user.DisplayName(user.User{Username: r.Username}, user.Profile{FirstName: first, LastName: last})
		return r, err
	})
}

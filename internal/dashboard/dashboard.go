package dashboard

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/bank"
	"github.com/sahilkarwasra/moepay/internal/history"
	"github.com/sahilkarwasra/moepay/internal/ledger"
	"github.com/sahilkarwasra/moepay/internal/money"
	"github.com/sahilkarwasra/moepay/internal/moneyrequest"
	"github.com/sahilkarwasra/moepay/internal/payment"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
	"github.com/sahilkarwasra/moepay/internal/user"
	"github.com/sahilkarwasra/moepay/internal/wallet"
)

// Handler serves screen-shaped aggregates so the app can render the home and wallet
// screens with a single request each.
type Handler struct {
	pool     *pgxpool.Pool
	users    *user.Repository
	wallets  *wallet.Service
	banks    *bank.Repository
	ledger   *ledger.Ledger
	history  *history.Reader
	payments *payment.Service
	requests *moneyrequest.Service
}

type Deps struct {
	Pool     *pgxpool.Pool
	Users    *user.Repository
	Wallets  *wallet.Service
	Banks    *bank.Repository
	Ledger   *ledger.Ledger
	History  *history.Reader
	Payments *payment.Service
	Requests *moneyrequest.Service
}

func NewHandler(d Deps) *Handler {
	return &Handler{pool: d.Pool, users: d.Users, wallets: d.Wallets, banks: d.Banks, ledger: d.Ledger,
		history: d.History, payments: d.Payments, requests: d.Requests}
}

func (h *Handler) Register(authed *gin.RouterGroup) {
	authed.GET("/home", h.home)
	authed.GET("/wallet", h.wallet)
}

// Action is a button the app shows; disabled actions carry a machine-readable reason.
type Action struct {
	ID             string  `json:"id"`
	Label          string  `json:"label"`
	Enabled        bool    `json:"enabled"`
	DisabledReason *string `json:"disabled_reason"`
}

type Me struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Username    *string `json:"username"`
	AvatarURL   *string `json:"avatar_url"`
}

type WalletSummary struct {
	ID      string           `json:"id"`
	Status  string           `json:"status"`
	Balance money.Amount     `json:"balance"`
	PIN     wallet.PINStatus `json:"pin"`
}

type Home struct {
	Me                 Me                        `json:"me"`
	WalletReady        bool                      `json:"wallet_ready"`
	Wallet             *WalletSummary            `json:"wallet"`
	QuickActions       []Action                  `json:"quick_actions"`
	RecentRecipients   []payment.RecentRecipient `json:"recent_recipients"`
	Requests           moneyrequest.Counts       `json:"requests"`
	RecentTransactions []history.View            `json:"recent_transactions"`
}

func (h *Handler) home(c *gin.Context) {
	ctx, me := c.Request.Context(), httpx.MustPrincipal(c).UserID
	out := Home{RecentRecipients: []payment.RecentRecipient{}, RecentTransactions: []history.View{}}
	var err error
	if out.Me, err = h.me(ctx, me); err != nil {
		httpx.Fail(c, err)
		return
	}
	w, err := h.wallets.ForUser(ctx, h.pool, me)
	if errors.Is(err, apperr.ErrWalletNotReady) {
		out.QuickActions = actions(nil, 0)
		httpx.OK(c, http.StatusOK, "Home", out)
		return
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out.WalletReady = true
	out.Wallet = &WalletSummary{ID: w.ID.String(), Status: w.Status, Balance: money.New(w.BalancePence), PIN: h.wallets.PINStatus(w)}

	accts, err := h.banks.List(ctx, h.pool, me)
	if err == nil {
		out.QuickActions = actions(&w, len(accts))
		out.RecentRecipients, err = h.payments.Recent(ctx, h.pool, w.ID, 5)
	}
	if err == nil {
		out.Requests, err = h.requests.Counts(ctx, h.pool, me)
	}
	if err == nil {
		var page history.Page
		page, err = h.history.List(ctx, h.pool, w.ID, history.Filter{Limit: 3})
		out.RecentTransactions = nonNil(page.Items)
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out.RecentRecipients = nonNilRecipients(out.RecentRecipients)
	httpx.OK(c, http.StatusOK, "Home", out)
}

type DailyLimit struct {
	Limit     money.Amount `json:"limit"`
	Used      money.Amount `json:"used"`
	Remaining money.Amount `json:"remaining"`
	ResetsAt  time.Time    `json:"resets_at"`
}

type Limits struct {
	MinPerTransaction money.Amount `json:"min_per_transaction"`
	MaxPerTransaction money.Amount `json:"max_per_transaction"`
	MaxBalance        money.Amount `json:"max_balance"`
	CanAdd            money.Amount `json:"can_add"`
	DailyOutgoing     DailyLimit   `json:"daily_outgoing"`
}

type WalletScreen struct {
	Wallet             WalletSummary       `json:"wallet"`
	Limits             Limits              `json:"limits"`
	Actions            []Action            `json:"actions"`
	BankAccounts       []bank.View         `json:"bank_accounts"`
	Requests           moneyrequest.Counts `json:"requests"`
	RecentTransactions []history.View      `json:"recent_transactions"`
}

func (h *Handler) wallet(c *gin.Context) {
	ctx, me := c.Request.Context(), httpx.MustPrincipal(c).UserID
	w, err := h.wallets.ForUser(ctx, h.pool, me)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	cfg := h.ledger.Limits()
	used, err := h.ledger.DailyOutgoing(ctx, h.pool, w.ID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	accts, err := h.banks.List(ctx, h.pool, me)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	counts, err := h.requests.Counts(ctx, h.pool, me)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	page, err := h.history.List(ctx, h.pool, w.ID, history.Filter{Limit: 5})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	views := make([]bank.View, len(accts))
	for i, a := range accts {
		views[i] = a.View()
	}
	httpx.OK(c, http.StatusOK, "Wallet", WalletScreen{
		Wallet: WalletSummary{ID: w.ID.String(), Status: w.Status, Balance: money.New(w.BalancePence), PIN: h.wallets.PINStatus(w)},
		Limits: Limits{
			MinPerTransaction: money.New(cfg.MinPerTxnPence),
			MaxPerTransaction: money.New(cfg.MaxPerTxnPence),
			MaxBalance:        money.New(cfg.MaxBalancePence),
			CanAdd:            money.New(max(cfg.MaxBalancePence-w.BalancePence, 0)),
			DailyOutgoing: DailyLimit{
				Limit:     money.New(cfg.DailyOutgoingPence),
				Used:      money.New(used),
				Remaining: money.New(max(cfg.DailyOutgoingPence-used, 0)),
				ResetsAt:  h.ledger.StartOfDay(time.Now()).AddDate(0, 0, 1),
			},
		},
		Actions:            actions(&w, len(accts)),
		BankAccounts:       views,
		Requests:           counts,
		RecentTransactions: nonNil(page.Items),
	})
}

func (h *Handler) me(ctx context.Context, id uuid.UUID) (Me, error) {
	u, err := h.users.GetByID(ctx, h.pool, id)
	if err != nil {
		return Me{}, err
	}
	p, err := h.users.GetProfile(ctx, h.pool, id)
	if err != nil {
		return Me{}, err
	}
	return Me{ID: u.ID.String(), DisplayName: user.DisplayName(u, p), Username: u.Username, AvatarURL: p.AvatarURL}, nil
}

// actions lists wallet actions in display order and why any are unavailable.
func actions(w *wallet.Wallet, bankAccounts int) []Action {
	reason := func(r string) *string { return &r }
	var (
		noWallet = reason("WALLET_NOT_READY")
		noPIN    = reason("PIN_NOT_SET")
		noBank   = reason("NO_BANK_ACCOUNT")
		noFunds  = reason("INSUFFICIENT_BALANCE")
		frozen   = reason("WALLET_FROZEN")
	)
	check := func(needFunds, needBank bool) *string {
		switch {
		case w == nil:
			return noWallet
		case w.Status != "active":
			return frozen
		case !w.PINSet:
			return noPIN
		case needBank && bankAccounts == 0:
			return noBank
		case needFunds && w.BalancePence == 0:
			return noFunds
		}
		return nil
	}
	mk := func(id, label string, r *string) Action {
		return Action{ID: id, Label: label, Enabled: r == nil, DisabledReason: r}
	}
	var qr *string
	if w == nil {
		qr = noWallet
	}
	return []Action{
		mk("send", "Send", check(true, false)),
		mk("request", "Request", qr),
		mk("scan_qr", "Scan & pay", check(true, false)),
		mk("add_money", "Add money", check(false, true)),
		mk("withdraw", "Withdraw", check(true, true)),
		mk("history", "History", qr),
	}
}

func nonNil(v []history.View) []history.View {
	if v == nil {
		return []history.View{}
	}
	return v
}

func nonNilRecipients(v []payment.RecentRecipient) []payment.RecentRecipient {
	if v == nil {
		return []payment.RecentRecipient{}
	}
	return v
}

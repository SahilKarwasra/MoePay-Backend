package history

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
	"github.com/sahilkarwasra/moepay/internal/wallet"
)

type Handler struct {
	pool    *pgxpool.Pool
	reader  *Reader
	wallets *wallet.Service
	loc     *time.Location
}

func NewHandler(pool *pgxpool.Pool, reader *Reader, wallets *wallet.Service, loc *time.Location) *Handler {
	return &Handler{pool: pool, reader: reader, wallets: wallets, loc: loc}
}

func (h *Handler) Register(authed *gin.RouterGroup) {
	authed.GET("/transactions", h.list)
	authed.GET("/transactions/:id", h.get)
}

func (h *Handler) list(c *gin.Context) {
	ctx := c.Request.Context()
	f, err := ParseFilter(c.Query, h.loc)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	w, err := h.wallets.ForUser(ctx, h.pool, httpx.MustPrincipal(c).UserID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	page, err := h.reader.List(ctx, h.pool, w.ID, f)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if page.Items == nil {
		page.Items = []View{}
	}
	httpx.OK(c, http.StatusOK, "Transactions", page)
}

func (h *Handler) get(c *gin.Context) {
	ctx := c.Request.Context()
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Fail(c, apperr.ErrTransactionNotFound)
		return
	}
	w, err := h.wallets.ForUser(ctx, h.pool, httpx.MustPrincipal(c).UserID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	v, err := h.reader.Get(ctx, h.pool, w.ID, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Transaction", v)
}

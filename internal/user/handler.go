package user

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
)

type Handler struct {
	pool *pgxpool.Pool
	repo *Repository
}

func NewHandler(pool *pgxpool.Pool, repo *Repository) *Handler {
	return &Handler{pool: pool, repo: repo}
}

func (h *Handler) Register(authed *gin.RouterGroup) {
	authed.GET("/users/me", h.me)
}

func (h *Handler) me(c *gin.Context) {
	ctx, id := c.Request.Context(), httpx.MustPrincipal(c).UserID
	u, err := h.repo.GetByID(ctx, h.pool, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	p, err := h.repo.GetProfile(ctx, h.pool, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Current user", NewView(u, p))
}

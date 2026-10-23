package bank

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(authed *gin.RouterGroup) {
	g := authed.Group("/bank-accounts")
	g.GET("", h.list)
	g.POST("", h.add)
	g.DELETE("/:id", h.remove)
	g.PUT("/:id/primary", h.setPrimary)
	authed.GET("/banks/sort-code/:code", h.lookup)
}

func (h *Handler) list(c *gin.Context) {
	accts, err := h.svc.List(c.Request.Context(), httpx.MustPrincipal(c).UserID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Bank accounts", gin.H{"bank_accounts": accts})
}

func (h *Handler) add(c *gin.Context) {
	var in AddInput
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	acct, err := h.svc.Add(c.Request.Context(), httpx.MustPrincipal(c).UserID, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusCreated, "Bank account linked", acct)
}

func (h *Handler) remove(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.svc.Remove(c.Request.Context(), httpx.MustPrincipal(c).UserID, id); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Bank account removed", nil)
}

func (h *Handler) setPrimary(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	acct, err := h.svc.SetPrimary(c.Request.Context(), httpx.MustPrincipal(c).UserID, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Primary bank account updated", acct)
}

func (h *Handler) lookup(c *gin.Context) {
	info, err := h.svc.LookupSortCode(c.Param("code"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, "Bank details", info)
}

func pathID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Fail(c, apperr.ErrBankAccountNotFound)
		return uuid.Nil, false
	}
	return id, true
}

package app

import (
	"net/http"

	"github.com/sahilkarwasra/moepay/internal/apperr"
)

var (
	notFound         = apperr.New(http.StatusNotFound, "ROUTE_NOT_FOUND", "Route not found")
	methodNotAllowed = apperr.New(http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
)

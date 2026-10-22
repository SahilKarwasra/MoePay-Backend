package httpx

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/platform/logging"
)

// Response keeps the original MoePay envelope ({status_code, success, message, data})
// and adds a machine-readable error object for failures.
type Response struct {
	StatusCode int        `json:"status_code"`
	Success    bool       `json:"success"`
	Message    string     `json:"message"`
	Data       any        `json:"data"`
	Error      *ErrorBody `json:"error,omitempty"`
}

type ErrorBody struct {
	Code    string         `json:"code"`
	Details map[string]any `json:"details,omitempty"`
}

func OK(c *gin.Context, status int, message string, data any) {
	if data == nil {
		data = gin.H{}
	}
	c.JSON(status, Response{StatusCode: status, Success: true, Message: message, Data: data})
}

// Fail renders err. Unknown errors are logged with the request logger and rendered as a
// generic 500 so internal details never reach the client.
func Fail(c *gin.Context, err error) {
	ae, ok := apperr.As(err)
	if !ok {
		logging.FromContext(c.Request.Context()).Error("unhandled error", "err", err)
		ae = apperr.ErrInternal
	} else if ae.Status >= 500 {
		logging.FromContext(c.Request.Context()).Error("server error", "err", err)
	}
	c.AbortWithStatusJSON(ae.Status, Response{
		StatusCode: ae.Status,
		Success:    false,
		Message:    ae.Message,
		Data:       gin.H{},
		Error:      &ErrorBody{Code: ae.Code, Details: ae.Details},
	})
}

// BindJSON decodes the body into dst, rejecting unknown fields and malformed JSON.
func BindJSON(c *gin.Context, dst any) error {
	if c.Request.Body == nil {
		return apperr.ErrInvalidRequest
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	dec := newStrictDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return apperr.ErrInvalidRequest
		}
		return apperr.ErrInvalidRequest.WithDetails(map[string]any{"reason": err.Error()})
	}
	return nil
}

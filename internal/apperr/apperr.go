package apperr

import (
	"errors"
	"net/http"
)

type Error struct {
	Code    string
	Message string
	Status  int
	// Details is optional structured, client-safe context (e.g. field errors, retry_after).
	Details map[string]any
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return e.Code + ": " + e.Message + ": " + e.cause.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.cause }

// Is matches on Code so sentinel errors work with errors.Is even after WithDetails.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func New(status int, code, message string) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

// WithDetails returns a copy of e carrying details.
func (e *Error) WithDetails(details map[string]any) *Error {
	cp := *e
	cp.Details = details
	return &cp
}

// Wrap returns a copy of e that wraps cause (kept for logs, not shown to clients).
func (e *Error) Wrap(cause error) *Error {
	cp := *e
	cp.cause = cause
	return &cp
}

// As extracts an *Error from err.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

func Validation(message string, fields map[string]string) *Error {
	e := New(http.StatusBadRequest, "VALIDATION_FAILED", message)
	if len(fields) > 0 {
		details := make(map[string]any, len(fields))
		for k, v := range fields {
			details[k] = v
		}
		e.Details = map[string]any{"fields": details}
	}
	return e
}

var (
	ErrInvalidRequest       = New(http.StatusBadRequest, "INVALID_REQUEST", "Invalid request body")
	ErrUnauthorized         = New(http.StatusUnauthorized, "UNAUTHORIZED", "Authentication required")
	ErrForbidden            = New(http.StatusForbidden, "FORBIDDEN", "You do not have access to this resource")
	ErrNotFound             = New(http.StatusNotFound, "NOT_FOUND", "Resource not found")
	ErrRateLimited          = New(http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests, please try again later")
	ErrInternal             = New(http.StatusInternalServerError, "INTERNAL_ERROR", "Something went wrong")
	ErrUnavailable          = New(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service temporarily unavailable")
	ErrInvalidPhone         = New(http.StatusBadRequest, "INVALID_PHONE_NUMBER", "Phone number is not valid")
	ErrOTPInvalid           = New(http.StatusUnauthorized, "OTP_INVALID", "The OTP is incorrect")
	ErrOTPExpired           = New(http.StatusUnauthorized, "OTP_EXPIRED", "The OTP has expired or was not requested; request a new one")
	ErrOTPExhausted         = New(http.StatusUnauthorized, "OTP_ATTEMPTS_EXCEEDED", "Too many incorrect attempts; request a new OTP")
	ErrOTPCooldown          = New(http.StatusTooManyRequests, "OTP_RESEND_COOLDOWN", "Please wait before requesting another OTP")
	ErrTokenInvalid         = New(http.StatusUnauthorized, "TOKEN_INVALID", "Invalid or expired token")
	ErrSessionRevoked       = New(http.StatusUnauthorized, "SESSION_REVOKED", "Session has been signed out")
	ErrUserInactive         = New(http.StatusForbidden, "USER_INACTIVE", "This account is not active")
	ErrUsernameTaken        = New(http.StatusConflict, "USERNAME_TAKEN", "Username is already taken")
	ErrUsernameInvalid      = New(http.StatusBadRequest, "USERNAME_INVALID", "Username is not valid")
	ErrStepOutOfOrder       = New(http.StatusConflict, "ONBOARDING_STEP_OUT_OF_ORDER", "Complete the previous onboarding steps first")
	ErrOnboardingDone       = New(http.StatusConflict, "ONBOARDING_ALREADY_COMPLETED", "Onboarding is already completed")
	ErrOnboardingIncomplete = New(http.StatusConflict, "ONBOARDING_INCOMPLETE", "Required onboarding steps are not completed")
)

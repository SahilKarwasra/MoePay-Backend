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

	ErrWalletNotReady       = New(http.StatusConflict, "WALLET_NOT_READY", "Complete onboarding to activate your wallet")
	ErrWalletFrozen         = New(http.StatusForbidden, "WALLET_FROZEN", "This wallet is frozen")
	ErrInsufficientBalance  = New(http.StatusUnprocessableEntity, "INSUFFICIENT_BALANCE", "Not enough money in your wallet")
	ErrAmountInvalid        = New(http.StatusBadRequest, "AMOUNT_INVALID", "Amount is not valid")
	ErrLimitExceeded        = New(http.StatusUnprocessableEntity, "LIMIT_EXCEEDED", "This payment exceeds your limits")
	ErrWalletBalanceLimit   = New(http.StatusUnprocessableEntity, "WALLET_BALANCE_LIMIT", "This would take the wallet over its maximum balance")
	ErrRecipientCannotTake  = New(http.StatusUnprocessableEntity, "RECIPIENT_CANNOT_RECEIVE", "The recipient cannot receive this amount right now")
	ErrPINNotSet            = New(http.StatusConflict, "PIN_NOT_SET", "Set your wallet PIN first")
	ErrPINAlreadySet        = New(http.StatusConflict, "PIN_ALREADY_SET", "A wallet PIN is already set; change it instead")
	ErrPINInvalid           = New(http.StatusUnauthorized, "PIN_INVALID", "The PIN is incorrect")
	ErrPINLocked            = New(http.StatusLocked, "PIN_LOCKED", "Too many incorrect PIN attempts; try again later or reset your PIN")
	ErrPINFormat            = New(http.StatusBadRequest, "PIN_FORMAT", "PIN must be exactly 6 digits")
	ErrPINTooWeak           = New(http.StatusBadRequest, "PIN_TOO_WEAK", "Choose a less predictable PIN")
	ErrRecipientNotFound    = New(http.StatusNotFound, "RECIPIENT_NOT_FOUND", "No MoePay user found")
	ErrCannotPaySelf        = New(http.StatusBadRequest, "CANNOT_PAY_SELF", "You cannot send money to yourself")
	ErrBankAccountNotFound  = New(http.StatusNotFound, "BANK_ACCOUNT_NOT_FOUND", "Bank account not found")
	ErrBankAccountExists    = New(http.StatusConflict, "BANK_ACCOUNT_EXISTS", "This bank account is already linked")
	ErrBankAccountLimit     = New(http.StatusConflict, "BANK_ACCOUNT_LIMIT", "You have linked the maximum number of bank accounts")
	ErrBankDeclined         = New(http.StatusPaymentRequired, "BANK_DECLINED", "The bank declined this transfer")
	ErrIdempotencyRequired  = New(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required")
	ErrIdempotencyReused    = New(http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED", "This Idempotency-Key was already used for a different request")
	ErrIdempotencyInFlight  = New(http.StatusConflict, "IDEMPOTENCY_KEY_IN_USE", "A request with this Idempotency-Key is still being processed")
	ErrRequestNotPending    = New(http.StatusConflict, "REQUEST_NOT_PENDING", "This money request is no longer pending")
	ErrTransactionNotFound  = New(http.StatusNotFound, "TRANSACTION_NOT_FOUND", "Transaction not found")
	ErrMoneyRequestNotFound = New(http.StatusNotFound, "MONEY_REQUEST_NOT_FOUND", "Money request not found")
)

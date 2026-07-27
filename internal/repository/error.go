package repository

import "errors"

var (
	ErrInvalidPhoneNumber    = errors.New("invalid phone number")
	ErrUserNotFound          = errors.New("user not found")
	ErrUserAlreadyExists     = errors.New("user already exists")
	ErrOtpNotFound           = errors.New("otp not found")
	ErrInvalidOtp            = errors.New("invalid otp")
	ErrOtpUsed               = errors.New("otp already used")
	ErrOtpExpired            = errors.New("otp expired")
	ErrWalletIdAlreadyExists = errors.New("wallet id already in use by another user")
	ErrInvalidWalletID       = errors.New("invalid wallet id format")
	ErrInvalidUserName       = errors.New("invalid user name format")
	ErrInvalidUserID         = errors.New("invalid user id format")
)

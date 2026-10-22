package auth

import (
	"context"
	"fmt"
	"log/slog"
)

// SMSSender delivers OTP codes. A real provider (e.g. MSG91, Twilio) is a product and
// credentials decision; implement this interface and select it with SMS_PROVIDER.
type SMSSender interface {
	SendOTP(ctx context.Context, phoneE164, code string) error
}

// LogSender writes the OTP to the log. Config validation forbids it in production.
type LogSender struct{ Logger *slog.Logger }

func (s LogSender) SendOTP(ctx context.Context, phoneE164, code string) error {
	s.Logger.WarnContext(ctx, "DEV ONLY: OTP not sent via SMS", "phone", phoneE164, "otp", code)
	return nil
}

func NewSMSSender(provider string, logger *slog.Logger) (SMSSender, error) {
	switch provider {
	case "log":
		return LogSender{Logger: logger}, nil
	default:
		return nil, fmt.Errorf("SMS_PROVIDER %q is not implemented", provider)
	}
}

package auth

import (
	"strings"

	"github.com/nyaruka/phonenumbers"

	"github.com/sahilkarwasra/moepay/internal/apperr"
)

// NormalizePhone parses raw into E.164 (e.g. +919876543210). Numbers without a
// leading + are interpreted in defaultRegion (ISO 3166 alpha-2, e.g. "IN").
func NormalizePhone(raw, defaultRegion string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 32 {
		return "", apperr.ErrInvalidPhone
	}
	num, err := phonenumbers.Parse(raw, defaultRegion)
	if err != nil || !phonenumbers.IsValidNumber(num) {
		return "", apperr.ErrInvalidPhone
	}
	return phonenumbers.Format(num, phonenumbers.E164), nil
}

// MaskPhone returns a log-safe representation such as +91******3210.
func MaskPhone(e164 string) string {
	if len(e164) <= 7 {
		return "***"
	}
	return e164[:3] + strings.Repeat("*", len(e164)-7) + e164[len(e164)-4:]
}

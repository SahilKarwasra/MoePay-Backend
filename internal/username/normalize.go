package username

import (
	"regexp"
	"strings"

	"github.com/sahilkarwasra/moepay/internal/apperr"
)

const (
	MinLength = 3
	MaxLength = 30
)

// Must match users_username_normalized_format in migrations/00001.
var pattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{2,29}$`)

// reserved names cannot be claimed. This list is a product decision; extend as needed.
var reserved = map[string]struct{}{
	"admin": {}, "administrator": {}, "root": {}, "support": {}, "help": {}, "moepay": {},
	"moe": {}, "official": {}, "system": {}, "security": {}, "staff": {}, "api": {},
	"null": {}, "undefined": {}, "me": {}, "settings": {}, "wallet": {}, "payments": {},
}

// Normalize trims and lowercases s and validates it. It returns the display form
// (trimmed, original case) and the normalized identity used for uniqueness.
// Only ASCII letters, digits, '_' and '.' are allowed so there are no Unicode
// confusables to fold.
func Normalize(s string) (display, normalized string, err error) {
	display = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@"))
	normalized = strings.ToLower(display)
	if !pattern.MatchString(normalized) ||
		strings.Contains(normalized, "..") ||
		strings.HasSuffix(normalized, ".") {
		return "", "", apperr.ErrUsernameInvalid.WithDetails(map[string]any{
			"rules": "3-30 characters; start with a letter; letters, digits, '_' and '.' only; no consecutive or trailing '.'",
		})
	}
	return display, normalized, nil
}

func IsReserved(normalized string) bool {
	_, ok := reserved[normalized]
	return ok
}

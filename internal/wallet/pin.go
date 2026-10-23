package wallet

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/sahilkarwasra/moepay/internal/apperr"
)

// argon2id parameters (OWASP minimum recommendation: 19 MiB, 2 iterations, 1 lane).
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	pinLength      = 6
)

var weakPINs = map[string]bool{
	"123123": true, "121212": true, "112233": true, "123321": true, "101010": true,
	"696969": true, "200000": true, "100000": true, "102030": true,
}

// ValidatePIN enforces the 6-digit format and rejects easily guessed PINs.
func ValidatePIN(pin string) error {
	if len(pin) != pinLength {
		return apperr.ErrPINFormat
	}
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return apperr.ErrPINFormat
		}
	}
	if weakPINs[pin] || strings.Count(pin, pin[:1]) == len(pin) || isSequence(pin, 1) || isSequence(pin, -1) {
		return apperr.ErrPINTooWeak
	}
	return nil
}

func isSequence(pin string, step int) bool {
	for i := 1; i < len(pin); i++ {
		if (int(pin[i])-int(pin[i-1])+10)%10 != (step+10)%10 {
			return false
		}
	}
	return true
}

// hashPIN returns a PHC-formatted argon2id hash.
func hashPIN(pin string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("pin salt: %w", err)
	}
	key := argon2.IDKey([]byte(pin), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func verifyPIN(pin, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("unsupported pin hash format")
	}
	var version int
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("pin hash version: %w", err)
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false, fmt.Errorf("pin hash params: %w", err)
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("pin hash salt: %w", err)
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("pin hash key: %w", err)
	}
	got := argon2.IDKey([]byte(pin), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

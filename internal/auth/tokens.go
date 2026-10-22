package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sahilkarwasra/moepay/internal/apperr"
)

// AccessClaims are the claims of a short-lived access token. The session id lets the
// server revoke access tokens on logout before they expire.
type AccessClaims struct {
	SessionID string `json:"sid"`
	TokenType string `json:"typ"`
	jwt.RegisteredClaims
}

const accessTokenType = "access"

type TokenIssuer struct {
	secret []byte
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

func NewTokenIssuer(secret []byte, issuer string, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, issuer: issuer, ttl: ttl, now: time.Now}
}

func (t *TokenIssuer) TTL() time.Duration { return t.ttl }

func (t *TokenIssuer) Issue(userID, sessionID uuid.UUID) (string, time.Time, error) {
	now := t.now()
	exp := now.Add(t.ttl)
	claims := AccessClaims{
		SessionID: sessionID.String(),
		TokenType: accessTokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    t.issuer,
			Subject:   userID.String(),
			Audience:  jwt.ClaimStrings{t.issuer},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, exp, nil
}

// Parse validates signature, algorithm, issuer, audience, expiry and token type.
func (t *TokenIssuer) Parse(raw string) (userID, sessionID uuid.UUID, exp time.Time, err error) {
	var claims AccessClaims
	_, err = jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithAudience(t.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil || claims.TokenType != accessTokenType {
		return uuid.Nil, uuid.Nil, time.Time{}, apperr.ErrTokenInvalid
	}
	if userID, err = uuid.Parse(claims.Subject); err != nil {
		return uuid.Nil, uuid.Nil, time.Time{}, apperr.ErrTokenInvalid
	}
	if sessionID, err = uuid.Parse(claims.SessionID); err != nil {
		return uuid.Nil, uuid.Nil, time.Time{}, apperr.ErrTokenInvalid
	}
	return userID, sessionID, claims.ExpiresAt.Time, nil
}

// NewRefreshToken returns an opaque 256-bit token and its SHA-256 hash for storage.
func NewRefreshToken() (token string, hash []byte, err error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", nil, fmt.Errorf("generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf[:])
	return token, HashRefreshToken(token), nil
}

func HashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// OTPCodec generates and verifies OTP codes. Codes are stored as an HMAC keyed by a
// server secret and bound to the phone number, so a database leak does not reveal
// codes and a 6-digit code cannot be brute-forced offline without the key.
type OTPCodec struct {
	secret []byte
	length int
}

func NewOTPCodec(secret []byte, length int) *OTPCodec {
	return &OTPCodec{secret: secret, length: length}
}

func (o *OTPCodec) Generate() (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(o.length)), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", errors.New("crypto/rand unavailable")
	}
	return fmt.Sprintf("%0*d", o.length, n), nil
}

func (o *OTPCodec) Hash(phoneE164, code string) []byte {
	mac := hmac.New(sha256.New, o.secret)
	mac.Write([]byte(phoneE164))
	mac.Write([]byte{0})
	mac.Write([]byte(code))
	return mac.Sum(nil)
}

func (o *OTPCodec) Verify(phoneE164, code string, hash []byte) bool {
	return subtle.ConstantTimeCompare(o.Hash(phoneE164, code), hash) == 1
}

func (o *OTPCodec) Length() int { return o.length }

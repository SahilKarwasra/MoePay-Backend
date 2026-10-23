package bank

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sahilkarwasra/moepay/internal/apperr"
)

type Account struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	HolderName string
	BankName   string
	SortCode   string
	Last4      string
	IsPrimary  bool
	CreatedAt  time.Time
}

// View is the client representation; the full account number is never returned.
type View struct {
	ID                  string    `json:"id"`
	HolderName          string    `json:"holder_name"`
	BankName            string    `json:"bank_name"`
	SortCode            string    `json:"sort_code"`
	AccountNumberMasked string    `json:"account_number_masked"`
	Last4               string    `json:"last4"`
	IsPrimary           bool      `json:"is_primary"`
	CreatedAt           time.Time `json:"created_at"`
}

func (a Account) View() View {
	return View{
		ID: a.ID.String(), HolderName: a.HolderName, BankName: a.BankName,
		SortCode: FormatSortCode(a.SortCode), AccountNumberMasked: "••••" + a.Last4, Last4: a.Last4,
		IsPrimary: a.IsPrimary, CreatedAt: a.CreatedAt,
	}
}

// Label is a short description such as "MoeBank ••1234".
func (a Account) Label() string { return a.BankName + " ••" + a.Last4 }

// NormalizeSortCode accepts "04-00-04", "04 00 04" or "040004".
func NormalizeSortCode(raw string) (string, bool) {
	s := strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(raw))
	return s, len(s) == 6 && isDigits(s)
}

func FormatSortCode(s string) string {
	if len(s) != 6 {
		return s
	}
	return s[0:2] + "-" + s[2:4] + "-" + s[4:6]
}

func normalizeAccountNumber(raw string) (string, bool) {
	s := strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(raw))
	return s, len(s) == 8 && isDigits(s)
}

func cleanHolderName(raw string) (string, bool) {
	s := strings.Join(strings.Fields(raw), " ")
	if s == "" || utf8.RuneCountInString(s) > 70 {
		return "", false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsMark(r) && !strings.ContainsRune(" .-'&", r) {
			return "", false
		}
	}
	return s, true
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func accountHash(secret []byte, sortCode, number string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sortCode + ":" + number))
	return mac.Sum(nil)
}

// Directory resolves a sort code to a (fictional) bank. Unknown prefixes map to a
// generic demo bank, so any well-formed sort code can be linked.
var directory = map[string]string{
	"04": "MoeBank",
	"10": "Sterling Demo Bank",
	"20": "Northwind Bank",
	"30": "Crown Test Bank",
	"40": "Thames Savings",
	"60": "Albion Trust",
	"77": "Highland Mutual",
}

func BankName(sortCode string) string {
	if len(sortCode) >= 2 {
		if name, ok := directory[sortCode[:2]]; ok {
			return name
		}
	}
	return "Demo Bank"
}

// Gateway moves money between MoePay and a user's bank account.
type Gateway interface {
	// PullFunds debits the bank account (add money to wallet).
	PullFunds(ctx context.Context, acct Account, amountPence int64, reference string) error
	// PushFunds credits the bank account (withdraw from wallet).
	PushFunds(ctx context.Context, acct Account, amountPence int64, reference string) error
}

// DeclineError is returned when the bank refuses a transfer.
type DeclineError struct {
	Code   string
	Reason string
}

func (e *DeclineError) Error() string { return e.Code + ": " + e.Reason }

// DummyGateway settles instantly. Failures are deterministic so they can be demoed:
// accounts ending in 0000 have insufficient funds to pull from, and accounts ending
// in 9999 belong to a bank that is always unavailable.
type DummyGateway struct{}

func (DummyGateway) PullFunds(_ context.Context, acct Account, _ int64, _ string) error {
	switch acct.Last4 {
	case "0000":
		return &DeclineError{Code: "insufficient_funds", Reason: "Your bank account has insufficient funds"}
	case "9999":
		return &DeclineError{Code: "bank_unavailable", Reason: "Your bank is not responding; try again later"}
	}
	return nil
}

func (DummyGateway) PushFunds(_ context.Context, acct Account, _ int64, _ string) error {
	if acct.Last4 == "9999" {
		return &DeclineError{Code: "bank_unavailable", Reason: "Your bank is not responding; try again later"}
	}
	return nil
}

func validationErr(fields map[string]string) error {
	return apperr.Validation("Bank account details are not valid", fields)
}

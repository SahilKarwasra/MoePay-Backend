package money

import (
	"strconv"
	"strings"
)

// Currency is the only currency MoePay supports. Amounts are integer pence.
const Currency = "GBP"

// Amount is the client representation of a sum of money.
type Amount struct {
	Pence    int64  `json:"amount_pence"`
	Currency string `json:"currency"`
	Display  string `json:"display"`
}

func New(pence int64) Amount {
	return Amount{Pence: pence, Currency: Currency, Display: Format(pence)}
}

// Format renders pence as pounds, e.g. 123456 -> "£1,234.56".
func Format(pence int64) string {
	sign := ""
	if pence < 0 {
		sign, pence = "-", -pence
	}
	pounds := strconv.FormatInt(pence/100, 10)
	var b strings.Builder
	for i, r := range pounds {
		if i > 0 && (len(pounds)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	frac := pence % 100
	return sign + "£" + b.String() + "." + string(rune('0'+frac/10)) + string(rune('0'+frac%10))
}

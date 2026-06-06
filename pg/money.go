package pg

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money is a precision-safe monetary amount represented as a signed
// 64-bit integer in minor units (cents for USD, eurocents for EUR,
// etc.). The default exponent is 2 decimal places — override at
// declaration time when the currency uses something else (JPY=0,
// BTC=8).
//
//	type Payment struct {
//	    ID     int64    `drop:"id,primaryKey,autoIncrement"`
//	    Amount pg.Money `drop:"amount"`
//	}
//
//	p := Payment{Amount: pg.MoneyFromString("12.34")}
//	p.Amount = p.Amount.Add(pg.MoneyFromCents(50))
//	p.Amount = p.Amount.MulRate(0.10) // apply 10% (banker's rounding)
//
// Floats in monetary code are bugs waiting to ship — Money never
// converts to float for storage or comparison; only MulRate uses
// a floating-point factor and rounds half-to-even at the end. For
// chains of percentage operations, use Decimal in a future commit.
//
// Wire / storage: bigint (cents). JSON: string in canonical
// "<sign>?<integer>.<fraction>" form so JavaScript clients don't
// lose precision on values > 2^53.
//
// Equality, ordering and zero-checks compare cents directly, so
// two Money values with the same cents but different exponents
// are NOT equal — wrap mixed-exponent stores carefully.
type Money struct {
	cents    int64
	exponent uint8 // number of decimal places; default 2 if zero
}

// MoneyFromCents returns a Money expressed as a count of minor
// units at the default 2-place exponent.
func MoneyFromCents(cents int64) Money { return Money{cents: cents, exponent: 2} }

// MoneyFromUnits returns a Money for the supplied whole-number
// amount (units) plus a minor-unit remainder (cents). 12.34 ->
// MoneyFromUnits(12, 34).
func MoneyFromUnits(units, cents int64) Money {
	exp := uint8(2)
	scale := pow10(exp)
	return Money{cents: units*scale + cents, exponent: exp}
}

// MoneyWithExponent returns a Money with explicit precision. Use
// for currencies whose minor unit isn't 1/100 (JPY uses 0, BTC
// often uses 8).
func MoneyWithExponent(units int64, exponent uint8) Money {
	if exponent > 9 {
		exponent = 9
	}
	return Money{cents: units, exponent: exponent}
}

// MoneyFromString parses "12.34", "-1.5", "10" into Money at the
// default 2-place exponent. Truncates extra fractional digits;
// callers needing different rounding should pre-round.
func MoneyFromString(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, errors.New("drops/pg: MoneyFromString empty")
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	} else if s[0] == '+' {
		s = s[1:]
	}
	const exp = uint8(2)
	scale := pow10(exp)
	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("drops/pg: MoneyFromString integer: %w", err)
	}
	var frac int64
	if hasFrac && fracPart != "" {
		// Pad / truncate to `exp` digits.
		if len(fracPart) > int(exp) {
			fracPart = fracPart[:exp]
		} else if len(fracPart) < int(exp) {
			fracPart = fracPart + strings.Repeat("0", int(exp)-len(fracPart))
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return Money{}, fmt.Errorf("drops/pg: MoneyFromString fraction: %w", err)
		}
	}
	cents := whole*scale + frac
	if neg {
		cents = -cents
	}
	return Money{cents: cents, exponent: exp}, nil
}

// Cents returns the underlying minor-unit count.
func (m Money) Cents() int64 { return m.cents }

// Exponent returns the number of decimal places.
func (m Money) Exponent() uint8 {
	if m.exponent == 0 {
		return 2
	}
	return m.exponent
}

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.cents == 0 }

// IsNegative reports whether the amount is < 0.
func (m Money) IsNegative() bool { return m.cents < 0 }

// Add returns m + other. Panics on exponent mismatch — the
// caller should normalise first.
func (m Money) Add(other Money) Money {
	m.assertSameExponent(other)
	return Money{cents: m.cents + other.cents, exponent: m.Exponent()}
}

// Sub returns m - other.
func (m Money) Sub(other Money) Money {
	m.assertSameExponent(other)
	return Money{cents: m.cents - other.cents, exponent: m.Exponent()}
}

// Neg returns -m.
func (m Money) Neg() Money { return Money{cents: -m.cents, exponent: m.Exponent()} }

// MulInt scales m by n (e.g. quantity × unit price).
func (m Money) MulInt(n int64) Money {
	return Money{cents: m.cents * n, exponent: m.Exponent()}
}

// MulRate scales m by a floating-point rate (e.g. tax 0.07). The
// result is rounded half-to-even (banker's rounding) to keep
// rounding bias out of long sums. Use for tax / fee / interest
// calculations where the rate is genuinely a fraction.
//
// Float precision means MulRate is appropriate for rates with at
// most ~12 significant digits; for sub-cent precision over many
// operations, use a dedicated decimal library and round once at
// the end.
func (m Money) MulRate(rate float64) Money {
	return Money{cents: roundHalfEven(float64(m.cents) * rate), exponent: m.Exponent()}
}

// Compare returns -1, 0, +1 if m is less than, equal to, greater
// than other.
func (m Money) Compare(other Money) int {
	m.assertSameExponent(other)
	switch {
	case m.cents < other.cents:
		return -1
	case m.cents > other.cents:
		return 1
	default:
		return 0
	}
}

// String renders m as "<integer>.<fraction>".
func (m Money) String() string {
	exp := m.Exponent()
	scale := pow10(exp)
	abs := m.cents
	sign := ""
	if abs < 0 {
		sign = "-"
		abs = -abs
	}
	whole := abs / scale
	frac := abs % scale
	if exp == 0 {
		return sign + strconv.FormatInt(whole, 10)
	}
	fracStr := strconv.FormatInt(frac, 10)
	if len(fracStr) < int(exp) {
		fracStr = strings.Repeat("0", int(exp)-len(fracStr)) + fracStr
	}
	return sign + strconv.FormatInt(whole, 10) + "." + fracStr
}

// MarshalJSON renders as a string to preserve precision on JS
// clients (numbers > 2^53 lose precision in JSON.parse).
func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(`"` + m.String() + `"`), nil
}

// UnmarshalJSON accepts string ("12.34"), number (123) or null.
func (m *Money) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		parsed, err := MoneyFromString(s)
		if err != nil {
			return err
		}
		*m = parsed
		return nil
	}
	// Numeric form — treat as minor units.
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*m = MoneyFromCents(n)
	return nil
}

// Value implements driver.Valuer — stores as int64 (minor units).
func (m Money) Value() (driver.Value, error) { return m.cents, nil }

// Scan implements sql.Scanner.
func (m *Money) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*m = Money{}
	case int64:
		*m = Money{cents: v, exponent: 2}
	case int:
		*m = Money{cents: int64(v), exponent: 2}
	case float64:
		// Some drivers surface numerics as float64. Round here to
		// avoid the inherent float drift.
		*m = Money{cents: roundHalfEven(v), exponent: 2}
	case []byte:
		parsed, err := MoneyFromString(string(v))
		if err != nil {
			return err
		}
		*m = parsed
	case string:
		parsed, err := MoneyFromString(v)
		if err != nil {
			return err
		}
		*m = parsed
	default:
		return fmt.Errorf("drops/pg: Money.Scan unsupported src %T", src)
	}
	return nil
}

// assertSameExponent panics on exponent mismatch — silently
// adding 12.345 (3 places) to 12.34 (2 places) would corrupt
// totals. The zero exponent defaults to 2, so unset values
// agree with the standard 2-place form.
func (m Money) assertSameExponent(other Money) {
	if m.Exponent() != other.Exponent() {
		panic(fmt.Sprintf("drops/pg: Money exponent mismatch (%d vs %d) — normalise first",
			m.Exponent(), other.Exponent()))
	}
}

// pow10 returns 10^n as int64. Capped at 1e9 because larger
// exponents overflow / aren't meaningful for currencies.
func pow10(n uint8) int64 {
	switch n {
	case 0:
		return 1
	case 1:
		return 10
	case 2:
		return 100
	case 3:
		return 1000
	case 4:
		return 10000
	case 5:
		return 100000
	case 6:
		return 1000000
	case 7:
		return 10000000
	case 8:
		return 100000000
	case 9:
		return 1000000000
	}
	return 100
}

// roundHalfEven implements banker's rounding so a stream of
// rate-multiplications doesn't accumulate bias.
func roundHalfEven(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	r := math.RoundToEven(v)
	if r > math.MaxInt64 {
		return math.MaxInt64
	}
	if r < math.MinInt64 {
		return math.MinInt64
	}
	return int64(r)
}

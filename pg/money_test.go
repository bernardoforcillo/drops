package pg_test

import (
	"encoding/json"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestMoneyFromStringRoundTrips(t *testing.T) {
	cases := []struct {
		in    string
		cents int64
		out   string
	}{
		{"12.34", 1234, "12.34"},
		{"0.99", 99, "0.99"},
		{"-1.50", -150, "-1.50"},
		{"100", 10000, "100.00"},
		{"+0.05", 5, "0.05"},
		{"7.5", 750, "7.50"},   // pad
		{"7.567", 756, "7.56"}, // truncate extra digit
	}
	for _, tc := range cases {
		m, err := pg.MoneyFromString(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if m.Cents() != tc.cents {
			t.Errorf("%q: cents got %d, want %d", tc.in, m.Cents(), tc.cents)
		}
		if m.String() != tc.out {
			t.Errorf("%q: string got %q, want %q", tc.in, m.String(), tc.out)
		}
	}
}

func TestMoneyArithmeticIsExact(t *testing.T) {
	a := pg.MoneyFromCents(1234)
	b := pg.MoneyFromCents(50)
	if got := a.Add(b); got.Cents() != 1284 {
		t.Errorf("Add: %d", got.Cents())
	}
	if got := a.Sub(b); got.Cents() != 1184 {
		t.Errorf("Sub: %d", got.Cents())
	}
	if got := a.Neg(); got.Cents() != -1234 {
		t.Errorf("Neg: %d", got.Cents())
	}
	if got := a.MulInt(3); got.Cents() != 3702 {
		t.Errorf("MulInt: %d", got.Cents())
	}
}

func TestMoneyMulRateBankersRounding(t *testing.T) {
	// 0.05 (5 cents) rounded half-to-even should give 0 (5 → even neighbour 0).
	// Test a few known half-even cases.
	cases := []struct {
		cents int64
		rate  float64
		want  int64
	}{
		{1000, 0.10, 100},  // 100.0 exact
		{1234, 0.5, 617},   // 617.0 exact
		{125, 0.5, 62},     // 62.5 → 62 (even)
		{375, 0.5, 188},    // 187.5 → 188 (even)
		{1000, 1.0, 1000},  // identity
	}
	for _, tc := range cases {
		got := pg.MoneyFromCents(tc.cents).MulRate(tc.rate)
		if got.Cents() != tc.want {
			t.Errorf("MulRate(%d, %v) = %d, want %d", tc.cents, tc.rate, got.Cents(), tc.want)
		}
	}
}

func TestMoneyCompare(t *testing.T) {
	a := pg.MoneyFromCents(100)
	b := pg.MoneyFromCents(200)
	if a.Compare(b) >= 0 {
		t.Errorf("a should be less than b")
	}
	if b.Compare(a) <= 0 {
		t.Errorf("b should be greater than a")
	}
	if a.Compare(a) != 0 {
		t.Errorf("equal compare should be 0")
	}
}

func TestMoneyJSONRoundTrip(t *testing.T) {
	m := pg.MoneyFromCents(1234)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"12.34"` {
		t.Errorf("JSON encoding: %s", b)
	}
	var back pg.Money
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Cents() != m.Cents() {
		t.Errorf("round-trip: %d != %d", back.Cents(), m.Cents())
	}
}

func TestMoneyValueAndScan(t *testing.T) {
	m := pg.MoneyFromCents(7890)
	v, err := m.Value()
	if err != nil {
		t.Fatal(err)
	}
	if v != int64(7890) {
		t.Errorf("Value: %v", v)
	}
	var back pg.Money
	if err := back.Scan(int64(7890)); err != nil {
		t.Fatal(err)
	}
	if back.Cents() != m.Cents() {
		t.Errorf("Scan: %d", back.Cents())
	}
	// String src.
	if err := back.Scan("123.45"); err != nil {
		t.Fatal(err)
	}
	if back.Cents() != 12345 {
		t.Errorf("Scan string: %d", back.Cents())
	}
}

func TestMoneyAutoTableMapsToBigint(t *testing.T) {
	type payment struct {
		ID     int64    `drop:"id,primaryKey,autoIncrement"`
		Amount pg.Money `drop:"amount,notNull"`
	}
	tbl := pg.AutoTable[payment]("payments")
	col := tbl.Col("amount")
	if col == nil {
		t.Fatal("amount column missing")
	}
	if col.Type().TypeSQL() != "bigint" {
		t.Errorf("Money should map to bigint, got %q", col.Type().TypeSQL())
	}
}

func TestMoneyAddPanicsOnExponentMismatch(t *testing.T) {
	a := pg.MoneyWithExponent(100, 2)
	b := pg.MoneyWithExponent(100, 4)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on exponent mismatch")
		}
	}()
	_ = a.Add(b)
}

func TestMoneyFromStringRejectsBadInput(t *testing.T) {
	if _, err := pg.MoneyFromString(""); err == nil {
		t.Error("empty string should error")
	}
	if _, err := pg.MoneyFromString("abc"); err == nil {
		t.Error("garbage should error")
	}
}

func TestMoneyZeroAndNegative(t *testing.T) {
	if !pg.MoneyFromCents(0).IsZero() {
		t.Error("0 should be zero")
	}
	if pg.MoneyFromCents(0).IsNegative() {
		t.Error("0 should not be negative")
	}
	if !pg.MoneyFromCents(-1).IsNegative() {
		t.Error("-1 should be negative")
	}
}

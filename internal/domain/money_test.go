package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func mustMoney(t *testing.T, value string) Money {
	t.Helper()

	m, err := ParseMoney(value, BRL)
	if err != nil {
		t.Fatalf("ParseMoney(%q) returned error: %v", value, err)
	}

	return m
}

func mustUnits(t *testing.T, amount int64, currency Currency) Money {
	t.Helper()

	m, err := NewMoney(amount, currency)
	if err != nil {
		t.Fatalf("NewMoney(%d, %q) returned error: %v", amount, currency, err)
	}

	return m
}

func TestParseMoney(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		text  string
	}{
		{"25.00", 2500, "25.00"},
		{"0.50", 50, "0.50"},
		{"100", 10000, "100.00"},
		{"12.3", 1230, "12.30"},
		{"0.01", 1, "0.01"},
		{"0", 0, "0.00"},
		{"007.5", 750, "7.50"},
	}

	for _, tt := range tests {
		got, err := ParseMoney(tt.input, BRL)
		if err != nil {
			t.Fatalf("ParseMoney(%q) returned error: %v", tt.input, err)
		}
		if got.Amount() != tt.want {
			t.Errorf("ParseMoney(%q) = %d, want %d", tt.input, got.Amount(), tt.want)
		}
		if got.Currency() != BRL {
			t.Errorf("ParseMoney(%q) currency = %q, want BRL", tt.input, got.Currency())
		}
		if got.String() != tt.text {
			t.Errorf("ParseMoney(%q).String() = %q, want %q", tt.input, got.String(), tt.text)
		}
	}
}

func TestParseMoneyRejectsInvalidInput(t *testing.T) {
	inputs := []string{
		"",
		" 25.00",
		"25.00 ",
		"-10.00",
		"+10.00",
		"1.999",
		"1.",
		".5",
		"1.2.3",
		"1,00",
		"1e3",
		"NaN",
		"Infinity",
		"abc",
	}

	for _, input := range inputs {
		if _, err := ParseMoney(input, BRL); !errors.Is(err, ErrInvalidMoney) {
			t.Errorf("ParseMoney(%q) error = %v, want ErrInvalidMoney", input, err)
		}
	}
}

func TestParseMoneyRejectsInvalidCurrency(t *testing.T) {
	for _, currency := range []Currency{"", "brl", "XXX", "BRLL"} {
		if _, err := ParseMoney("1.00", currency); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("ParseMoney with currency %q error = %v, want ErrInvalidCurrency", currency, err)
		}
	}
}

func TestParseMoneyOverflow(t *testing.T) {
	got, err := ParseMoney("92233720368547758.07", BRL)
	if err != nil {
		t.Fatalf("ParseMoney returned error: %v", err)
	}
	if got.Amount() != maxInt64 {
		t.Errorf("amount = %d, want %d", got.Amount(), maxInt64)
	}

	for _, input := range []string{"92233720368547758.08", "92233720368547759", "99999999999999999999"} {
		if _, err := ParseMoney(input, BRL); !errors.Is(err, ErrMoneyOverflow) {
			t.Errorf("ParseMoney(%q) error = %v, want ErrMoneyOverflow", input, err)
		}
	}
}

func TestMoneyStringMinimumInt64(t *testing.T) {
	got := mustUnits(t, minInt64, BRL).String()
	want := "-92233720368547758.08"

	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestZeroMoney(t *testing.T) {
	zero, err := ZeroMoney(BRL)
	if err != nil {
		t.Fatal(err)
	}
	if !zero.IsZero() || zero.Currency() != BRL || zero.String() != "0.00" {
		t.Errorf("ZeroMoney(BRL) = %s %s", zero, zero.Currency())
	}

	if _, err := ZeroMoney("XXX"); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("ZeroMoney(XXX) error = %v, want ErrInvalidCurrency", err)
	}
}

func TestMoneyArithmetic(t *testing.T) {
	a := mustMoney(t, "100.00")
	b := mustMoney(t, "80.00")

	sum, err := a.Add(b)
	if err != nil || sum.Amount() != 18000 {
		t.Errorf("Add = %v, %v; want 18000", sum.Amount(), err)
	}

	diff, err := b.Sub(a)
	if err != nil || diff.Amount() != -2000 || !diff.IsNegative() {
		t.Errorf("Sub = %v, %v; want -2000", diff.Amount(), err)
	}

	neg, err := a.Neg()
	if err != nil || neg.Amount() != -10000 {
		t.Errorf("Neg = %v, %v; want -10000", neg.Amount(), err)
	}

	if a.Amount() != 10000 || b.Amount() != 8000 {
		t.Errorf("operands changed: %d, %d", a.Amount(), b.Amount())
	}
}

func TestMoneyCompare(t *testing.T) {
	small := mustMoney(t, "1.00")
	big := mustMoney(t, "2.00")

	tests := []struct {
		a, b Money
		want int
	}{
		{small, big, -1},
		{big, small, 1},
		{small, small, 0},
	}

	for _, tt := range tests {
		got, err := tt.a.Compare(tt.b)
		if err != nil || got != tt.want {
			t.Errorf("%s.Compare(%s) = %d, %v; want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
}

func TestMoneyArithmeticOverflow(t *testing.T) {
	maxMoney := mustUnits(t, maxInt64, BRL)
	minMoney := mustUnits(t, minInt64, BRL)
	one := mustUnits(t, 1, BRL)
	minusOne := mustUnits(t, -1, BRL)

	if _, err := maxMoney.Add(one); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("max + 1 error = %v, want ErrMoneyOverflow", err)
	}
	if _, err := minMoney.Add(minusOne); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("min + (-1) error = %v, want ErrMoneyOverflow", err)
	}
	if _, err := minMoney.Sub(one); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("min - 1 error = %v, want ErrMoneyOverflow", err)
	}
	if _, err := maxMoney.Sub(minusOne); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("max - (-1) error = %v, want ErrMoneyOverflow", err)
	}
	if _, err := minMoney.Neg(); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("-min error = %v, want ErrMoneyOverflow", err)
	}

	if got, err := maxMoney.Add(minusOne); err != nil || got.Amount() != maxInt64-1 {
		t.Errorf("max + (-1) = %d, %v", got.Amount(), err)
	}
	if got, err := maxMoney.Neg(); err != nil || got.Amount() != -maxInt64 {
		t.Errorf("-max = %d, %v", got.Amount(), err)
	}
}

func TestMoneyCurrencyMismatch(t *testing.T) {
	brl := mustUnits(t, 100, BRL)
	usd := mustUnits(t, 100, USD)

	if _, err := brl.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Compare(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Compare error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestMoneyZeroValueIsRejected(t *testing.T) {
	var uninitialized Money
	valid := mustUnits(t, 100, BRL)

	if uninitialized.IsValid() {
		t.Fatal("zero value Money must not be valid")
	}
	if _, err := valid.Add(uninitialized); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("Add error = %v, want ErrInvalidCurrency", err)
	}
	if _, err := uninitialized.Sub(valid); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("Sub error = %v, want ErrInvalidCurrency", err)
	}
	if _, err := uninitialized.Neg(); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("Neg error = %v, want ErrInvalidCurrency", err)
	}
	if _, err := uninitialized.Compare(valid); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("Compare error = %v, want ErrInvalidCurrency", err)
	}
	if _, err := json.Marshal(uninitialized); err == nil {
		t.Error("Marshal of zero value Money expected an error")
	}
}

func TestMoneyJSON(t *testing.T) {
	data, err := json.Marshal(mustMoney(t, "25"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"amount":"25.00","currency":"BRL"}` {
		t.Errorf("Marshal = %s", data)
	}

	var got Money
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != mustMoney(t, "25.00") {
		t.Errorf("Unmarshal = %s %s", got, got.Currency())
	}

	data, err = json.Marshal(mustUnits(t, -150, BRL))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"amount":"-1.50","currency":"BRL"}` {
		t.Errorf("Marshal negative = %s", data)
	}
}

func TestMoneyJSONRejectsInvalidInput(t *testing.T) {
	inputs := []string{
		`{"amount":25.00,"currency":"BRL"}`,
		`{"amount":"-25.00","currency":"BRL"}`,
		`{"amount":"1.999","currency":"BRL"}`,
		`{"amount":"1e3","currency":"BRL"}`,
		`{"amount":"25.00"}`,
		`{"currency":"BRL"}`,
		`{"amount":"25.00","currency":"XXX"}`,
		`{"amount":"25.00","currency":"BRL","extra":1}`,
		`{"amount":null,"currency":"BRL"}`,
		`"25.00"`,
	}

	for _, input := range inputs {
		var m Money
		if err := json.Unmarshal([]byte(input), &m); err == nil {
			t.Errorf("Unmarshal(%s) expected an error", input)
		}
	}
}

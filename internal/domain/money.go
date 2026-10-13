package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
	CAD Currency = "CAD"
	AUD Currency = "AUD"
	CHF Currency = "CHF"
	CNY Currency = "CNY"
	NZD Currency = "NZD"
	SGD Currency = "SGD"
	HKD Currency = "HKD"
	MXN Currency = "MXN"
)

type Money struct {
	amount   int64
	currency Currency
}

const (
	maxInt64 = int64(1<<63 - 1)
	minInt64 = int64(-1 << 63)
)

var (
	ErrInvalidMoney     = errors.New("invalid money amount")
	ErrMoneyOverflow    = errors.New("money amount overflow")
	ErrInvalidCurrency  = errors.New("invalid or unsupported currency")
	ErrCurrencyMismatch = errors.New("currency mismatch")
)

var currencies = map[Currency]struct{}{
	BRL: {},
	USD: {},
	EUR: {},
	GBP: {},
	CAD: {},
	AUD: {},
	CHF: {},
	CNY: {},
	NZD: {},
	SGD: {},
	HKD: {},
	MXN: {},
}

func ValidateCurrency(currency Currency) error {
	if _, ok := currencies[currency]; !ok {
		return ErrInvalidCurrency
	}

	return nil
}

func NewMoney(amount int64, currency Currency) (Money, error) {
	if err := ValidateCurrency(currency); err != nil {
		return Money{}, err
	}

	return Money{
		amount:   amount,
		currency: currency,
	}, nil
}

func ZeroMoney(currency Currency) (Money, error) {
	return NewMoney(0, currency)
}

func onlyDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

func ParseMoney(value string, currency Currency) (Money, error) {
	if err := ValidateCurrency(currency); err != nil {
		return Money{}, err
	}

	if value == "" || strings.TrimSpace(value) != value {
		return Money{}, ErrInvalidMoney
	}

	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || !onlyDigits(parts[0]) {
		return Money{}, ErrInvalidMoney
	}

	fraction := "00"

	if len(parts) == 2 {
		if len(parts[1]) == 0 || len(parts[1]) > 2 || !onlyDigits(parts[1]) {
			return Money{}, ErrInvalidMoney
		}

		fraction = parts[1]
		if len(fraction) == 1 {
			fraction += "0"
		}
	}

	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Money{}, ErrMoneyOverflow
	}

	fractionCents, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return Money{}, ErrInvalidMoney
	}

	maxWhole := (maxInt64 - fractionCents) / 100
	if whole > maxWhole {
		return Money{}, ErrMoneyOverflow
	}

	return Money{
		amount:   whole*100 + fractionCents,
		currency: currency,
	}, nil
}

func (m Money) Amount() int64 {
	return m.amount
}

func (m Money) Currency() Currency {
	return m.currency
}

func (m Money) IsValid() bool {
	return ValidateCurrency(m.currency) == nil
}

func (m Money) IsZero() bool {
	return m.amount == 0
}

func (m Money) IsPositive() bool {
	return m.amount > 0
}

func (m Money) IsNegative() bool {
	return m.amount < 0
}

func (m Money) requireValid() error {
	if !m.IsValid() {
		return ErrInvalidCurrency
	}

	return nil
}

func (m Money) requireSameCurrency(other Money) error {
	if err := m.requireValid(); err != nil {
		return err
	}

	if err := other.requireValid(); err != nil {
		return err
	}

	if m.currency != other.currency {
		return ErrCurrencyMismatch
	}

	return nil
}

func (m Money) Add(other Money) (Money, error) {
	if err := m.requireSameCurrency(other); err != nil {
		return Money{}, err
	}

	if (other.amount > 0 && m.amount > maxInt64-other.amount) ||
		(other.amount < 0 && m.amount < minInt64-other.amount) {
		return Money{}, ErrMoneyOverflow
	}

	return Money{
		amount:   m.amount + other.amount,
		currency: m.currency,
	}, nil
}

func (m Money) Sub(other Money) (Money, error) {
	if err := m.requireSameCurrency(other); err != nil {
		return Money{}, err
	}

	if (other.amount < 0 && m.amount > maxInt64+other.amount) ||
		(other.amount > 0 && m.amount < minInt64+other.amount) {
		return Money{}, ErrMoneyOverflow
	}

	return Money{
		amount:   m.amount - other.amount,
		currency: m.currency,
	}, nil
}

func (m Money) Neg() (Money, error) {
	if err := m.requireValid(); err != nil {
		return Money{}, err
	}

	if m.amount == minInt64 {
		return Money{}, ErrMoneyOverflow
	}

	return Money{
		amount:   -m.amount,
		currency: m.currency,
	}, nil
}

func (m Money) Compare(other Money) (int, error) {
	if err := m.requireSameCurrency(other); err != nil {
		return 0, err
	}

	switch {
	case m.amount < other.amount:
		return -1, nil
	case m.amount > other.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) String() string {
	var (
		sign      string
		magnitude uint64
	)

	if m.amount < 0 {
		sign = "-"
		magnitude = uint64(-(m.amount + 1)) + 1
	} else {
		magnitude = uint64(m.amount)
	}

	return fmt.Sprintf("%s%d.%02d", sign, magnitude/100, magnitude%100)
}

type moneyJSON struct {
	Amount   string   `json:"amount"`
	Currency Currency `json:"currency"`
}

func (m Money) MarshalJSON() ([]byte, error) {
	if err := m.requireValid(); err != nil {
		return nil, err
	}

	return json.Marshal(moneyJSON{
		Amount:   m.String(),
		Currency: m.currency,
	})
}

func (m *Money) UnmarshalJSON(data []byte) error {
	var input struct {
		Amount   *string   `json:"amount"`
		Currency *Currency `json:"currency"`
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&input); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMoney, err)
	}

	if decoder.More() {
		return ErrInvalidMoney
	}

	if input.Amount == nil {
		return ErrInvalidMoney
	}

	if input.Currency == nil {
		return ErrInvalidCurrency
	}

	parsed, err := ParseMoney(*input.Amount, *input.Currency)
	if err != nil {
		return err
	}

	*m = parsed
	return nil
}

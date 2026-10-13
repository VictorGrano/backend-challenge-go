package domain

import (
	"errors"
	"testing"
	"time"
)

func TestNewWalletLedgerEntry(t *testing.T) {
	tests := []struct {
		name      string
		direction LedgerDirection
		before    string
		after     string
	}{
		{"debit", LedgerDebit, "100.00", "20.00"},
		{"credit", LedgerCredit, "100.00", "180.00"},
		{"opening credit", LedgerCredit, "0", "80.00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := NewWalletLedgerEntry(
				"entry-1", "wallet-1", "tx-1", tt.direction,
				mustMoney(t, "80.00"), mustMoney(t, tt.before), mustMoney(t, tt.after), testNow,
			)
			if err != nil {
				t.Fatal(err)
			}

			if entry.ID() != "entry-1" || entry.Money() != mustMoney(t, "80.00") || !entry.CreatedAt().Equal(testNow) {
				t.Errorf("entry = %s %s %v", entry.ID(), entry.Money(), entry.CreatedAt())
			}
		})
	}
}

func TestNewWalletLedgerEntryRejectsInvalidInput(t *testing.T) {
	money := mustMoney(t, "80.00")
	before := mustMoney(t, "100.00")
	after := mustMoney(t, "20.00")

	tests := []struct {
		name      string
		id        string
		direction LedgerDirection
		money     Money
		before    Money
		after     Money
		createdAt time.Time
		want      error
	}{
		{"blank id", "", LedgerDebit, money, before, after, testNow, ErrInvalidLedgerEntry},
		{"zero time", "entry-1", LedgerDebit, money, before, after, time.Time{}, ErrInvalidLedgerEntry},
		{"unknown direction", "entry-1", "TRANSFER", money, before, after, testNow, ErrInvalidLedgerDirection},
		{"zero money", "entry-1", LedgerDebit, mustMoney(t, "0"), before, before, testNow, ErrInvalidLedgerEntry},
		{"wrong debit balance", "entry-1", LedgerDebit, money, before, mustMoney(t, "30.00"), testNow, ErrInvalidLedgerEntry},
		{"wrong credit balance", "entry-1", LedgerCredit, money, before, after, testNow, ErrInvalidLedgerEntry},
		{"negative balance after", "entry-1", LedgerDebit, money, mustMoney(t, "50.00"), mustUnits(t, -3000, BRL), testNow, ErrInvalidLedgerEntry},
		{"currency mismatch", "entry-1", LedgerDebit, mustUnits(t, 8000, USD), before, after, testNow, ErrCurrencyMismatch},
		{"uninitialized money", "entry-1", LedgerDebit, Money{}, before, after, testNow, ErrInvalidCurrency},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewWalletLedgerEntry(
				tt.id, "wallet-1", "tx-1", tt.direction, tt.money, tt.before, tt.after, tt.createdAt,
			)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRehydrateWalletLedgerEntryChecksInvariant(t *testing.T) {
	_, err := RehydrateWalletLedgerEntry(
		"entry-1", "wallet-1", "tx-1", LedgerCredit,
		mustMoney(t, "80.00"), mustMoney(t, "100.00"), mustMoney(t, "100.00"), testNow,
	)
	if !errors.Is(err, ErrInvalidLedgerEntry) {
		t.Fatalf("error = %v, want ErrInvalidLedgerEntry", err)
	}
}

package domain

import (
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func newTestWallet(t *testing.T, balance string) *Wallet {
	t.Helper()

	wallet, err := NewWallet("wallet-1", "player-1", mustMoney(t, balance), testNow)
	if err != nil {
		t.Fatal(err)
	}

	return wallet
}

func TestNewWallet(t *testing.T) {
	wallet := newTestWallet(t, "0")

	if wallet.Version() != 1 {
		t.Errorf("version = %d, want 1", wallet.Version())
	}
	if !wallet.Balance().IsZero() || wallet.Currency() != BRL {
		t.Errorf("balance = %s %s", wallet.Balance(), wallet.Currency())
	}
	if !wallet.CreatedAt().Equal(testNow) || !wallet.UpdatedAt().Equal(testNow) {
		t.Errorf("timestamps = %v, %v", wallet.CreatedAt(), wallet.UpdatedAt())
	}
}

func TestNewWalletRejectsInvalidInput(t *testing.T) {
	balance := mustMoney(t, "10.00")

	tests := []struct {
		name     string
		id       string
		playerID string
		balance  Money
		now      time.Time
		want     error
	}{
		{"blank id", " ", "player-1", balance, testNow, ErrInvalidWallet},
		{"blank player", "wallet-1", "", balance, testNow, ErrInvalidWallet},
		{"zero time", "wallet-1", "player-1", balance, time.Time{}, ErrInvalidWallet},
		{"uninitialized money", "wallet-1", "player-1", Money{}, testNow, ErrInvalidCurrency},
		{"negative balance", "wallet-1", "player-1", mustUnits(t, -1, BRL), testNow, ErrInvalidOpeningBalance},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewWallet(tt.id, tt.playerID, tt.balance, tt.now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestWalletDebit(t *testing.T) {
	wallet := newTestWallet(t, "100.00")
	later := testNow.Add(time.Minute)

	entry, err := wallet.Debit("entry-1", "tx-1", mustMoney(t, "80.00"), later)
	if err != nil {
		t.Fatal(err)
	}

	if wallet.Balance() != mustMoney(t, "20.00") {
		t.Errorf("balance = %s, want 20.00", wallet.Balance())
	}
	if wallet.Version() != 2 {
		t.Errorf("version = %d, want 2", wallet.Version())
	}
	if !wallet.UpdatedAt().Equal(later) {
		t.Errorf("updatedAt = %v, want %v", wallet.UpdatedAt(), later)
	}

	if entry.Direction() != LedgerDebit || entry.WalletID() != "wallet-1" || entry.TransactionID() != "tx-1" {
		t.Errorf("entry = %s %s %s", entry.Direction(), entry.WalletID(), entry.TransactionID())
	}
	if entry.BalanceBefore() != mustMoney(t, "100.00") || entry.BalanceAfter() != mustMoney(t, "20.00") {
		t.Errorf("entry balances = %s -> %s", entry.BalanceBefore(), entry.BalanceAfter())
	}
}

func TestWalletDebitInsufficientFunds(t *testing.T) {
	wallet := newTestWallet(t, "20.00")

	entry, err := wallet.Debit("entry-1", "tx-1", mustMoney(t, "80.00"), testNow)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}
	if entry != nil {
		t.Error("rejected debit must not produce a ledger entry")
	}
	if wallet.Balance() != mustMoney(t, "20.00") || wallet.Version() != 1 {
		t.Errorf("wallet changed after rejected debit: %s v%d", wallet.Balance(), wallet.Version())
	}
}

func TestWalletDebitEntireBalance(t *testing.T) {
	wallet := newTestWallet(t, "20.00")

	if _, err := wallet.Debit("entry-1", "tx-1", mustMoney(t, "20.00"), testNow); err != nil {
		t.Fatal(err)
	}
	if !wallet.Balance().IsZero() {
		t.Errorf("balance = %s, want 0.00", wallet.Balance())
	}
}

func TestWalletCredit(t *testing.T) {
	wallet := newTestWallet(t, "20.00")

	entry, err := wallet.Credit("entry-1", "tx-1", mustMoney(t, "30.00"), testNow)
	if err != nil {
		t.Fatal(err)
	}

	if wallet.Balance() != mustMoney(t, "50.00") || wallet.Version() != 2 {
		t.Errorf("wallet = %s v%d, want 50.00 v2", wallet.Balance(), wallet.Version())
	}
	if entry.Direction() != LedgerCredit || entry.BalanceAfter() != mustMoney(t, "50.00") {
		t.Errorf("entry = %s -> %s", entry.Direction(), entry.BalanceAfter())
	}
}

func TestWalletRejectsInvalidMovements(t *testing.T) {
	tests := []struct {
		name   string
		amount Money
		now    time.Time
		want   error
	}{
		{"zero amount", mustMoney(t, "0"), testNow, ErrInvalidAmount},
		{"negative amount", mustUnits(t, -1, BRL), testNow, ErrInvalidAmount},
		{"other currency", mustUnits(t, 100, USD), testNow, ErrCurrencyMismatch},
		{"uninitialized money", Money{}, testNow, ErrInvalidCurrency},
		{"zero time", mustMoney(t, "1.00"), time.Time{}, ErrInvalidWallet},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wallet := newTestWallet(t, "100.00")

			if _, err := wallet.Debit("entry-1", "tx-1", tt.amount, tt.now); !errors.Is(err, tt.want) {
				t.Errorf("Debit error = %v, want %v", err, tt.want)
			}
			if _, err := wallet.Credit("entry-1", "tx-1", tt.amount, tt.now); !errors.Is(err, tt.want) {
				t.Errorf("Credit error = %v, want %v", err, tt.want)
			}
			if wallet.Balance() != mustMoney(t, "100.00") || wallet.Version() != 1 {
				t.Errorf("wallet changed: %s v%d", wallet.Balance(), wallet.Version())
			}
		})
	}
}

func TestWalletRejectsBlankLedgerIdentifiers(t *testing.T) {
	wallet := newTestWallet(t, "100.00")
	amount := mustMoney(t, "1.00")

	if _, err := wallet.Debit("", "tx-1", amount, testNow); !errors.Is(err, ErrInvalidLedgerEntry) {
		t.Errorf("blank entry id error = %v, want ErrInvalidLedgerEntry", err)
	}
	if _, err := wallet.Credit("entry-1", "", amount, testNow); !errors.Is(err, ErrInvalidLedgerEntry) {
		t.Errorf("blank transaction id error = %v, want ErrInvalidLedgerEntry", err)
	}
	if wallet.Balance() != mustMoney(t, "100.00") || wallet.Version() != 1 {
		t.Errorf("wallet changed: %s v%d", wallet.Balance(), wallet.Version())
	}
}

func TestWalletCreditOverflow(t *testing.T) {
	wallet, err := NewWallet("wallet-1", "player-1", mustUnits(t, maxInt64, BRL), testNow)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := wallet.Credit("entry-1", "tx-1", mustUnits(t, 1, BRL), testNow); !errors.Is(err, ErrMoneyOverflow) {
		t.Fatalf("error = %v, want ErrMoneyOverflow", err)
	}
	if wallet.Balance().Amount() != maxInt64 || wallet.Version() != 1 {
		t.Errorf("wallet changed after overflow: %s v%d", wallet.Balance(), wallet.Version())
	}
}

func TestRehydrateWallet(t *testing.T) {
	updated := testNow.Add(time.Hour)

	wallet, err := RehydrateWallet("wallet-1", "player-1", mustMoney(t, "20.00"), 7, testNow, updated)
	if err != nil {
		t.Fatal(err)
	}

	if wallet.Balance() != mustMoney(t, "20.00") || wallet.Version() != 7 {
		t.Errorf("wallet = %s v%d", wallet.Balance(), wallet.Version())
	}
	if !wallet.CreatedAt().Equal(testNow) || !wallet.UpdatedAt().Equal(updated) {
		t.Errorf("timestamps = %v, %v", wallet.CreatedAt(), wallet.UpdatedAt())
	}
}

func TestRehydrateWalletRejectsInvalidState(t *testing.T) {
	balance := mustMoney(t, "20.00")

	tests := []struct {
		name      string
		id        string
		balance   Money
		version   int64
		createdAt time.Time
		updatedAt time.Time
		want      error
	}{
		{"blank id", "", balance, 1, testNow, testNow, ErrInvalidWallet},
		{"version zero", "wallet-1", balance, 0, testNow, testNow, ErrInvalidWallet},
		{"zero created", "wallet-1", balance, 1, time.Time{}, testNow, ErrInvalidWallet},
		{"updated before created", "wallet-1", balance, 1, testNow, testNow.Add(-time.Second), ErrInvalidWallet},
		{"negative balance", "wallet-1", mustUnits(t, -1, BRL), 1, testNow, testNow, ErrInvalidWallet},
		{"uninitialized money", "wallet-1", Money{}, 1, testNow, testNow, ErrInvalidCurrency},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RehydrateWallet(tt.id, "player-1", tt.balance, tt.version, tt.createdAt, tt.updatedAt)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

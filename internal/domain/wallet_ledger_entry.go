package domain

import (
	"errors"
	"time"
)

type LedgerDirection string

const (
	LedgerDebit  LedgerDirection = "DEBIT"
	LedgerCredit LedgerDirection = "CREDIT"
)

var (
	ErrInvalidLedgerEntry     = errors.New("invalid wallet ledger entry")
	ErrInvalidLedgerDirection = errors.New("invalid ledger direction")
)

type WalletLedgerEntry struct {
	id            string
	walletID      string
	transactionID string
	direction     LedgerDirection
	money         Money
	balanceBefore Money
	balanceAfter  Money
	createdAt     time.Time
}

func NewWalletLedgerEntry(
	id string,
	walletID string,
	transactionID string,
	direction LedgerDirection,
	money Money,
	balanceBefore Money,
	balanceAfter Money,
	createdAt time.Time,
) (*WalletLedgerEntry, error) {
	return buildLedgerEntry(id, walletID, transactionID, direction, money, balanceBefore, balanceAfter, createdAt)
}

func RehydrateWalletLedgerEntry(
	id string,
	walletID string,
	transactionID string,
	direction LedgerDirection,
	money Money,
	balanceBefore Money,
	balanceAfter Money,
	createdAt time.Time,
) (*WalletLedgerEntry, error) {
	return buildLedgerEntry(id, walletID, transactionID, direction, money, balanceBefore, balanceAfter, createdAt)
}

func buildLedgerEntry(
	id string,
	walletID string,
	transactionID string,
	direction LedgerDirection,
	money Money,
	balanceBefore Money,
	balanceAfter Money,
	createdAt time.Time,
) (*WalletLedgerEntry, error) {
	if blank(id) || blank(walletID) || blank(transactionID) || createdAt.IsZero() {
		return nil, ErrInvalidLedgerEntry
	}

	if direction != LedgerDebit && direction != LedgerCredit {
		return nil, ErrInvalidLedgerDirection
	}

	if !money.IsValid() || !balanceBefore.IsValid() || !balanceAfter.IsValid() {
		return nil, ErrInvalidCurrency
	}

	if !money.IsPositive() || balanceBefore.IsNegative() || balanceAfter.IsNegative() {
		return nil, ErrInvalidLedgerEntry
	}

	var (
		expected Money
		err      error
	)

	switch direction {
	case LedgerDebit:
		expected, err = balanceBefore.Sub(money)
	default:
		expected, err = balanceBefore.Add(money)
	}

	if err != nil {
		return nil, err
	}

	cmp, err := expected.Compare(balanceAfter)
	if err != nil {
		return nil, err
	}

	if cmp != 0 {
		return nil, ErrInvalidLedgerEntry
	}

	return &WalletLedgerEntry{
		id:            id,
		walletID:      walletID,
		transactionID: transactionID,
		direction:     direction,
		money:         money,
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		createdAt:     createdAt.UTC(),
	}, nil
}

func (e *WalletLedgerEntry) ID() string                 { return e.id }
func (e *WalletLedgerEntry) WalletID() string           { return e.walletID }
func (e *WalletLedgerEntry) TransactionID() string      { return e.transactionID }
func (e *WalletLedgerEntry) Direction() LedgerDirection { return e.direction }
func (e *WalletLedgerEntry) Money() Money               { return e.money }
func (e *WalletLedgerEntry) BalanceBefore() Money       { return e.balanceBefore }
func (e *WalletLedgerEntry) BalanceAfter() Money        { return e.balanceAfter }
func (e *WalletLedgerEntry) CreatedAt() time.Time       { return e.createdAt }

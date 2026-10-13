package domain

import (
	"errors"
	"time"
)

var (
	ErrInvalidWallet         = errors.New("invalid wallet")
	ErrInvalidAmount         = errors.New("amount must be greater than zero")
	ErrInsufficientFunds     = errors.New("insufficient funds")
	ErrInvalidOpeningBalance = errors.New("opening balance cannot be negative")
)

type Wallet struct {
	id        string
	playerID  string
	balance   Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

func NewWallet(id, playerID string, openingBalance Money, now time.Time) (*Wallet, error) {
	if blank(id) || blank(playerID) || now.IsZero() {
		return nil, ErrInvalidWallet
	}

	if !openingBalance.IsValid() {
		return nil, ErrInvalidCurrency
	}

	if openingBalance.IsNegative() {
		return nil, ErrInvalidOpeningBalance
	}

	now = now.UTC()

	return &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   openingBalance,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}, nil
}

func RehydrateWallet(
	id string,
	playerID string,
	balance Money,
	version int64,
	createdAt time.Time,
	updatedAt time.Time,
) (*Wallet, error) {
	if blank(id) || blank(playerID) || version < 1 ||
		createdAt.IsZero() || updatedAt.IsZero() || updatedAt.Before(createdAt) {
		return nil, ErrInvalidWallet
	}

	if !balance.IsValid() {
		return nil, ErrInvalidCurrency
	}

	if balance.IsNegative() {
		return nil, ErrInvalidWallet
	}

	return &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   balance,
		version:   version,
		createdAt: createdAt.UTC(),
		updatedAt: updatedAt.UTC(),
	}, nil
}

func (w *Wallet) Debit(
	entryID string,
	transactionID string,
	amount Money,
	now time.Time,
) (*WalletLedgerEntry, error) {
	if err := w.validateMovement(amount, now); err != nil {
		return nil, err
	}

	cmp, err := w.balance.Compare(amount)
	if err != nil {
		return nil, err
	}

	if cmp < 0 {
		return nil, ErrInsufficientFunds
	}

	after, err := w.balance.Sub(amount)
	if err != nil {
		return nil, err
	}

	return w.commit(entryID, transactionID, LedgerDebit, amount, after, now)
}

func (w *Wallet) Credit(
	entryID string,
	transactionID string,
	amount Money,
	now time.Time,
) (*WalletLedgerEntry, error) {
	if err := w.validateMovement(amount, now); err != nil {
		return nil, err
	}

	after, err := w.balance.Add(amount)
	if err != nil {
		return nil, err
	}

	return w.commit(entryID, transactionID, LedgerCredit, amount, after, now)
}

func (w *Wallet) validateMovement(amount Money, now time.Time) error {
	if now.IsZero() {
		return ErrInvalidWallet
	}

	if !amount.IsValid() {
		return ErrInvalidCurrency
	}

	if amount.Currency() != w.balance.Currency() {
		return ErrCurrencyMismatch
	}

	if !amount.IsPositive() {
		return ErrInvalidAmount
	}

	return nil
}

func (w *Wallet) commit(
	entryID string,
	transactionID string,
	direction LedgerDirection,
	amount Money,
	after Money,
	now time.Time,
) (*WalletLedgerEntry, error) {
	entry, err := NewWalletLedgerEntry(
		entryID,
		w.id,
		transactionID,
		direction,
		amount,
		w.balance,
		after,
		now,
	)
	if err != nil {
		return nil, err
	}

	w.balance = after
	w.version++
	w.updatedAt = laterOf(w.updatedAt, now)

	return entry, nil
}

func (w *Wallet) ID() string           { return w.id }
func (w *Wallet) PlayerID() string     { return w.playerID }
func (w *Wallet) Balance() Money       { return w.balance }
func (w *Wallet) Currency() Currency   { return w.balance.Currency() }
func (w *Wallet) Version() int64       { return w.version }
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

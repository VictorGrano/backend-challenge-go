package domain

import "errors"

var (
	ErrWalletNotFound      = errors.New("wallet not found")
	ErrWalletAlreadyExists = errors.New("wallet already exists for player and currency")
	ErrConcurrentUpdate    = errors.New("wallet was updated concurrently")

	ErrTransactionNotFound      = errors.New("wager transaction not found")
	ErrDuplicateTransaction     = errors.New("wager transaction already exists")
	ErrDuplicateIdempotencyKey  = errors.New("idempotency key already used")
	ErrOpeningAlreadyExists     = errors.New("wallet already has an opening transaction")
	ErrReferenceAlreadyReversed = errors.New("reference transaction already reversed")

	ErrDuplicateLedgerEntry = errors.New("ledger entry already exists for wallet and transaction")
	ErrDuplicateEvent       = errors.New("outbox event already exists")
)

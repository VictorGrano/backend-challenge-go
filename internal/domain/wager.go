package domain

import (
	"errors"
	"time"
)

type WagerKind string

const (
	WagerOpening  WagerKind = "OPENING"
	WagerBet      WagerKind = "BET"
	WagerWin      WagerKind = "WIN"
	WagerLoss     WagerKind = "LOSS"
	WagerRefund   WagerKind = "REFUND"
	WagerRollback WagerKind = "ROLLBACK"
)

func (k WagerKind) IsExternal() bool {
	switch k {
	case WagerBet, WagerWin, WagerLoss, WagerRefund, WagerRollback:
		return true
	default:
		return false
	}
}

func (k WagerKind) RequiresReference() bool {
	return k == WagerRefund || k == WagerRollback
}

type WagerStatus string

const (
	WagerPending          WagerStatus = "PENDING"
	WagerPendingReference WagerStatus = "PENDING_REFERENCE"
	WagerProcessed        WagerStatus = "PROCESSED"
	WagerRejected         WagerStatus = "REJECTED"
	WagerFailed           WagerStatus = "FAILED"
)

func (s WagerStatus) IsTerminal() bool {
	return s == WagerProcessed || s == WagerRejected || s == WagerFailed
}

func (s WagerStatus) valid() bool {
	switch s {
	case WagerPending, WagerPendingReference, WagerProcessed, WagerRejected, WagerFailed:
		return true
	default:
		return false
	}
}

type FailureCode string

const (
	FailureInsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	FailureReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"
	FailureReferenceMismatch         FailureCode = "REFERENCE_MISMATCH"
	FailureReferenceNotProcessed     FailureCode = "REFERENCE_NOT_PROCESSED"
	FailureReferenceAlreadyReversed  FailureCode = "REFERENCE_ALREADY_REVERSED"
	FailureWalletMismatch            FailureCode = "WALLET_MISMATCH"
	FailureCurrencyMismatch          FailureCode = "CURRENCY_MISMATCH"
	FailureInfrastructure            FailureCode = "INFRASTRUCTURE_FAILURE"
)

var (
	ErrInvalidWagerTransaction = errors.New("invalid wager transaction")
	ErrInvalidWagerKind        = errors.New("invalid wager kind")
	ErrInvalidWagerStatus      = errors.New("invalid wager status")
	ErrInvalidWagerAmount      = errors.New("invalid amount for wager kind")
	ErrReferenceRequired       = errors.New("reference transaction is required")
	ErrInvalidFailureCode      = errors.New("failure code is required")
	ErrInvalidTransition       = errors.New("invalid wager status transition")
)

type WagerTransaction struct {
	id                             string
	providerID                     string
	externalTransactionID          string
	idempotencyKey                 string
	payloadHash                    string
	walletID                       string
	playerID                       string
	roundID                        string
	gameID                         string
	kind                           WagerKind
	money                          Money
	referenceExternalTransactionID string
	referenceTransactionID         string
	status                         WagerStatus
	failureCode                    FailureCode
	resultBalance                  Money
	hasResultBalance               bool
	createdAt                      time.Time
	updatedAt                      time.Time
}

type NewWagerTransactionParams struct {
	ID                             string
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           WagerKind
	Money                          Money
	ReferenceExternalTransactionID string
	Now                            time.Time
}

func NewWagerTransaction(p NewWagerTransactionParams) (*WagerTransaction, error) {
	if !p.Kind.IsExternal() {
		return nil, ErrInvalidWagerKind
	}

	if p.Now.IsZero() {
		return nil, ErrInvalidWagerTransaction
	}

	now := p.Now.UTC()

	tx := &WagerTransaction{
		id:                             p.ID,
		providerID:                     p.ProviderID,
		externalTransactionID:          p.ExternalTransactionID,
		idempotencyKey:                 p.IdempotencyKey,
		payloadHash:                    p.PayloadHash,
		walletID:                       p.WalletID,
		playerID:                       p.PlayerID,
		roundID:                        p.RoundID,
		gameID:                         p.GameID,
		kind:                           p.Kind,
		money:                          p.Money,
		referenceExternalTransactionID: p.ReferenceExternalTransactionID,
		status:                         WagerPending,
		createdAt:                      now,
		updatedAt:                      now,
	}

	if err := tx.validate(); err != nil {
		return nil, err
	}

	return tx, nil
}

func NewOpeningTransaction(
	id string,
	walletID string,
	playerID string,
	money Money,
	now time.Time,
) (*WagerTransaction, error) {
	if now.IsZero() {
		return nil, ErrInvalidWagerTransaction
	}

	now = now.UTC()

	tx := &WagerTransaction{
		id:        id,
		walletID:  walletID,
		playerID:  playerID,
		kind:      WagerOpening,
		money:     money,
		status:    WagerPending,
		createdAt: now,
		updatedAt: now,
	}

	if err := tx.validate(); err != nil {
		return nil, err
	}

	return tx, nil
}

type WagerTransactionState struct {
	ID                             string
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           WagerKind
	Money                          Money
	ReferenceExternalTransactionID string
	ReferenceTransactionID         string
	Status                         WagerStatus
	FailureCode                    FailureCode
	ResultBalance                  *Money
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}

func RehydrateWagerTransaction(s WagerTransactionState) (*WagerTransaction, error) {
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		return nil, ErrInvalidWagerTransaction
	}

	tx := &WagerTransaction{
		id:                             s.ID,
		providerID:                     s.ProviderID,
		externalTransactionID:          s.ExternalTransactionID,
		idempotencyKey:                 s.IdempotencyKey,
		payloadHash:                    s.PayloadHash,
		walletID:                       s.WalletID,
		playerID:                       s.PlayerID,
		roundID:                        s.RoundID,
		gameID:                         s.GameID,
		kind:                           s.Kind,
		money:                          s.Money,
		referenceExternalTransactionID: s.ReferenceExternalTransactionID,
		referenceTransactionID:         s.ReferenceTransactionID,
		status:                         s.Status,
		failureCode:                    s.FailureCode,
		createdAt:                      s.CreatedAt.UTC(),
		updatedAt:                      s.UpdatedAt.UTC(),
	}

	if s.ResultBalance != nil {
		tx.resultBalance = *s.ResultBalance
		tx.hasResultBalance = true
	}

	if err := tx.validate(); err != nil {
		return nil, err
	}

	return tx, nil
}

func (t *WagerTransaction) validate() error {
	if blank(t.id) || blank(t.walletID) || blank(t.playerID) {
		return ErrInvalidWagerTransaction
	}

	if t.kind == WagerOpening {
		if t.providerID != "" || t.externalTransactionID != "" ||
			t.idempotencyKey != "" || t.payloadHash != "" ||
			t.roundID != "" || t.gameID != "" ||
			t.referenceExternalTransactionID != "" || t.referenceTransactionID != "" {
			return ErrInvalidWagerTransaction
		}
	} else {
		if !t.kind.IsExternal() {
			return ErrInvalidWagerKind
		}

		if blank(t.providerID) || blank(t.externalTransactionID) ||
			blank(t.idempotencyKey) || blank(t.payloadHash) ||
			blank(t.roundID) || blank(t.gameID) {
			return ErrInvalidWagerTransaction
		}

		if t.kind.RequiresReference() && blank(t.referenceExternalTransactionID) {
			return ErrReferenceRequired
		}

		if t.referenceExternalTransactionID == t.externalTransactionID {
			return ErrInvalidWagerTransaction
		}

		if t.referenceTransactionID != "" &&
			(t.referenceExternalTransactionID == "" || t.referenceTransactionID == t.id) {
			return ErrInvalidWagerTransaction
		}
	}

	if err := validateWagerAmount(t.kind, t.money); err != nil {
		return err
	}

	return t.validateStatus()
}

func (t *WagerTransaction) validateStatus() error {
	if !t.status.valid() {
		return ErrInvalidWagerStatus
	}

	switch t.status {
	case WagerProcessed:
		if !t.hasResultBalance || t.failureCode != "" {
			return ErrInvalidWagerTransaction
		}

		return t.validateResultBalance(t.resultBalance)
	case WagerRejected, WagerFailed:
		if blank(string(t.failureCode)) || t.hasResultBalance {
			return ErrInvalidWagerTransaction
		}
	default:
		if t.failureCode != "" || t.hasResultBalance {
			return ErrInvalidWagerTransaction
		}

		if t.status == WagerPendingReference && t.referenceExternalTransactionID == "" {
			return ErrInvalidWagerTransaction
		}
	}

	return nil
}

func (t *WagerTransaction) validateResultBalance(balance Money) error {
	if !balance.IsValid() {
		return ErrInvalidCurrency
	}

	if balance.Currency() != t.money.Currency() {
		return ErrCurrencyMismatch
	}

	if balance.IsNegative() {
		return ErrInvalidWagerTransaction
	}

	return nil
}

func validateWagerAmount(kind WagerKind, money Money) error {
	if !money.IsValid() {
		return ErrInvalidCurrency
	}

	if kind == WagerLoss {
		if !money.IsZero() {
			return ErrInvalidWagerAmount
		}

		return nil
	}

	if !money.IsPositive() {
		return ErrInvalidWagerAmount
	}

	return nil
}

func (t *WagerTransaction) transition(now time.Time) error {
	if now.IsZero() {
		return ErrInvalidWagerTransaction
	}

	if t.status.IsTerminal() {
		return ErrInvalidTransition
	}

	return nil
}

func (t *WagerTransaction) MarkPendingReference(now time.Time) error {
	if now.IsZero() {
		return ErrInvalidWagerTransaction
	}

	if t.status != WagerPending {
		return ErrInvalidTransition
	}

	if t.referenceExternalTransactionID == "" {
		return ErrReferenceRequired
	}

	t.status = WagerPendingReference
	t.updatedAt = laterOf(t.updatedAt, now)

	return nil
}

func (t *WagerTransaction) ResolveReference(referenceTransactionID string, now time.Time) error {
	if err := t.transition(now); err != nil {
		return err
	}

	if t.referenceExternalTransactionID == "" {
		return ErrReferenceRequired
	}

	if blank(referenceTransactionID) || referenceTransactionID == t.id {
		return ErrInvalidWagerTransaction
	}

	if t.referenceTransactionID != "" && t.referenceTransactionID != referenceTransactionID {
		return ErrInvalidTransition
	}

	t.referenceTransactionID = referenceTransactionID
	t.updatedAt = laterOf(t.updatedAt, now)

	return nil
}

func (t *WagerTransaction) MarkProcessed(resultBalance Money, now time.Time) error {
	if err := t.transition(now); err != nil {
		return err
	}

	if t.kind.RequiresReference() && t.referenceTransactionID == "" {
		return ErrReferenceRequired
	}

	if err := t.validateResultBalance(resultBalance); err != nil {
		return err
	}

	t.status = WagerProcessed
	t.resultBalance = resultBalance
	t.hasResultBalance = true
	t.updatedAt = laterOf(t.updatedAt, now)

	return nil
}

func (t *WagerTransaction) Reject(code FailureCode, now time.Time) error {
	return t.finishWithFailure(WagerRejected, code, now)
}

func (t *WagerTransaction) Fail(code FailureCode, now time.Time) error {
	return t.finishWithFailure(WagerFailed, code, now)
}

func (t *WagerTransaction) finishWithFailure(status WagerStatus, code FailureCode, now time.Time) error {
	if err := t.transition(now); err != nil {
		return err
	}

	if blank(string(code)) {
		return ErrInvalidFailureCode
	}

	t.status = status
	t.failureCode = code
	t.updatedAt = laterOf(t.updatedAt, now)

	return nil
}

func (t *WagerTransaction) IsInternal() bool { return t.kind == WagerOpening }

func (t *WagerTransaction) ResultBalance() (Money, bool) {
	return t.resultBalance, t.hasResultBalance
}

func (t *WagerTransaction) ID() string                    { return t.id }
func (t *WagerTransaction) ProviderID() string            { return t.providerID }
func (t *WagerTransaction) ExternalTransactionID() string { return t.externalTransactionID }
func (t *WagerTransaction) IdempotencyKey() string        { return t.idempotencyKey }
func (t *WagerTransaction) PayloadHash() string           { return t.payloadHash }
func (t *WagerTransaction) WalletID() string              { return t.walletID }
func (t *WagerTransaction) PlayerID() string              { return t.playerID }
func (t *WagerTransaction) RoundID() string               { return t.roundID }
func (t *WagerTransaction) GameID() string                { return t.gameID }
func (t *WagerTransaction) Kind() WagerKind               { return t.kind }
func (t *WagerTransaction) Money() Money                  { return t.money }
func (t *WagerTransaction) ReferenceExternalTransactionID() string {
	return t.referenceExternalTransactionID
}
func (t *WagerTransaction) ReferenceTransactionID() string { return t.referenceTransactionID }
func (t *WagerTransaction) Status() WagerStatus            { return t.status }
func (t *WagerTransaction) FailureCode() FailureCode       { return t.failureCode }
func (t *WagerTransaction) CreatedAt() time.Time           { return t.createdAt }
func (t *WagerTransaction) UpdatedAt() time.Time           { return t.updatedAt }

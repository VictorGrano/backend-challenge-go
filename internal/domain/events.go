package domain

import (
	"encoding/json"
	"errors"
	"time"
)

type EventType string

const (
	EventWagerTransactionProcessed        EventType = "WagerTransactionProcessed"
	EventWagerTransactionRejected         EventType = "WagerTransactionRejected"
	EventWalletBalanceChanged             EventType = "WalletBalanceChanged"
	EventWagerTransactionPendingReference EventType = "WagerTransactionPendingReference"
)

const eventVersion = 1

var ErrInvalidEvent = errors.New("invalid event")

type EventData interface {
	eventType() EventType
}

type EventMeta struct {
	EventID       string
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

type Event struct {
	id            string
	eventType     EventType
	aggregateID   string
	correlationID string
	causationID   string
	occurredAt    time.Time
	version       int
	data          EventData
}

func newEvent(meta EventMeta, aggregateID string, data EventData) (Event, error) {
	if blank(meta.EventID) || blank(meta.CorrelationID) || meta.OccurredAt.IsZero() || blank(aggregateID) {
		return Event{}, ErrInvalidEvent
	}

	return Event{
		id:            meta.EventID,
		eventType:     data.eventType(),
		aggregateID:   aggregateID,
		correlationID: meta.CorrelationID,
		causationID:   meta.CausationID,
		occurredAt:    meta.OccurredAt.UTC(),
		version:       eventVersion,
		data:          data,
	}, nil
}

func (e Event) ID() string            { return e.id }
func (e Event) Type() EventType       { return e.eventType }
func (e Event) AggregateID() string   { return e.aggregateID }
func (e Event) CorrelationID() string { return e.correlationID }
func (e Event) CausationID() string   { return e.causationID }
func (e Event) OccurredAt() time.Time { return e.occurredAt }
func (e Event) Version() int          { return e.version }
func (e Event) Data() EventData       { return e.data }

func (e Event) MarshalJSON() ([]byte, error) {
	if e.data == nil {
		return nil, ErrInvalidEvent
	}

	return json.Marshal(struct {
		EventID       string    `json:"eventId"`
		EventType     EventType `json:"eventType"`
		AggregateID   string    `json:"aggregateId"`
		CorrelationID string    `json:"correlationId"`
		CausationID   string    `json:"causationId,omitempty"`
		OccurredAt    string    `json:"occurredAt"`
		Version       int       `json:"version"`
		Data          EventData `json:"data"`
	}{
		EventID:       e.id,
		EventType:     e.eventType,
		AggregateID:   e.aggregateID,
		CorrelationID: e.correlationID,
		CausationID:   e.causationID,
		OccurredAt:    e.occurredAt.Format(time.RFC3339Nano),
		Version:       e.version,
		Data:          e.data,
	})
}

type WagerTransactionProcessed struct {
	TransactionID                  string    `json:"transactionId"`
	WalletID                       string    `json:"walletId"`
	PlayerID                       string    `json:"playerId"`
	Kind                           WagerKind `json:"kind"`
	Money                          Money     `json:"money"`
	Balance                        Money     `json:"balance"`
	ProviderID                     string    `json:"providerId,omitempty"`
	ExternalTransactionID          string    `json:"externalTransactionId,omitempty"`
	RoundID                        string    `json:"roundId,omitempty"`
	GameID                         string    `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string    `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string    `json:"referenceTransactionId,omitempty"`
}

func (WagerTransactionProcessed) eventType() EventType { return EventWagerTransactionProcessed }

type WagerTransactionRejected struct {
	TransactionID                  string      `json:"transactionId"`
	WalletID                       string      `json:"walletId"`
	PlayerID                       string      `json:"playerId"`
	Kind                           WagerKind   `json:"kind"`
	Money                          Money       `json:"money"`
	FailureCode                    FailureCode `json:"failureCode"`
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string      `json:"referenceTransactionId,omitempty"`
}

func (WagerTransactionRejected) eventType() EventType { return EventWagerTransactionRejected }

type WagerTransactionPendingReference struct {
	TransactionID                  string    `json:"transactionId"`
	WalletID                       string    `json:"walletId"`
	PlayerID                       string    `json:"playerId"`
	Kind                           WagerKind `json:"kind"`
	Money                          Money     `json:"money"`
	ProviderID                     string    `json:"providerId"`
	ExternalTransactionID          string    `json:"externalTransactionId"`
	RoundID                        string    `json:"roundId"`
	GameID                         string    `json:"gameId"`
	ReferenceExternalTransactionID string    `json:"referenceExternalTransactionId"`
}

func (WagerTransactionPendingReference) eventType() EventType {
	return EventWagerTransactionPendingReference
}

type WalletBalanceChanged struct {
	WalletID      string          `json:"walletId"`
	TransactionID string          `json:"transactionId"`
	Direction     LedgerDirection `json:"direction"`
	Money         Money           `json:"money"`
	BalanceBefore Money           `json:"balanceBefore"`
	BalanceAfter  Money           `json:"balanceAfter"`
	WalletVersion int64           `json:"walletVersion"`
}

func (WalletBalanceChanged) eventType() EventType { return EventWalletBalanceChanged }

func NewWagerTransactionProcessedEvent(meta EventMeta, tx *WagerTransaction) (Event, error) {
	if tx == nil || tx.Status() != WagerProcessed {
		return Event{}, ErrInvalidEvent
	}

	balance, ok := tx.ResultBalance()
	if !ok {
		return Event{}, ErrInvalidEvent
	}

	return newEvent(meta, tx.ID(), WagerTransactionProcessed{
		TransactionID:                  tx.ID(),
		WalletID:                       tx.WalletID(),
		PlayerID:                       tx.PlayerID(),
		Kind:                           tx.Kind(),
		Money:                          tx.Money(),
		Balance:                        balance,
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalTransactionID(),
		RoundID:                        tx.RoundID(),
		GameID:                         tx.GameID(),
		ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID(),
		ReferenceTransactionID:         tx.ReferenceTransactionID(),
	})
}

func NewWagerTransactionRejectedEvent(meta EventMeta, tx *WagerTransaction) (Event, error) {
	if tx == nil || tx.Status() != WagerRejected {
		return Event{}, ErrInvalidEvent
	}

	return newEvent(meta, tx.ID(), WagerTransactionRejected{
		TransactionID:                  tx.ID(),
		WalletID:                       tx.WalletID(),
		PlayerID:                       tx.PlayerID(),
		Kind:                           tx.Kind(),
		Money:                          tx.Money(),
		FailureCode:                    tx.FailureCode(),
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalTransactionID(),
		RoundID:                        tx.RoundID(),
		GameID:                         tx.GameID(),
		ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID(),
		ReferenceTransactionID:         tx.ReferenceTransactionID(),
	})
}

func NewWagerTransactionPendingReferenceEvent(meta EventMeta, tx *WagerTransaction) (Event, error) {
	if tx == nil || tx.Status() != WagerPendingReference {
		return Event{}, ErrInvalidEvent
	}

	return newEvent(meta, tx.ID(), WagerTransactionPendingReference{
		TransactionID:                  tx.ID(),
		WalletID:                       tx.WalletID(),
		PlayerID:                       tx.PlayerID(),
		Kind:                           tx.Kind(),
		Money:                          tx.Money(),
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalTransactionID(),
		RoundID:                        tx.RoundID(),
		GameID:                         tx.GameID(),
		ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID(),
	})
}

func NewWalletBalanceChangedEvent(meta EventMeta, wallet *Wallet, entry *WalletLedgerEntry) (Event, error) {
	if wallet == nil || entry == nil ||
		entry.WalletID() != wallet.ID() || entry.BalanceAfter() != wallet.Balance() {
		return Event{}, ErrInvalidEvent
	}

	return newEvent(meta, wallet.ID(), WalletBalanceChanged{
		WalletID:      wallet.ID(),
		TransactionID: entry.TransactionID(),
		Direction:     entry.Direction(),
		Money:         entry.Money(),
		BalanceBefore: entry.BalanceBefore(),
		BalanceAfter:  entry.BalanceAfter(),
		WalletVersion: wallet.Version(),
	})
}

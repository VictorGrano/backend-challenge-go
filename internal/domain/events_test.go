package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func testMeta() EventMeta {
	return EventMeta{
		EventID:       "event-1",
		CorrelationID: "correlation-1",
		OccurredAt:    testNow,
	}
}

func eventJSON(t *testing.T, event Event) map[string]any {
	t.Helper()

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	return decoded
}

func TestWagerTransactionProcessedEvent(t *testing.T) {
	tx := newTestWager(t, WagerBet, "25.00")
	if err := tx.MarkProcessed(mustMoney(t, "975.00"), testNow); err != nil {
		t.Fatal(err)
	}

	meta := testMeta()
	meta.CausationID = "msg-123"

	event, err := NewWagerTransactionProcessedEvent(meta, tx)
	if err != nil {
		t.Fatal(err)
	}

	if event.Type() != EventWagerTransactionProcessed || event.Version() != 1 || event.AggregateID() != "tx-1" {
		t.Errorf("event = %s v%d aggregate %s", event.Type(), event.Version(), event.AggregateID())
	}

	got := eventJSON(t, event)

	envelope := map[string]any{
		"eventId":       "event-1",
		"eventType":     "WagerTransactionProcessed",
		"aggregateId":   "tx-1",
		"correlationId": "correlation-1",
		"causationId":   "msg-123",
		"occurredAt":    "2026-09-08T12:00:00Z",
		"version":       float64(1),
	}
	for key, want := range envelope {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}

	data := got["data"].(map[string]any)
	if data["transactionId"] != "tx-1" || data["providerId"] != "provider-a" || data["kind"] != "BET" {
		t.Errorf("data = %v", data)
	}

	balance := data["balance"].(map[string]any)
	if balance["amount"] != "975.00" || balance["currency"] != "BRL" {
		t.Errorf("balance = %v", balance)
	}
}

func TestEventOmitsBlankCausationID(t *testing.T) {
	tx := newTestWager(t, WagerBet, "25.00")
	if err := tx.MarkProcessed(mustMoney(t, "975.00"), testNow); err != nil {
		t.Fatal(err)
	}

	event, err := NewWagerTransactionProcessedEvent(testMeta(), tx)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := eventJSON(t, event)["causationId"]; ok {
		t.Error("blank causationId must be omitted")
	}
}

func TestEventOccurredAtIsUTC(t *testing.T) {
	tx := newTestWager(t, WagerBet, "25.00")
	if err := tx.MarkProcessed(mustMoney(t, "975.00"), testNow); err != nil {
		t.Fatal(err)
	}

	meta := testMeta()
	meta.OccurredAt = testNow.In(time.FixedZone("BRT", -3*60*60))

	event, err := NewWagerTransactionProcessedEvent(meta, tx)
	if err != nil {
		t.Fatal(err)
	}

	if got := eventJSON(t, event)["occurredAt"]; got != "2026-09-08T12:00:00Z" {
		t.Errorf("occurredAt = %v, want UTC", got)
	}
}

func TestLossProducesProcessedEvent(t *testing.T) {
	tx := newTestWager(t, WagerLoss, "0.00")
	if err := tx.MarkProcessed(mustMoney(t, "975.00"), testNow); err != nil {
		t.Fatal(err)
	}

	event, err := NewWagerTransactionProcessedEvent(testMeta(), tx)
	if err != nil {
		t.Fatal(err)
	}

	data := eventJSON(t, event)["data"].(map[string]any)
	if data["kind"] != "LOSS" || data["money"].(map[string]any)["amount"] != "0.00" {
		t.Errorf("data = %v", data)
	}
}

func TestOpeningEvents(t *testing.T) {
	balance := mustMoney(t, "1000.00")
	zero := mustMoney(t, "0")

	wallet, err := NewWallet("wallet-1", "player-1", balance, testNow)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := NewOpeningTransaction("tx-1", wallet.ID(), wallet.PlayerID(), balance, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkProcessed(wallet.Balance(), testNow); err != nil {
		t.Fatal(err)
	}

	entry, err := NewWalletLedgerEntry("entry-1", wallet.ID(), tx.ID(), LedgerCredit, balance, zero, balance, testNow)
	if err != nil {
		t.Fatal(err)
	}

	processed, err := NewWagerTransactionProcessedEvent(testMeta(), tx)
	if err != nil {
		t.Fatal(err)
	}

	data := eventJSON(t, processed)["data"].(map[string]any)
	if data["kind"] != "OPENING" {
		t.Errorf("kind = %v, want OPENING", data["kind"])
	}
	for _, key := range []string{"providerId", "externalTransactionId", "roundId", "gameId", "referenceExternalTransactionId"} {
		if _, ok := data[key]; ok {
			t.Errorf("opening event must not carry %s", key)
		}
	}

	changed, err := NewWalletBalanceChangedEvent(testMeta(), wallet, entry)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Type() != EventWalletBalanceChanged || changed.AggregateID() != "wallet-1" {
		t.Errorf("event = %s aggregate %s", changed.Type(), changed.AggregateID())
	}

	data = eventJSON(t, changed)["data"].(map[string]any)
	if data["walletVersion"] != float64(1) || data["direction"] != "CREDIT" || data["transactionId"] != "tx-1" {
		t.Errorf("data = %v", data)
	}
	if data["balanceBefore"].(map[string]any)["amount"] != "0.00" ||
		data["balanceAfter"].(map[string]any)["amount"] != "1000.00" {
		t.Errorf("balances = %v -> %v", data["balanceBefore"], data["balanceAfter"])
	}
}

func TestWalletBalanceChangedEvent(t *testing.T) {
	wallet := newTestWallet(t, "100.00")

	entry, err := wallet.Debit("entry-1", "tx-1", mustMoney(t, "80.00"), testNow)
	if err != nil {
		t.Fatal(err)
	}

	event, err := NewWalletBalanceChangedEvent(testMeta(), wallet, entry)
	if err != nil {
		t.Fatal(err)
	}

	data, ok := event.Data().(WalletBalanceChanged)
	if !ok {
		t.Fatalf("data type = %T", event.Data())
	}
	if data.WalletVersion != 2 || data.Direction != LedgerDebit ||
		data.Money != mustMoney(t, "80.00") || data.BalanceAfter != mustMoney(t, "20.00") {
		t.Errorf("data = %+v", data)
	}

	if _, err := wallet.Credit("entry-2", "tx-2", mustMoney(t, "1.00"), testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWalletBalanceChangedEvent(testMeta(), wallet, entry); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("stale entry error = %v, want ErrInvalidEvent", err)
	}
}

func TestWagerTransactionRejectedEvent(t *testing.T) {
	tx := newTestWager(t, WagerBet, "80.00")
	if err := tx.Reject(FailureInsufficientFunds, testNow); err != nil {
		t.Fatal(err)
	}

	event, err := NewWagerTransactionRejectedEvent(testMeta(), tx)
	if err != nil {
		t.Fatal(err)
	}

	data := eventJSON(t, event)["data"].(map[string]any)
	if event.Type() != EventWagerTransactionRejected || data["failureCode"] != "INSUFFICIENT_FUNDS" {
		t.Errorf("event = %s, data = %v", event.Type(), data)
	}
}

func TestWagerTransactionPendingReferenceEvent(t *testing.T) {
	tx := newTestWager(t, WagerRefund, "25.00")
	if err := tx.MarkPendingReference(testNow); err != nil {
		t.Fatal(err)
	}

	event, err := NewWagerTransactionPendingReferenceEvent(testMeta(), tx)
	if err != nil {
		t.Fatal(err)
	}

	data := eventJSON(t, event)["data"].(map[string]any)
	if event.Type() != EventWagerTransactionPendingReference || data["referenceExternalTransactionId"] != "external-0" {
		t.Errorf("event = %s, data = %v", event.Type(), data)
	}
}

func TestEventsRequireMatchingStatus(t *testing.T) {
	pending := newTestWager(t, WagerBet, "25.00")

	if _, err := NewWagerTransactionProcessedEvent(testMeta(), pending); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("processed event error = %v, want ErrInvalidEvent", err)
	}
	if _, err := NewWagerTransactionRejectedEvent(testMeta(), pending); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("rejected event error = %v, want ErrInvalidEvent", err)
	}
	if _, err := NewWagerTransactionPendingReferenceEvent(testMeta(), pending); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("pending reference event error = %v, want ErrInvalidEvent", err)
	}
	if _, err := NewWagerTransactionProcessedEvent(testMeta(), nil); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("nil transaction error = %v, want ErrInvalidEvent", err)
	}

	failed := newTestWager(t, WagerBet, "25.00")
	if err := failed.Fail(FailureInfrastructure, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWagerTransactionRejectedEvent(testMeta(), failed); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("failed transaction error = %v, want ErrInvalidEvent", err)
	}
}

func TestEventRejectsInvalidMeta(t *testing.T) {
	tx := newTestWager(t, WagerBet, "25.00")
	if err := tx.MarkProcessed(mustMoney(t, "975.00"), testNow); err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*EventMeta){
		"blank event id":       func(m *EventMeta) { m.EventID = "" },
		"blank correlation id": func(m *EventMeta) { m.CorrelationID = " " },
		"zero time":            func(m *EventMeta) { m.OccurredAt = time.Time{} },
	}

	for name, mutate := range mutations {
		meta := testMeta()
		mutate(&meta)

		if _, err := NewWagerTransactionProcessedEvent(meta, tx); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s error = %v, want ErrInvalidEvent", name, err)
		}
	}

	if _, err := json.Marshal(Event{}); err == nil {
		t.Error("Marshal of zero value Event expected an error")
	}
}

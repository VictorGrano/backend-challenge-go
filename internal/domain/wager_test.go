package domain

import (
	"errors"
	"testing"
	"time"
)

func wagerParams(t *testing.T, kind WagerKind, amount string) NewWagerTransactionParams {
	t.Helper()

	p := NewWagerTransactionParams{
		ID:                    "tx-1",
		ProviderID:            "provider-a",
		ExternalTransactionID: "external-1",
		IdempotencyKey:        "provider-a:external-1",
		PayloadHash:           "hash-1",
		WalletID:              "wallet-1",
		PlayerID:              "player-1",
		RoundID:               "round-1",
		GameID:                "game-1",
		Kind:                  kind,
		Money:                 mustMoney(t, amount),
		Now:                   testNow,
	}

	if kind.RequiresReference() {
		p.ReferenceExternalTransactionID = "external-0"
	}

	return p
}

func newTestWager(t *testing.T, kind WagerKind, amount string) *WagerTransaction {
	t.Helper()

	tx, err := NewWagerTransaction(wagerParams(t, kind, amount))
	if err != nil {
		t.Fatalf("NewWagerTransaction(%s) returned error: %v", kind, err)
	}

	return tx
}

func TestNewWagerTransaction(t *testing.T) {
	tx := newTestWager(t, WagerBet, "20.00")

	if tx.Status() != WagerPending {
		t.Errorf("status = %s, want %s", tx.Status(), WagerPending)
	}
	if tx.Money() != mustMoney(t, "20.00") || tx.Kind() != WagerBet {
		t.Errorf("tx = %s %s", tx.Kind(), tx.Money())
	}
	if tx.IsInternal() {
		t.Error("external transaction reported as internal")
	}
	if _, ok := tx.ResultBalance(); ok {
		t.Error("pending transaction must not have a result balance")
	}
	if tx.FailureCode() != "" {
		t.Errorf("failure code = %q, want empty", tx.FailureCode())
	}
	if !tx.CreatedAt().Equal(testNow) || !tx.UpdatedAt().Equal(testNow) {
		t.Errorf("timestamps = %v, %v", tx.CreatedAt(), tx.UpdatedAt())
	}
}

func TestWagerAmountPolicy(t *testing.T) {
	tests := []struct {
		kind   WagerKind
		amount string
		ok     bool
	}{
		{WagerBet, "25.00", true},
		{WagerBet, "0", false},
		{WagerWin, "25.00", true},
		{WagerWin, "0", false},
		{WagerLoss, "0.00", true},
		{WagerLoss, "0.01", false},
		{WagerRefund, "25.00", true},
		{WagerRefund, "0", false},
		{WagerRollback, "25.00", true},
		{WagerRollback, "0", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind)+" "+tt.amount, func(t *testing.T) {
			_, err := NewWagerTransaction(wagerParams(t, tt.kind, tt.amount))

			if tt.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidWagerAmount) {
				t.Fatalf("error = %v, want ErrInvalidWagerAmount", err)
			}
		})
	}
}

func TestNewWagerTransactionRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*NewWagerTransactionParams)
		want   error
	}{
		{"unknown kind", func(p *NewWagerTransactionParams) { p.Kind = "UNKNOWN" }, ErrInvalidWagerKind},
		{"opening kind", func(p *NewWagerTransactionParams) { p.Kind = WagerOpening }, ErrInvalidWagerKind},
		{"negative amount", func(p *NewWagerTransactionParams) { p.Money = mustUnits(t, -100, BRL) }, ErrInvalidWagerAmount},
		{"uninitialized money", func(p *NewWagerTransactionParams) { p.Money = Money{} }, ErrInvalidCurrency},
		{"blank id", func(p *NewWagerTransactionParams) { p.ID = "" }, ErrInvalidWagerTransaction},
		{"blank provider", func(p *NewWagerTransactionParams) { p.ProviderID = " " }, ErrInvalidWagerTransaction},
		{"blank external id", func(p *NewWagerTransactionParams) { p.ExternalTransactionID = "" }, ErrInvalidWagerTransaction},
		{"blank idempotency key", func(p *NewWagerTransactionParams) { p.IdempotencyKey = "" }, ErrInvalidWagerTransaction},
		{"blank payload hash", func(p *NewWagerTransactionParams) { p.PayloadHash = "" }, ErrInvalidWagerTransaction},
		{"blank wallet", func(p *NewWagerTransactionParams) { p.WalletID = "" }, ErrInvalidWagerTransaction},
		{"blank player", func(p *NewWagerTransactionParams) { p.PlayerID = "" }, ErrInvalidWagerTransaction},
		{"blank round", func(p *NewWagerTransactionParams) { p.RoundID = "" }, ErrInvalidWagerTransaction},
		{"blank game", func(p *NewWagerTransactionParams) { p.GameID = "" }, ErrInvalidWagerTransaction},
		{"zero time", func(p *NewWagerTransactionParams) { p.Now = time.Time{} }, ErrInvalidWagerTransaction},
		{"self reference", func(p *NewWagerTransactionParams) { p.ReferenceExternalTransactionID = "external-1" }, ErrInvalidWagerTransaction},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := wagerParams(t, WagerBet, "20.00")
			tt.mutate(&p)

			if _, err := NewWagerTransaction(p); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestReversalsRequireReference(t *testing.T) {
	for _, kind := range []WagerKind{WagerRefund, WagerRollback} {
		p := wagerParams(t, kind, "20.00")
		p.ReferenceExternalTransactionID = ""

		if _, err := NewWagerTransaction(p); !errors.Is(err, ErrReferenceRequired) {
			t.Errorf("%s without reference error = %v, want ErrReferenceRequired", kind, err)
		}
	}

	p := wagerParams(t, WagerWin, "20.00")
	p.ReferenceExternalTransactionID = "external-0"

	tx, err := NewWagerTransaction(p)
	if err != nil {
		t.Fatal(err)
	}
	if tx.ReferenceExternalTransactionID() != "external-0" {
		t.Errorf("reference = %q", tx.ReferenceExternalTransactionID())
	}
}

func TestWagerMarkProcessed(t *testing.T) {
	tx := newTestWager(t, WagerBet, "25.00")
	later := testNow.Add(time.Second)

	if err := tx.MarkProcessed(mustMoney(t, "975.00"), later); err != nil {
		t.Fatal(err)
	}

	if tx.Status() != WagerProcessed {
		t.Errorf("status = %s, want %s", tx.Status(), WagerProcessed)
	}
	if balance, ok := tx.ResultBalance(); !ok || balance != mustMoney(t, "975.00") {
		t.Errorf("result balance = %s, %v", balance, ok)
	}
	if !tx.UpdatedAt().Equal(later) {
		t.Errorf("updatedAt = %v, want %v", tx.UpdatedAt(), later)
	}
}

func TestWagerMarkProcessedRejectsInvalidBalance(t *testing.T) {
	tests := []struct {
		name    string
		balance Money
		want    error
	}{
		{"negative", mustUnits(t, -1, BRL), ErrInvalidWagerTransaction},
		{"other currency", mustUnits(t, 100, USD), ErrCurrencyMismatch},
		{"uninitialized", Money{}, ErrInvalidCurrency},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestWager(t, WagerBet, "25.00")

			if err := tx.MarkProcessed(tt.balance, testNow); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if tx.Status() != WagerPending {
				t.Errorf("status changed to %s", tx.Status())
			}
		})
	}
}

func TestWagerLossIsProcessedWithZeroAmount(t *testing.T) {
	tx := newTestWager(t, WagerLoss, "0.00")

	if err := tx.MarkProcessed(mustMoney(t, "975.00"), testNow); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != WagerProcessed || !tx.Money().IsZero() {
		t.Errorf("tx = %s %s", tx.Status(), tx.Money())
	}
}

func TestWagerReject(t *testing.T) {
	tx := newTestWager(t, WagerBet, "80.00")

	if err := tx.Reject("", testNow); !errors.Is(err, ErrInvalidFailureCode) {
		t.Fatalf("blank code error = %v, want ErrInvalidFailureCode", err)
	}

	if err := tx.Reject(FailureInsufficientFunds, testNow); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != WagerRejected || tx.FailureCode() != FailureInsufficientFunds {
		t.Errorf("tx = %s %s", tx.Status(), tx.FailureCode())
	}
	if _, ok := tx.ResultBalance(); ok {
		t.Error("rejected transaction must not have a result balance")
	}
}

func TestWagerFail(t *testing.T) {
	tx := newTestWager(t, WagerBet, "80.00")

	if err := tx.Fail(FailureInfrastructure, testNow); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != WagerFailed || tx.FailureCode() != FailureInfrastructure {
		t.Errorf("tx = %s %s", tx.Status(), tx.FailureCode())
	}
}

func TestWagerPendingReferenceFlow(t *testing.T) {
	tx := newTestWager(t, WagerRefund, "25.00")

	if err := tx.MarkPendingReference(testNow); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != WagerPendingReference {
		t.Fatalf("status = %s, want %s", tx.Status(), WagerPendingReference)
	}

	if err := tx.MarkPendingReference(testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("second MarkPendingReference error = %v, want ErrInvalidTransition", err)
	}

	if err := tx.MarkProcessed(mustMoney(t, "100.00"), testNow); !errors.Is(err, ErrReferenceRequired) {
		t.Errorf("MarkProcessed error = %v, want ErrReferenceRequired", err)
	}

	if err := tx.ResolveReference("tx-0", testNow); err != nil {
		t.Fatal(err)
	}
	if err := tx.ResolveReference("tx-9", testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("changing reference error = %v, want ErrInvalidTransition", err)
	}

	if err := tx.MarkProcessed(mustMoney(t, "100.00"), testNow); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != WagerProcessed || tx.ReferenceTransactionID() != "tx-0" {
		t.Errorf("tx = %s ref %q", tx.Status(), tx.ReferenceTransactionID())
	}
}

func TestWagerPendingReferenceExpires(t *testing.T) {
	tx := newTestWager(t, WagerRollback, "25.00")

	if err := tx.MarkPendingReference(testNow); err != nil {
		t.Fatal(err)
	}
	if err := tx.Reject(FailureReferenceNotFound, testNow); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != WagerRejected || tx.FailureCode() != FailureReferenceNotFound {
		t.Errorf("tx = %s %s", tx.Status(), tx.FailureCode())
	}
}

func TestWagerReferenceRules(t *testing.T) {
	bet := newTestWager(t, WagerBet, "25.00")

	if err := bet.MarkPendingReference(testNow); !errors.Is(err, ErrReferenceRequired) {
		t.Errorf("MarkPendingReference without reference error = %v, want ErrReferenceRequired", err)
	}
	if err := bet.ResolveReference("tx-0", testNow); !errors.Is(err, ErrReferenceRequired) {
		t.Errorf("ResolveReference without reference error = %v, want ErrReferenceRequired", err)
	}

	refund := newTestWager(t, WagerRefund, "25.00")

	if err := refund.ResolveReference("", testNow); !errors.Is(err, ErrInvalidWagerTransaction) {
		t.Errorf("blank reference error = %v, want ErrInvalidWagerTransaction", err)
	}
	if err := refund.ResolveReference("tx-1", testNow); !errors.Is(err, ErrInvalidWagerTransaction) {
		t.Errorf("self reference error = %v, want ErrInvalidWagerTransaction", err)
	}
}

func TestWagerTerminalStatesRejectTransitions(t *testing.T) {
	finish := map[WagerStatus]func(*WagerTransaction) error{
		WagerProcessed: func(tx *WagerTransaction) error { return tx.MarkProcessed(mustMoney(t, "10.00"), testNow) },
		WagerRejected:  func(tx *WagerTransaction) error { return tx.Reject(FailureInsufficientFunds, testNow) },
		WagerFailed:    func(tx *WagerTransaction) error { return tx.Fail(FailureInfrastructure, testNow) },
	}

	for status, do := range finish {
		t.Run(string(status), func(t *testing.T) {
			p := wagerParams(t, WagerWin, "25.00")
			p.ReferenceExternalTransactionID = "external-0"

			tx, err := NewWagerTransaction(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := do(tx); err != nil {
				t.Fatal(err)
			}

			attempts := map[string]error{
				"MarkPendingReference": tx.MarkPendingReference(testNow),
				"ResolveReference":     tx.ResolveReference("tx-0", testNow),
				"MarkProcessed":        tx.MarkProcessed(mustMoney(t, "10.00"), testNow),
				"Reject":               tx.Reject(FailureInsufficientFunds, testNow),
				"Fail":                 tx.Fail(FailureInfrastructure, testNow),
			}

			for name, err := range attempts {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("%s error = %v, want ErrInvalidTransition", name, err)
				}
			}
			if tx.Status() != status {
				t.Errorf("status = %s, want %s", tx.Status(), status)
			}
		})
	}
}

func TestWagerTransitionsRejectZeroTime(t *testing.T) {
	tx := newTestWager(t, WagerRefund, "25.00")

	attempts := map[string]error{
		"MarkPendingReference": tx.MarkPendingReference(time.Time{}),
		"ResolveReference":     tx.ResolveReference("tx-0", time.Time{}),
		"MarkProcessed":        tx.MarkProcessed(mustMoney(t, "10.00"), time.Time{}),
		"Reject":               tx.Reject(FailureInsufficientFunds, time.Time{}),
		"Fail":                 tx.Fail(FailureInfrastructure, time.Time{}),
	}

	for name, err := range attempts {
		if !errors.Is(err, ErrInvalidWagerTransaction) {
			t.Errorf("%s error = %v, want ErrInvalidWagerTransaction", name, err)
		}
	}
	if tx.Status() != WagerPending {
		t.Errorf("status = %s, want %s", tx.Status(), WagerPending)
	}
}

func TestNewOpeningTransaction(t *testing.T) {
	tx, err := NewOpeningTransaction("tx-1", "wallet-1", "player-1", mustMoney(t, "1000.00"), testNow)
	if err != nil {
		t.Fatal(err)
	}

	if tx.Kind() != WagerOpening || !tx.IsInternal() || tx.Status() != WagerPending {
		t.Errorf("tx = %s %s", tx.Kind(), tx.Status())
	}

	if tx.ProviderID() != "" || tx.ExternalTransactionID() != "" || tx.IdempotencyKey() != "" ||
		tx.PayloadHash() != "" || tx.RoundID() != "" || tx.GameID() != "" ||
		tx.ReferenceExternalTransactionID() != "" {
		t.Error("opening transaction must not carry external metadata")
	}

	if err := tx.MarkProcessed(mustMoney(t, "1000.00"), testNow); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != WagerProcessed {
		t.Errorf("status = %s, want %s", tx.Status(), WagerProcessed)
	}
}

func TestNewOpeningTransactionRejectsInvalidInput(t *testing.T) {
	money := mustMoney(t, "1000.00")

	tests := []struct {
		name     string
		id       string
		walletID string
		playerID string
		money    Money
		now      time.Time
		want     error
	}{
		{"zero amount", "tx-1", "wallet-1", "player-1", mustMoney(t, "0"), testNow, ErrInvalidWagerAmount},
		{"negative amount", "tx-1", "wallet-1", "player-1", mustUnits(t, -1, BRL), testNow, ErrInvalidWagerAmount},
		{"uninitialized money", "tx-1", "wallet-1", "player-1", Money{}, testNow, ErrInvalidCurrency},
		{"blank id", "", "wallet-1", "player-1", money, testNow, ErrInvalidWagerTransaction},
		{"blank wallet", "tx-1", "", "player-1", money, testNow, ErrInvalidWagerTransaction},
		{"blank player", "tx-1", "wallet-1", "", money, testNow, ErrInvalidWagerTransaction},
		{"zero time", "tx-1", "wallet-1", "player-1", money, time.Time{}, ErrInvalidWagerTransaction},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewOpeningTransaction(tt.id, tt.walletID, tt.playerID, tt.money, tt.now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func wagerState(t *testing.T) WagerTransactionState {
	t.Helper()

	return WagerTransactionState{
		ID:                    "tx-1",
		ProviderID:            "provider-a",
		ExternalTransactionID: "external-1",
		IdempotencyKey:        "provider-a:external-1",
		PayloadHash:           "hash-1",
		WalletID:              "wallet-1",
		PlayerID:              "player-1",
		RoundID:               "round-1",
		GameID:                "game-1",
		Kind:                  WagerBet,
		Money:                 mustMoney(t, "25.00"),
		Status:                WagerPending,
		CreatedAt:             testNow,
		UpdatedAt:             testNow,
	}
}

func TestRehydrateWagerTransaction(t *testing.T) {
	balance := mustMoney(t, "975.00")

	s := wagerState(t)
	s.Status = WagerProcessed
	s.ResultBalance = &balance
	s.UpdatedAt = testNow.Add(time.Second)

	tx, err := RehydrateWagerTransaction(s)
	if err != nil {
		t.Fatal(err)
	}

	if tx.Status() != WagerProcessed || tx.Kind() != WagerBet {
		t.Errorf("tx = %s %s", tx.Kind(), tx.Status())
	}
	if got, ok := tx.ResultBalance(); !ok || got != balance {
		t.Errorf("result balance = %s, %v", got, ok)
	}
	if !tx.UpdatedAt().Equal(s.UpdatedAt) {
		t.Errorf("updatedAt = %v, want %v", tx.UpdatedAt(), s.UpdatedAt)
	}

	if err := tx.MarkProcessed(balance, testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("MarkProcessed error = %v, want ErrInvalidTransition", err)
	}
}

func TestRehydrateWagerTransactionRejectsInvalidState(t *testing.T) {
	balance := mustMoney(t, "975.00")

	tests := []struct {
		name   string
		mutate func(*WagerTransactionState)
		want   error
	}{
		{"unknown status", func(s *WagerTransactionState) { s.Status = "DONE" }, ErrInvalidWagerStatus},
		{"unknown kind", func(s *WagerTransactionState) { s.Kind = "UNKNOWN" }, ErrInvalidWagerKind},
		{"processed without balance", func(s *WagerTransactionState) { s.Status = WagerProcessed }, ErrInvalidWagerTransaction},
		{"rejected without code", func(s *WagerTransactionState) { s.Status = WagerRejected }, ErrInvalidWagerTransaction},
		{"failed without code", func(s *WagerTransactionState) { s.Status = WagerFailed }, ErrInvalidWagerTransaction},
		{"pending with code", func(s *WagerTransactionState) { s.FailureCode = FailureInsufficientFunds }, ErrInvalidWagerTransaction},
		{"pending with balance", func(s *WagerTransactionState) { s.ResultBalance = &balance }, ErrInvalidWagerTransaction},
		{"pending reference without reference", func(s *WagerTransactionState) { s.Status = WagerPendingReference }, ErrInvalidWagerTransaction},
		{"resolved reference without external", func(s *WagerTransactionState) { s.ReferenceTransactionID = "tx-0" }, ErrInvalidWagerTransaction},
		{"updated before created", func(s *WagerTransactionState) { s.UpdatedAt = testNow.Add(-time.Second) }, ErrInvalidWagerTransaction},
		{"zero created", func(s *WagerTransactionState) { s.CreatedAt = time.Time{} }, ErrInvalidWagerTransaction},
		{"opening with provider", func(s *WagerTransactionState) { s.Kind = WagerOpening }, ErrInvalidWagerTransaction},
		{"loss with amount", func(s *WagerTransactionState) { s.Kind = WagerLoss }, ErrInvalidWagerAmount},
		{"refund without reference", func(s *WagerTransactionState) { s.Kind = WagerRefund }, ErrReferenceRequired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := wagerState(t)
			tt.mutate(&s)

			if _, err := RehydrateWagerTransaction(s); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUpdatedAtNeverGoesBackwards(t *testing.T) {
	earlier := testNow.Add(-time.Minute)

	tx := newTestWager(t, WagerRefund, "25.00")
	if err := tx.MarkPendingReference(earlier); err != nil {
		t.Fatal(err)
	}
	if !tx.UpdatedAt().Equal(testNow) {
		t.Errorf("transaction updatedAt = %v, want %v", tx.UpdatedAt(), testNow)
	}

	wallet := newTestWallet(t, "100.00")
	if _, err := wallet.Debit("entry-1", "tx-1", mustMoney(t, "10.00"), earlier); err != nil {
		t.Fatal(err)
	}
	if !wallet.UpdatedAt().Equal(testNow) {
		t.Errorf("wallet updatedAt = %v, want %v", wallet.UpdatedAt(), testNow)
	}

	if _, err := RehydrateWallet(wallet.ID(), wallet.PlayerID(), wallet.Balance(), wallet.Version(),
		wallet.CreatedAt(), wallet.UpdatedAt()); err != nil {
		t.Errorf("rehydrate: %v", err)
	}
}

//go:build integration

package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
)

const defaultTestDSN = "postgres://jungle:devpassword@localhost:5432/jungle_challenge?sslmode=disable"

type env struct {
	db      *DB
	wallets *WalletRepository
	txs     *WagerTransactionRepository
	ledger  *LedgerRepository
	inbox   *InboxRepository
	outbox  *OutboxRepository
}

func newEnv(t *testing.T) *env {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	return &env{
		db:      db,
		wallets: NewWalletRepository(db),
		txs:     NewWagerTransactionRepository(db),
		ledger:  NewLedgerRepository(db),
		inbox:   NewInboxRepository(db),
		outbox:  NewOutboxRepository(db),
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func brl(t *testing.T, value string) domain.Money {
	t.Helper()

	m, err := domain.ParseMoney(value, domain.BRL)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

func (e *env) openWallet(t *testing.T, balance string) *domain.Wallet {
	t.Helper()

	ctx := testContext(t)
	at := now()
	money := brl(t, balance)

	wallet, err := domain.NewWallet(uuid.NewString(), uuid.NewString(), money, at)
	if err != nil {
		t.Fatal(err)
	}

	opening, err := domain.NewOpeningTransaction(uuid.NewString(), wallet.ID(), wallet.PlayerID(), money, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := opening.MarkProcessed(money, at); err != nil {
		t.Fatal(err)
	}

	entry, err := domain.NewWalletLedgerEntry(
		uuid.NewString(), wallet.ID(), opening.ID(), domain.LedgerCredit, money, brl(t, "0"), money, at,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = e.db.WithinTx(ctx, func(ctx context.Context) error {
		if err := e.wallets.Insert(ctx, wallet); err != nil {
			return err
		}
		if err := e.txs.Insert(ctx, opening, ReferenceSchedule{}); err != nil {
			return err
		}
		return e.ledger.Insert(ctx, entry)
	})
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}

	return wallet
}

func newExternal(
	t *testing.T,
	wallet *domain.Wallet,
	providerID, externalID string,
	kind domain.WagerKind,
	amount string,
	reference string,
) *domain.WagerTransaction {
	t.Helper()

	tx, err := domain.NewWagerTransaction(domain.NewWagerTransactionParams{
		ID:                             uuid.NewString(),
		ProviderID:                     providerID,
		ExternalTransactionID:          externalID,
		IdempotencyKey:                 providerID + ":" + externalID,
		PayloadHash:                    "hash",
		WalletID:                       wallet.ID(),
		PlayerID:                       wallet.PlayerID(),
		RoundID:                        "round-1",
		GameID:                         "game-1",
		Kind:                           kind,
		Money:                          brl(t, amount),
		ReferenceExternalTransactionID: reference,
		Now:                            now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	return tx
}

func (e *env) bet(ctx context.Context, tx *domain.WagerTransaction) (domain.WagerStatus, error) {
	err := e.db.WithinTx(ctx, func(ctx context.Context) error {
		wallet, err := e.wallets.GetForUpdate(ctx, tx.WalletID())
		if err != nil {
			return err
		}

		at := now()

		entry, err := wallet.Debit(uuid.NewString(), tx.ID(), tx.Money(), at)
		if errors.Is(err, domain.ErrInsufficientFunds) {
			if err := tx.Reject(domain.FailureInsufficientFunds, at); err != nil {
				return err
			}
			return e.txs.Insert(ctx, tx, ReferenceSchedule{})
		}
		if err != nil {
			return err
		}

		if err := tx.MarkProcessed(wallet.Balance(), at); err != nil {
			return err
		}
		if err := e.txs.Insert(ctx, tx, ReferenceSchedule{}); err != nil {
			return err
		}
		if err := e.ledger.Insert(ctx, entry); err != nil {
			return err
		}
		return e.wallets.UpdateBalance(ctx, wallet)
	})

	return tx.Status(), err
}

func (e *env) assertReconciled(t *testing.T, walletID string, wantBalance string, wantEntries int64) {
	t.Helper()

	err := e.db.WithinSnapshot(testContext(t), func(ctx context.Context) error {
		wallet, err := e.wallets.Get(ctx, walletID)
		if err != nil {
			return err
		}

		totals, err := e.ledger.Totals(ctx, walletID)
		if err != nil {
			return err
		}

		if wallet.Balance() != brl(t, wantBalance) {
			t.Errorf("stored balance = %s, want %s", wallet.Balance(), wantBalance)
		}
		if totals.Balance != wallet.Balance().Amount() {
			t.Errorf("ledger balance = %d, stored = %d", totals.Balance, wallet.Balance().Amount())
		}
		if totals.Entries != wantEntries {
			t.Errorf("ledger entries = %d, want %d", totals.Entries, wantEntries)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) mustFail(t *testing.T, what, sql string, args ...any) {
	t.Helper()

	_, err := e.db.pool.Exec(testContext(t), sql, args...)

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !strings.HasPrefix(pgErr.Code, "23") {
		t.Errorf("%s: error = %v, want an integrity violation", what, err)
	}
}

func TestWalletRoundTrip(t *testing.T) {
	e := newEnv(t)
	created := e.openWallet(t, "1000.00")

	got, err := e.wallets.Get(testContext(t), created.ID())
	if err != nil {
		t.Fatal(err)
	}

	if got.ID() != created.ID() || got.PlayerID() != created.PlayerID() ||
		got.Balance() != created.Balance() || got.Version() != 1 ||
		!got.CreatedAt().Equal(created.CreatedAt()) {
		t.Errorf("wallet = %+v, want %+v", got, created)
	}

	if _, err := e.wallets.Get(testContext(t), uuid.NewString()); !errors.Is(err, domain.ErrWalletNotFound) {
		t.Errorf("missing wallet error = %v, want ErrWalletNotFound", err)
	}

	e.assertReconciled(t, created.ID(), "1000.00", 1)
}

func TestWalletUniquePerPlayerAndCurrency(t *testing.T) {
	e := newEnv(t)
	first := e.openWallet(t, "10.00")

	second, err := domain.NewWallet(uuid.NewString(), first.PlayerID(), brl(t, "0"), now())
	if err != nil {
		t.Fatal(err)
	}

	if err := e.wallets.Insert(testContext(t), second); !errors.Is(err, domain.ErrWalletAlreadyExists) {
		t.Fatalf("error = %v, want ErrWalletAlreadyExists", err)
	}
}

func TestOpeningCannotBeDuplicated(t *testing.T) {
	e := newEnv(t)
	wallet := e.openWallet(t, "10.00")
	at := now()

	again, err := domain.NewOpeningTransaction(uuid.NewString(), wallet.ID(), wallet.PlayerID(), brl(t, "10.00"), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.MarkProcessed(brl(t, "20.00"), at); err != nil {
		t.Fatal(err)
	}

	err = e.txs.Insert(testContext(t), again, ReferenceSchedule{})
	if !errors.Is(err, domain.ErrOpeningAlreadyExists) {
		t.Fatalf("error = %v, want ErrOpeningAlreadyExists", err)
	}
}

func TestSchemaProtectsFinancialInvariants(t *testing.T) {
	e := newEnv(t)
	wallet := e.openWallet(t, "100.00")
	ctx := testContext(t)

	provider := "provider-" + uuid.NewString()
	bet := newExternal(t, wallet, provider, "bet-1", domain.WagerBet, "80.00", "")
	if status, err := e.bet(ctx, bet); err != nil || status != domain.WagerProcessed {
		t.Fatalf("bet = %s, %v", status, err)
	}

	e.mustFail(t, "negative balance",
		`UPDATE wallets SET balance = -1 WHERE id = $1`, wallet.ID())
	e.mustFail(t, "ledger update",
		`UPDATE wallet_ledger_entries SET amount = 1 WHERE wallet_id = $1`, wallet.ID())
	e.mustFail(t, "ledger delete",
		`DELETE FROM wallet_ledger_entries WHERE wallet_id = $1`, wallet.ID())
	e.mustFail(t, "ledger truncate",
		`TRUNCATE wallet_ledger_entries`)
	e.mustFail(t, "terminal transaction update",
		`UPDATE wager_transactions SET status = 'PENDING', result_balance = NULL WHERE id = $1`, bet.ID())
	e.mustFail(t, "transaction delete",
		`DELETE FROM wager_transactions WHERE id = $1`, bet.ID())
	e.mustFail(t, "ledger entry breaking the balance invariant", `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount, currency, balance_before, balance_after, created_at)
		SELECT gen_random_uuid(), wallet_id, id, 'CREDIT', 10, 'BRL', 0, 999, now()
		  FROM wager_transactions WHERE id = $1`, bet.ID())
	e.mustFail(t, "second ledger entry for the same transaction", `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount, currency, balance_before, balance_after, created_at)
		SELECT gen_random_uuid(), wallet_id, id, 'CREDIT', 10, 'BRL', 0, 10, now()
		  FROM wager_transactions WHERE id = $1`, bet.ID())
	e.mustFail(t, "external transaction without provider", `
		INSERT INTO wager_transactions (id, kind, status, wallet_id, player_id, amount, currency, created_at, updated_at)
		VALUES (gen_random_uuid(), 'BET', 'PENDING', $1, $2, 100, 'BRL', now(), now())`,
		wallet.ID(), wallet.PlayerID())
	e.mustFail(t, "loss with amount", `
		INSERT INTO wager_transactions (id, kind, status, wallet_id, player_id, amount, currency,
			provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
			created_at, updated_at)
		VALUES (gen_random_uuid(), 'LOSS', 'PENDING', $1, $2, 100, 'BRL', $3, 'loss-1', 'k', 'h', 'r', 'g', now(), now())`,
		wallet.ID(), wallet.PlayerID(), provider)

	e.assertReconciled(t, wallet.ID(), "20.00", 2)
}

func TestTwoConcurrentBetsOnTheSameWallet(t *testing.T) {
	setup := newEnv(t)
	wallet := setup.openWallet(t, "100.00")
	provider := "provider-" + uuid.NewString()

	instances := []*env{newEnv(t), newEnv(t)}
	bets := []*domain.WagerTransaction{
		newExternal(t, wallet, provider, "bet-1", domain.WagerBet, "80.00", ""),
		newExternal(t, wallet, provider, "bet-2", domain.WagerBet, "80.00", ""),
	}

	ctx := testContext(t)
	start := make(chan struct{})
	statuses := make([]domain.WagerStatus, len(bets))
	errs := make([]error, len(bets))

	var wg sync.WaitGroup
	for i := range bets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses[i], errs[i] = instances[i].bet(ctx, bets[i])
		}()
	}
	close(start)
	wg.Wait()

	counts := map[domain.WagerStatus]int{}
	for i := range bets {
		if errs[i] != nil {
			t.Fatalf("bet %d: %v", i, errs[i])
		}
		counts[statuses[i]]++
	}

	if counts[domain.WagerProcessed] != 1 || counts[domain.WagerRejected] != 1 {
		t.Fatalf("statuses = %v, want one PROCESSED and one REJECTED", statuses)
	}

	for _, bet := range bets {
		stored, err := setup.txs.Get(ctx, bet.ID())
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status() == domain.WagerRejected && stored.FailureCode() != domain.FailureInsufficientFunds {
			t.Errorf("failure code = %s", stored.FailureCode())
		}
	}

	setup.assertReconciled(t, wallet.ID(), "20.00", 2)

	after, err := setup.wallets.Get(ctx, wallet.ID())
	if err != nil {
		t.Fatal(err)
	}
	if after.Version() != 2 {
		t.Errorf("version = %d, want 2", after.Version())
	}
}

func TestSameBetFiftyTimesDebitsOnce(t *testing.T) {
	setup := newEnv(t)
	wallet := setup.openWallet(t, "100.00")
	provider := "provider-" + uuid.NewString()
	instances := []*env{newEnv(t), newEnv(t), newEnv(t)}

	const attempts = 50

	ctx := testContext(t)
	start := make(chan struct{})
	errs := make([]error, attempts)

	var wg sync.WaitGroup
	for i := range attempts {
		bet := newExternal(t, wallet, provider, "bet-1", domain.WagerBet, "25.00", "")

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = instances[i%len(instances)].bet(ctx, bet)
		}()
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrDuplicateTransaction), errors.Is(err, domain.ErrDuplicateIdempotencyKey):
		default:
			t.Errorf("attempt %d: unexpected error %v", i, err)
		}
	}

	if succeeded != 1 {
		t.Fatalf("succeeded = %d, want 1", succeeded)
	}

	stored, err := setup.txs.GetByExternalID(ctx, provider, "bet-1")
	if err != nil {
		t.Fatal(err)
	}
	if balance, ok := stored.ResultBalance(); !ok || balance != brl(t, "75.00") {
		t.Errorf("result balance = %s, %v", balance, ok)
	}

	byKey, err := setup.txs.GetByIdempotencyKey(ctx, provider, provider+":bet-1")
	if err != nil || byKey.ID() != stored.ID() {
		t.Errorf("lookup by idempotency key = %v, %v", byKey, err)
	}

	setup.assertReconciled(t, wallet.ID(), "75.00", 2)
}

func TestIndependentWalletsDoNotBlockEachOther(t *testing.T) {
	e := newEnv(t)
	walletA := e.openWallet(t, "100.00")
	walletB := e.openWallet(t, "100.00")
	provider := "provider-" + uuid.NewString()

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- e.db.WithinTx(context.Background(), func(ctx context.Context) error {
			if _, err := e.wallets.GetForUpdate(ctx, walletA.ID()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()

	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("lock holder finished early: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := e.bet(ctx, newExternal(t, walletB, provider, "bet-b", domain.WagerBet, "10.00", ""))
	if err != nil || status != domain.WagerProcessed {
		t.Errorf("bet on wallet B = %s, %v", status, err)
	}

	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelBlocked()

	if _, err := e.bet(blockedCtx, newExternal(t, walletA, provider, "bet-a", domain.WagerBet, "10.00", "")); err == nil {
		t.Error("bet on locked wallet A should have timed out")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	e.assertReconciled(t, walletA.ID(), "100.00", 1)
	e.assertReconciled(t, walletB.ID(), "90.00", 2)
}

func TestUpdateBalanceDetectsStaleVersion(t *testing.T) {
	e := newEnv(t)
	wallet := e.openWallet(t, "100.00")
	ctx := testContext(t)
	provider := "provider-" + uuid.NewString()

	stale, err := e.wallets.Get(ctx, wallet.ID())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.bet(ctx, newExternal(t, wallet, provider, "bet-1", domain.WagerBet, "10.00", "")); err != nil {
		t.Fatal(err)
	}

	if _, err := stale.Debit(uuid.NewString(), uuid.NewString(), brl(t, "10.00"), now()); err != nil {
		t.Fatal(err)
	}
	if err := e.wallets.UpdateBalance(ctx, stale); !errors.Is(err, domain.ErrConcurrentUpdate) {
		t.Fatalf("error = %v, want ErrConcurrentUpdate", err)
	}

	e.assertReconciled(t, wallet.ID(), "90.00", 2)
}

func TestReferenceAcceptsASingleReversal(t *testing.T) {
	e := newEnv(t)
	wallet := e.openWallet(t, "100.00")
	ctx := testContext(t)
	provider := "provider-" + uuid.NewString()

	bet := newExternal(t, wallet, provider, "bet-1", domain.WagerBet, "25.00", "")
	if _, err := e.bet(ctx, bet); err != nil {
		t.Fatal(err)
	}

	reverse := func(externalID string, kind domain.WagerKind) error {
		tx := newExternal(t, wallet, provider, externalID, kind, "25.00", "bet-1")
		at := now()

		reference, err := e.txs.GetByExternalID(ctx, provider, tx.ReferenceExternalTransactionID())
		if err != nil {
			return err
		}
		if err := tx.ResolveReference(reference.ID(), at); err != nil {
			return err
		}
		if err := tx.MarkProcessed(brl(t, "100.00"), at); err != nil {
			return err
		}

		return e.txs.Insert(ctx, tx, ReferenceSchedule{})
	}

	if err := reverse("refund-1", domain.WagerRefund); err != nil {
		t.Fatalf("first reversal: %v", err)
	}
	if err := reverse("refund-2", domain.WagerRefund); !errors.Is(err, domain.ErrReferenceAlreadyReversed) {
		t.Errorf("second refund error = %v, want ErrReferenceAlreadyReversed", err)
	}
	if err := reverse("rollback-1", domain.WagerRollback); !errors.Is(err, domain.ErrReferenceAlreadyReversed) {
		t.Errorf("rollback after refund error = %v, want ErrReferenceAlreadyReversed", err)
	}
}

func TestPendingReferenceIsClaimedAndResolved(t *testing.T) {
	e := newEnv(t)
	wallet := e.openWallet(t, "100.00")
	ctx := testContext(t)
	provider := "provider-" + uuid.NewString()
	at := now()

	refund := newExternal(t, wallet, provider, "refund-1", domain.WagerRefund, "25.00", "bet-1")
	if err := refund.MarkPendingReference(at); err != nil {
		t.Fatal(err)
	}
	if err := e.txs.Insert(ctx, refund, ReferenceSchedule{NextAttemptAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}

	find := func(pending []PendingReference) *PendingReference {
		for i := range pending {
			if pending[i].Transaction.ID() == refund.ID() {
				return &pending[i]
			}
		}
		return nil
	}

	err := e.db.WithinTx(ctx, func(ctx context.Context) error {
		early, err := e.txs.ClaimDuePendingReferences(ctx, at, 1000)
		if err != nil {
			return err
		}
		if find(early) != nil {
			t.Error("transaction claimed before its next attempt")
		}

		due, err := e.txs.ClaimDuePendingReferences(ctx, at.Add(2*time.Minute), 1000)
		if err != nil {
			return err
		}

		claimed := find(due)
		if claimed == nil {
			t.Fatal("due transaction was not claimed")
		}
		if claimed.Attempts != 0 || claimed.Transaction.Status() != domain.WagerPendingReference {
			t.Errorf("claimed = %d attempts, %s", claimed.Attempts, claimed.Transaction.Status())
		}

		if err := claimed.Transaction.Reject(domain.FailureReferenceNotFound, now()); err != nil {
			return err
		}
		return e.txs.Update(ctx, claimed.Transaction, ReferenceSchedule{Attempts: claimed.Attempts + 1})
	})
	if err != nil {
		t.Fatal(err)
	}

	stored, err := e.txs.Get(ctx, refund.ID())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status() != domain.WagerRejected || stored.FailureCode() != domain.FailureReferenceNotFound {
		t.Errorf("stored = %s %s", stored.Status(), stored.FailureCode())
	}

	e.assertReconciled(t, wallet.ID(), "100.00", 1)
}

func TestInboxDeduplicatesRedelivery(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)
	messageID := "msg-" + uuid.NewString()
	at := now()

	err := e.db.WithinTx(ctx, func(ctx context.Context) error {
		msg, inserted, err := e.inbox.Receive(ctx, "wager-consumer", messageID, "hash-1", at)
		if err != nil {
			return err
		}
		if !inserted || msg.Completed() {
			t.Errorf("first delivery: inserted = %v, completed = %v", inserted, msg.Completed())
		}
		return e.inbox.Complete(ctx, "wager-consumer", messageID, "", at)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = e.db.WithinTx(ctx, func(ctx context.Context) error {
		msg, inserted, err := e.inbox.Receive(ctx, "wager-consumer", messageID, "hash-2", now())
		if err != nil {
			return err
		}
		if inserted || !msg.Completed() {
			t.Errorf("redelivery: inserted = %v, completed = %v", inserted, msg.Completed())
		}
		if msg.PayloadHash != "hash-1" {
			t.Errorf("payload hash = %q, want hash-1", msg.PayloadHash)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = e.db.WithinTx(ctx, func(ctx context.Context) error {
		_, inserted, err := e.inbox.Receive(ctx, "other-consumer", messageID, "hash-1", now())
		if !inserted {
			t.Error("another consumer should register the message")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInboxRollsBackWithTheTransaction(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)
	messageID := "msg-" + uuid.NewString()
	boom := errors.New("boom")

	err := e.db.WithinTx(ctx, func(ctx context.Context) error {
		if _, _, err := e.inbox.Receive(ctx, "wager-consumer", messageID, "hash-1", now()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}

	err = e.db.WithinTx(ctx, func(ctx context.Context) error {
		_, inserted, err := e.inbox.Receive(ctx, "wager-consumer", messageID, "hash-1", now())
		if !inserted {
			t.Error("message should be new after the rollback")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) newEvents(t *testing.T, n int, at time.Time) []domain.Event {
	t.Helper()

	events := make([]domain.Event, n)

	for i := range events {
		wallet, err := domain.NewWallet(uuid.NewString(), uuid.NewString(), brl(t, "100.00"), at)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := wallet.Debit(uuid.NewString(), uuid.NewString(), brl(t, "80.00"), at)
		if err != nil {
			t.Fatal(err)
		}

		events[i], err = domain.NewWalletBalanceChangedEvent(domain.EventMeta{
			EventID:       uuid.NewString(),
			CorrelationID: uuid.NewString(),
			OccurredAt:    at,
		}, wallet, entry)
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := e.db.WithinTx(testContext(t), func(ctx context.Context) error {
		return e.outbox.Insert(ctx, events...)
	}); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		for _, event := range events {
			_ = e.outbox.MarkPublished(context.Background(), event.ID(), now())
		}
	})

	return events
}

func TestOutboxConcurrentPublishersAndRecovery(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	const total = 40
	const lease = time.Minute

	claimedAt := now().Add(-24 * time.Hour)
	events := e.newEvents(t, total, claimedAt)

	mine := make(map[string]bool, total)
	for _, event := range events {
		mine[event.ID()] = true
	}

	publishers := []*env{newEnv(t), newEnv(t)}
	batches := make([][]OutboxRecord, len(publishers))
	errs := make([]error, len(publishers))
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				batch, err := publishers[i].outbox.Claim(ctx, claimedAt, 5, lease)
				if err != nil {
					errs[i] = err
					return
				}
				if len(batch) == 0 {
					return
				}
				batches[i] = append(batches[i], batch...)
			}
		}()
	}
	close(start)
	wg.Wait()

	claims := map[string]int{}
	payloads := map[string]string{}
	for i, batch := range batches {
		if errs[i] != nil {
			t.Fatalf("publisher %d: %v", i, errs[i])
		}
		for _, record := range batch {
			if !mine[record.EventID] {
				continue
			}
			claims[record.EventID]++
			payloads[record.EventID] = string(record.Payload)
			if record.Attempts != 1 {
				t.Errorf("event %s attempts = %d, want 1", record.EventID, record.Attempts)
			}
		}
	}

	if len(claims) != total {
		t.Fatalf("claimed %d distinct events, want %d", len(claims), total)
	}
	for id, n := range claims {
		if n != 1 {
			t.Errorf("event %s claimed %d times", id, n)
		}
	}

	published := map[string]bool{}
	for i, event := range events {
		if i%2 == 0 {
			if err := e.outbox.MarkPublished(ctx, event.ID(), now()); err != nil {
				t.Fatal(err)
			}
			published[event.ID()] = true
		}
	}

	early, err := e.outbox.Claim(ctx, claimedAt.Add(lease/2), 1000, lease)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range early {
		if mine[record.EventID] {
			t.Errorf("event %s reclaimed before the lease expired", record.EventID)
		}
	}

	recovered, err := e.outbox.Claim(ctx, claimedAt.Add(lease+time.Second), 1000, lease)
	if err != nil {
		t.Fatal(err)
	}

	reclaimed := 0
	for _, record := range recovered {
		if !mine[record.EventID] {
			continue
		}
		reclaimed++

		if published[record.EventID] {
			t.Errorf("published event %s was claimed again", record.EventID)
		}
		if record.Attempts != 2 {
			t.Errorf("event %s attempts = %d, want 2", record.EventID, record.Attempts)
		}
		if string(record.Payload) != payloads[record.EventID] {
			t.Errorf("event %s payload changed on republication", record.EventID)
		}
	}

	if reclaimed != total/2 {
		t.Errorf("reclaimed = %d, want %d", reclaimed, total/2)
	}

	e.mustFail(t, "outbox payload update",
		`UPDATE outbox_events SET payload = '{}' WHERE event_id = $1`, events[0].ID())
}

func TestOutboxRetryBackoffAndDuplicateEvent(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	at := now().Add(-48 * time.Hour)
	events := e.newEvents(t, 1, at)
	id := events[0].ID()

	if err := e.outbox.Insert(ctx, events...); !errors.Is(err, domain.ErrDuplicateEvent) {
		t.Errorf("duplicate insert error = %v, want ErrDuplicateEvent", err)
	}

	retryAt := at.Add(time.Hour)

	if err := e.outbox.MarkFailed(ctx, id, retryAt, "sqs unavailable"); err != nil {
		t.Fatal(err)
	}

	contains := func(records []OutboxRecord) bool {
		for _, record := range records {
			if record.EventID == id {
				return true
			}
		}
		return false
	}

	before, err := e.outbox.Claim(ctx, at, 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if contains(before) {
		t.Error("event claimed before its backoff expired")
	}

	after, err := e.outbox.Claim(ctx, retryAt, 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(after) {
		t.Error("event not claimed after its backoff expired")
	}

	if lag, err := e.outbox.OldestPendingAge(ctx, at.Add(time.Minute)); err != nil || lag < time.Minute {
		t.Errorf("lag = %v, %v; want at least 1m", lag, err)
	}
}

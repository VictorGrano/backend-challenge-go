//go:build integration

package wagering

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
	"github.com/VictorGrano/backend-challenge-go/internal/postgres"
)

const defaultTestDSN = "postgres://jungle:devpassword@localhost:5432/jungle_challenge?sslmode=disable"

func testDSN() string {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}

	return defaultTestDSN
}

type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return systemNow().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.offset += d
}

type instance struct {
	*Service
	clock *clock
}

var testConfig = Config{
	ReferenceMaxAttempts: 3,
	ReferenceBaseBackoff: time.Second,
	ReferenceMaxBackoff:  4 * time.Second,
}

func newInstance(t *testing.T) *instance {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := postgres.Connect(ctx, testDSN())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	svc, err := NewService(
		db,
		postgres.NewWalletRepository(db),
		postgres.NewWagerTransactionRepository(db),
		postgres.NewLedgerRepository(db),
		postgres.NewInboxRepository(db),
		postgres.NewOutboxRepository(db),
		testConfig,
	)
	if err != nil {
		t.Fatal(err)
	}

	c := &clock{}

	return &instance{Service: svc.WithClock(c.now), clock: c}
}

func rawPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), testDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func brl(t *testing.T, value string) domain.Money {
	t.Helper()

	m, err := domain.ParseMoney(value, domain.BRL)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(testContext(t), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

func eventCount(t *testing.T, pool *pgxpool.Pool, aggregateID string, eventType domain.EventType) int {
	t.Helper()

	return count(t, pool,
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`,
		aggregateID, string(eventType))
}

type scenario struct {
	t        *testing.T
	app      *instance
	pool     *pgxpool.Pool
	wallet   *domain.Wallet
	provider string
}

func newScenario(t *testing.T, balance string) *scenario {
	t.Helper()

	app := newInstance(t)

	wallet, err := app.OpenWallet(testContext(t), uuid.NewString(), brl(t, balance), "")
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}

	return &scenario{
		t:        t,
		app:      app,
		pool:     rawPool(t),
		wallet:   wallet,
		provider: "provider-" + uuid.NewString(),
	}
}

func (s *scenario) command(externalID string, kind domain.WagerKind, amount, reference string) SubmitCommand {
	s.t.Helper()

	return SubmitCommand{
		ProviderID:                     s.provider,
		ExternalTransactionID:          externalID,
		IdempotencyKey:                 s.provider + ":" + externalID,
		PlayerID:                       s.wallet.PlayerID(),
		WalletID:                       s.wallet.ID(),
		RoundID:                        "round-1",
		GameID:                         "fortune-chimp",
		Kind:                           kind,
		Money:                          brl(s.t, amount),
		ReferenceExternalTransactionID: reference,
	}
}

func (s *scenario) submit(externalID string, kind domain.WagerKind, amount, reference string) Result {
	s.t.Helper()

	res, err := s.app.Submit(testContext(s.t), s.command(externalID, kind, amount, reference))
	if err != nil {
		s.t.Fatalf("submit %s %s: %v", kind, externalID, err)
	}

	return res
}

func (s *scenario) expect(res Result, status domain.WagerStatus, balance string, code domain.FailureCode) {
	s.t.Helper()

	if res.Status != status || res.FailureCode != code {
		s.t.Errorf("result = %s %q, want %s %q", res.Status, res.FailureCode, status, code)
	}

	switch {
	case balance == "" && res.Balance != nil:
		s.t.Errorf("result balance = %s, want none", res.Balance)
	case balance != "" && (res.Balance == nil || *res.Balance != brl(s.t, balance)):
		s.t.Errorf("result balance = %v, want %s", res.Balance, balance)
	}
}

func (s *scenario) reconciled(balance string, entries int64) {
	s.t.Helper()

	rec, err := s.app.Reconcile(testContext(s.t), s.wallet.ID())
	if err != nil {
		s.t.Fatal(err)
	}

	if !rec.Consistent || !rec.Difference.IsZero() {
		s.t.Errorf("wallet inconsistent: stored %s, calculated %s", rec.StoredBalance, rec.CalculatedBalance)
	}
	if rec.StoredBalance != brl(s.t, balance) {
		s.t.Errorf("stored balance = %s, want %s", rec.StoredBalance, balance)
	}
	if rec.CheckedEntries != entries {
		s.t.Errorf("ledger entries = %d, want %d", rec.CheckedEntries, entries)
	}
}

func TestOpenWalletWithPositiveBalance(t *testing.T) {
	s := newScenario(t, "1000.00")

	if s.wallet.Version() != 1 || s.wallet.Balance() != brl(t, "1000.00") {
		t.Errorf("wallet = %s v%d", s.wallet.Balance(), s.wallet.Version())
	}

	openings := count(t, s.pool,
		`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING' AND status = 'PROCESSED'`,
		s.wallet.ID())
	if openings != 1 {
		t.Errorf("openings = %d, want 1", openings)
	}

	if n := eventCount(t, s.pool, s.wallet.ID(), domain.EventWalletBalanceChanged); n != 1 {
		t.Errorf("WalletBalanceChanged events = %d, want 1", n)
	}

	processed := count(t, s.pool, `
		SELECT COUNT(*) FROM outbox_events o
		  JOIN wager_transactions w ON w.id = o.aggregate_id
		 WHERE w.wallet_id = $1 AND o.event_type = 'WagerTransactionProcessed'`, s.wallet.ID())
	if processed != 1 {
		t.Errorf("WagerTransactionProcessed events = %d, want 1", processed)
	}

	s.reconciled("1000.00", 1)

	_, err := s.app.OpenWallet(testContext(t), s.wallet.PlayerID(), brl(t, "5.00"), "")
	if !errors.Is(err, domain.ErrWalletAlreadyExists) {
		t.Errorf("second wallet error = %v, want ErrWalletAlreadyExists", err)
	}
}

func TestOpenWalletWithZeroBalance(t *testing.T) {
	s := newScenario(t, "0")

	if n := count(t, s.pool, `SELECT COUNT(*) FROM wager_transactions WHERE wallet_id = $1`, s.wallet.ID()); n != 0 {
		t.Errorf("transactions = %d, want 0", n)
	}
	if n := eventCount(t, s.pool, s.wallet.ID(), domain.EventWalletBalanceChanged); n != 0 {
		t.Errorf("events = %d, want 0", n)
	}

	s.reconciled("0.00", 0)
}

func TestOpenWalletRejectsInvalidInput(t *testing.T) {
	app := newInstance(t)
	ctx := testContext(t)

	if _, err := app.OpenWallet(ctx, "not-a-uuid", brl(t, "1.00"), ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("invalid player error = %v, want ErrInvalidInput", err)
	}
	if _, err := app.OpenWallet(ctx, uuid.NewString(), domain.Money{}, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("uninitialized money error = %v, want ErrInvalidInput", err)
	}
}

func TestBetAndIdempotentReplay(t *testing.T) {
	s := newScenario(t, "1000.00")

	first := s.submit("bet-1", domain.WagerBet, "25.00", "")
	s.expect(first, domain.WagerProcessed, "975.00", "")
	if first.IdempotentReplay {
		t.Error("first submission reported as replay")
	}

	s.expect(s.submit("bet-2", domain.WagerBet, "100.00", ""), domain.WagerProcessed, "875.00", "")

	replay := s.submit("bet-1", domain.WagerBet, "25", "")
	s.expect(replay, domain.WagerProcessed, "975.00", "")
	if !replay.IdempotentReplay || replay.TransactionID != first.TransactionID {
		t.Errorf("replay = %+v, want replay of %s", replay, first.TransactionID)
	}

	if n := eventCount(t, s.pool, first.TransactionID, domain.EventWagerTransactionProcessed); n != 1 {
		t.Errorf("processed events = %d, want 1", n)
	}

	s.reconciled("875.00", 3)
}

func TestIdempotencyConflicts(t *testing.T) {
	s := newScenario(t, "1000.00")
	ctx := testContext(t)

	s.submit("bet-1", domain.WagerBet, "25.00", "")

	changed := s.command("bet-1", domain.WagerBet, "26.00", "")
	if _, err := s.app.Submit(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("changed payload error = %v, want ErrIdempotencyConflict", err)
	}

	otherKey := s.command("bet-1", domain.WagerBet, "25.00", "")
	otherKey.IdempotencyKey = "another-key"
	if _, err := s.app.Submit(ctx, otherKey); !errors.Is(err, ErrExternalIDConflict) {
		t.Errorf("other key error = %v, want ErrExternalIDConflict", err)
	}

	s.reconciled("975.00", 2)
}

func TestSubmitRejectsInvalidInput(t *testing.T) {
	s := newScenario(t, "100.00")
	ctx := testContext(t)

	mutations := map[string]func(*SubmitCommand){
		"opening kind":             func(c *SubmitCommand) { c.Kind = domain.WagerOpening },
		"unknown kind":             func(c *SubmitCommand) { c.Kind = "JACKPOT" },
		"zero bet":                 func(c *SubmitCommand) { c.Money = brl(t, "0") },
		"blank idempotency key":    func(c *SubmitCommand) { c.IdempotencyKey = "" },
		"wallet id not uuid":       func(c *SubmitCommand) { c.WalletID = "wallet-1" },
		"player id not uuid":       func(c *SubmitCommand) { c.PlayerID = "player-1" },
		"refund without reference": func(c *SubmitCommand) { c.Kind = domain.WagerRefund },
		"loss with amount":         func(c *SubmitCommand) { c.Kind = domain.WagerLoss },
	}

	for name, mutate := range mutations {
		cmd := s.command("bet-"+name, domain.WagerBet, "10.00", "")
		mutate(&cmd)

		if _, err := s.app.Submit(ctx, cmd); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s error = %v, want ErrInvalidInput", name, err)
		}
	}

	missing := s.command("bet-missing-wallet", domain.WagerBet, "10.00", "")
	missing.WalletID = uuid.NewString()
	if _, err := s.app.Submit(ctx, missing); !errors.Is(err, domain.ErrWalletNotFound) {
		t.Errorf("missing wallet error = %v, want ErrWalletNotFound", err)
	}

	if n := count(t, s.pool, `SELECT COUNT(*) FROM wager_transactions WHERE provider_id = $1`, s.provider); n != 0 {
		t.Errorf("persisted transactions = %d, want 0", n)
	}
}

func TestBusinessRejectionsArePersisted(t *testing.T) {
	s := newScenario(t, "100.00")

	rejected := s.submit("bet-1", domain.WagerBet, "100.01", "")
	s.expect(rejected, domain.WagerRejected, "", domain.FailureInsufficientFunds)

	if n := eventCount(t, s.pool, rejected.TransactionID, domain.EventWagerTransactionRejected); n != 1 {
		t.Errorf("rejected events = %d, want 1", n)
	}

	replay := s.submit("bet-1", domain.WagerBet, "100.01", "")
	s.expect(replay, domain.WagerRejected, "", domain.FailureInsufficientFunds)
	if !replay.IdempotentReplay {
		t.Error("replay of a rejection not reported as replay")
	}

	otherPlayer := s.command("bet-2", domain.WagerBet, "10.00", "")
	otherPlayer.PlayerID = uuid.NewString()
	res, err := s.app.Submit(testContext(t), otherPlayer)
	if err != nil {
		t.Fatal(err)
	}
	s.expect(res, domain.WagerRejected, "", domain.FailureWalletMismatch)

	usd, err := domain.NewMoney(1000, domain.USD)
	if err != nil {
		t.Fatal(err)
	}
	otherCurrency := s.command("bet-3", domain.WagerBet, "10.00", "")
	otherCurrency.Money = usd
	res, err = s.app.Submit(testContext(t), otherCurrency)
	if err != nil {
		t.Fatal(err)
	}
	s.expect(res, domain.WagerRejected, "", domain.FailureCurrencyMismatch)

	s.reconciled("100.00", 1)
}

func TestWinAndLoss(t *testing.T) {
	s := newScenario(t, "100.00")

	s.expect(s.submit("bet-1", domain.WagerBet, "40.00", ""), domain.WagerProcessed, "60.00", "")
	s.expect(s.submit("win-1", domain.WagerWin, "90.00", "bet-1"), domain.WagerProcessed, "150.00", "")

	before, err := s.app.GetWallet(testContext(t), s.wallet.ID())
	if err != nil {
		t.Fatal(err)
	}

	loss := s.submit("loss-1", domain.WagerLoss, "0.00", "")
	s.expect(loss, domain.WagerProcessed, "150.00", "")

	after, err := s.app.GetWallet(testContext(t), s.wallet.ID())
	if err != nil {
		t.Fatal(err)
	}

	if after.Version() != before.Version() {
		t.Errorf("version = %d, want %d", after.Version(), before.Version())
	}
	if n := eventCount(t, s.pool, loss.TransactionID, domain.EventWagerTransactionProcessed); n != 1 {
		t.Errorf("processed events for loss = %d, want 1", n)
	}
	if n := eventCount(t, s.pool, s.wallet.ID(), domain.EventWalletBalanceChanged); n != 3 {
		t.Errorf("balance changed events = %d, want 3 (opening, bet, win)", n)
	}

	s.reconciled("150.00", 3)
}

func TestRefundAndSingleReversal(t *testing.T) {
	s := newScenario(t, "100.00")

	s.expect(s.submit("bet-1", domain.WagerBet, "40.00", ""), domain.WagerProcessed, "60.00", "")
	s.expect(s.submit("refund-1", domain.WagerRefund, "40.00", "bet-1"), domain.WagerProcessed, "100.00", "")

	s.expect(s.submit("refund-2", domain.WagerRefund, "40.00", "bet-1"),
		domain.WagerRejected, "", domain.FailureReferenceAlreadyReversed)
	s.expect(s.submit("rollback-1", domain.WagerRollback, "40.00", "bet-1"),
		domain.WagerRejected, "", domain.FailureReferenceAlreadyReversed)

	s.expect(s.submit("rollback-2", domain.WagerRollback, "40.00", "refund-1"), domain.WagerProcessed, "60.00", "")

	s.reconciled("60.00", 4)
}

func TestRollbackDirections(t *testing.T) {
	s := newScenario(t, "100.00")

	s.expect(s.submit("bet-1", domain.WagerBet, "40.00", ""), domain.WagerProcessed, "60.00", "")
	s.expect(s.submit("win-1", domain.WagerWin, "200.00", ""), domain.WagerProcessed, "260.00", "")

	s.expect(s.submit("rollback-bet", domain.WagerRollback, "40.00", "bet-1"), domain.WagerProcessed, "300.00", "")
	s.expect(s.submit("rollback-win", domain.WagerRollback, "200.00", "win-1"), domain.WagerProcessed, "100.00", "")

	s.reconciled("100.00", 5)
}

func TestRollbackWithInsufficientFunds(t *testing.T) {
	s := newScenario(t, "0")

	s.expect(s.submit("win-1", domain.WagerWin, "50.00", ""), domain.WagerProcessed, "50.00", "")
	s.expect(s.submit("bet-1", domain.WagerBet, "30.00", ""), domain.WagerProcessed, "20.00", "")

	rejected := s.submit("rollback-1", domain.WagerRollback, "50.00", "win-1")
	s.expect(rejected, domain.WagerRejected, "", domain.FailureReversalInsufficientFunds)

	s.reconciled("20.00", 2)
}

func TestReferenceValidation(t *testing.T) {
	s := newScenario(t, "100.00")
	ctx := testContext(t)

	s.submit("bet-1", domain.WagerBet, "40.00", "")
	s.submit("win-1", domain.WagerWin, "10.00", "")
	rejectedBet := s.submit("bet-rejected", domain.WagerBet, "999.00", "")
	s.expect(rejectedBet, domain.WagerRejected, "", domain.FailureInsufficientFunds)

	s.expect(s.submit("refund-partial", domain.WagerRefund, "39.99", "bet-1"),
		domain.WagerRejected, "", domain.FailureReferenceMismatch)

	s.expect(s.submit("refund-win", domain.WagerRefund, "10.00", "win-1"),
		domain.WagerRejected, "", domain.FailureReferenceMismatch)

	otherRound := s.command("refund-round", domain.WagerRefund, "40.00", "bet-1")
	otherRound.RoundID = "round-2"
	res, err := s.app.Submit(ctx, otherRound)
	if err != nil {
		t.Fatal(err)
	}
	s.expect(res, domain.WagerRejected, "", domain.FailureReferenceMismatch)

	s.expect(s.submit("refund-rejected", domain.WagerRefund, "999.00", "bet-rejected"),
		domain.WagerRejected, "", domain.FailureReferenceNotProcessed)

	otherProvider := s.command("refund-other", domain.WagerRefund, "40.00", "bet-1")
	otherProvider.ProviderID = "provider-" + uuid.NewString()
	otherProvider.IdempotencyKey = otherProvider.ProviderID + ":refund-other"
	res, err = s.app.Submit(ctx, otherProvider)
	if err != nil {
		t.Fatal(err)
	}
	s.expect(res, domain.WagerPendingReference, "", "")

	s.reconciled("70.00", 3)
}

func TestReversalBeforeReferenceIsResolvedLater(t *testing.T) {
	s := newScenario(t, "100.00")
	ctx := testContext(t)

	pending := s.submit("refund-1", domain.WagerRefund, "40.00", "bet-1")
	s.expect(pending, domain.WagerPendingReference, "", "")

	if n := eventCount(t, s.pool, pending.TransactionID, domain.EventWagerTransactionPendingReference); n != 1 {
		t.Errorf("pending reference events = %d, want 1", n)
	}

	replay := s.submit("refund-1", domain.WagerRefund, "40.00", "bet-1")
	s.expect(replay, domain.WagerPendingReference, "", "")
	if !replay.IdempotentReplay || replay.TransactionID != pending.TransactionID {
		t.Errorf("replay = %+v", replay)
	}

	if _, err := s.app.ResolvePendingReferences(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stored, err := s.app.GetTransaction(ctx, pending.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status() != domain.WagerPendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", stored.Status())
	}

	s.expect(s.submit("bet-1", domain.WagerBet, "40.00", ""), domain.WagerProcessed, "60.00", "")

	worker := newInstance(t)
	worker.clock.advance(testConfig.ReferenceBaseBackoff + time.Second)

	if _, err := worker.ResolvePendingReferences(ctx, 100); err != nil {
		t.Fatal(err)
	}

	stored, err = s.app.GetTransaction(ctx, pending.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status() != domain.WagerProcessed || stored.ReferenceTransactionID() == "" {
		t.Fatalf("stored = %s, reference %q", stored.Status(), stored.ReferenceTransactionID())
	}
	if balance, ok := stored.ResultBalance(); !ok || balance != brl(t, "100.00") {
		t.Errorf("result balance = %s, %v", balance, ok)
	}

	if n := eventCount(t, s.pool, pending.TransactionID, domain.EventWagerTransactionProcessed); n != 1 {
		t.Errorf("processed events = %d, want 1", n)
	}

	s.reconciled("100.00", 3)
}

func TestPendingReferenceExpires(t *testing.T) {
	s := newScenario(t, "100.00")
	ctx := testContext(t)

	pending := s.submit("rollback-1", domain.WagerRollback, "40.00", "bet-never")
	s.expect(pending, domain.WagerPendingReference, "", "")

	for attempt := 1; attempt <= testConfig.ReferenceMaxAttempts; attempt++ {
		s.app.clock.advance(testConfig.ReferenceMaxBackoff + time.Second)

		if _, err := s.app.ResolvePendingReferences(ctx, 100); err != nil {
			t.Fatal(err)
		}

		stored, err := s.app.GetTransaction(ctx, pending.TransactionID)
		if err != nil {
			t.Fatal(err)
		}

		want := domain.WagerPendingReference
		if attempt == testConfig.ReferenceMaxAttempts {
			want = domain.WagerRejected
		}
		if stored.Status() != want {
			t.Fatalf("after attempt %d: status = %s, want %s", attempt, stored.Status(), want)
		}
	}

	replay := s.submit("rollback-1", domain.WagerRollback, "40.00", "bet-never")
	s.expect(replay, domain.WagerRejected, "", domain.FailureReferenceNotFound)

	if n := eventCount(t, s.pool, pending.TransactionID, domain.EventWagerTransactionRejected); n != 1 {
		t.Errorf("rejected events = %d, want 1", n)
	}

	s.reconciled("100.00", 1)
}

func TestTwoConcurrentBetsAcrossInstances(t *testing.T) {
	s := newScenario(t, "100.00")
	instances := []*instance{newInstance(t), newInstance(t), newInstance(t)}
	ctx := testContext(t)

	const resends = 6

	results := make([]Result, 2*resends)
	errs := make([]error, 2*resends)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := range results {
		cmd := s.command("bet-a", domain.WagerBet, "80.00", "")
		if i%2 == 1 {
			cmd = s.command("bet-b", domain.WagerBet, "80.00", "")
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = instances[i%len(instances)].Submit(ctx, cmd)
		}()
	}
	close(start)
	wg.Wait()

	byBet := map[int]Result{}
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("submission %d: %v", i, errs[i])
		}

		first, seen := byBet[i%2]
		if !seen {
			byBet[i%2] = res
			continue
		}
		if res.TransactionID != first.TransactionID || res.Status != first.Status {
			t.Errorf("submission %d diverged: %+v vs %+v", i, res, first)
		}
	}

	statuses := map[domain.WagerStatus]int{byBet[0].Status: 1}
	statuses[byBet[1].Status]++

	if statuses[domain.WagerProcessed] != 1 || statuses[domain.WagerRejected] != 1 {
		t.Fatalf("statuses = %v, want one PROCESSED and one REJECTED", statuses)
	}

	for _, res := range byBet {
		if res.Status == domain.WagerRejected && res.FailureCode != domain.FailureInsufficientFunds {
			t.Errorf("failure code = %s", res.FailureCode)
		}
	}

	s.reconciled("20.00", 2)
	s.submit("bet-a", domain.WagerBet, "80.00", "")
	s.submit("bet-b", domain.WagerBet, "80.00", "")
	s.reconciled("20.00", 2)
}

func TestSameBetFiftyTimesAcrossInstances(t *testing.T) {
	s := newScenario(t, "100.00")
	instances := []*instance{newInstance(t), newInstance(t), newInstance(t)}
	ctx := testContext(t)

	const attempts = 50

	results := make([]Result, attempts)
	errs := make([]error, attempts)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := range attempts {
		cmd := s.command("bet-1", domain.WagerBet, "25.00", "")

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = instances[i%len(instances)].Submit(ctx, cmd)
		}()
	}
	close(start)
	wg.Wait()

	originals := 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if res.TransactionID != results[0].TransactionID {
			t.Errorf("attempt %d returned another transaction", i)
		}
		if !res.IdempotentReplay {
			originals++
		}

		s.expect(res, domain.WagerProcessed, "75.00", "")
	}

	if originals != 1 {
		t.Errorf("non-replay results = %d, want 1", originals)
	}

	if n := count(t, s.pool, `SELECT COUNT(*) FROM wager_transactions WHERE provider_id = $1`, s.provider); n != 1 {
		t.Errorf("transactions = %d, want 1", n)
	}
	if n := eventCount(t, s.pool, results[0].TransactionID, domain.EventWagerTransactionProcessed); n != 1 {
		t.Errorf("processed events = %d, want 1", n)
	}

	s.reconciled("75.00", 2)
}

func TestDistinctWalletsInParallel(t *testing.T) {
	const wallets = 8
	const betsPerWallet = 5

	scenarios := make([]*scenario, wallets)
	for i := range scenarios {
		scenarios[i] = newScenario(t, "100.00")
	}

	ctx := testContext(t)
	errs := make(chan error, wallets*betsPerWallet)

	var wg sync.WaitGroup
	for _, s := range scenarios {
		for j := range betsPerWallet {
			cmd := s.command("bet-"+string(rune('a'+j)), domain.WagerBet, "10.00", "")

			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.app.Submit(ctx, cmd); err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	for _, s := range scenarios {
		s.reconciled("50.00", 1+betsPerWallet)
	}
}

func inboxCommand(s *scenario, messageID, externalID string) SubmitCommand {
	cmd := s.command(externalID, domain.WagerBet, "25.00", "")
	cmd.CausationID = messageID
	cmd.Inbox = &InboxRef{ConsumerName: "wager-consumer", MessageID: messageID, PayloadHash: "body-hash-1"}

	return cmd
}

func TestInboxRedelivery(t *testing.T) {
	s := newScenario(t, "100.00")
	ctx := testContext(t)
	messageID := "msg-" + uuid.NewString()

	first, err := s.app.Submit(ctx, inboxCommand(s, messageID, "bet-1"))
	if err != nil {
		t.Fatal(err)
	}
	s.expect(first, domain.WagerProcessed, "75.00", "")

	redelivered, err := newInstance(t).Submit(ctx, inboxCommand(s, messageID, "bet-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !redelivered.IdempotentReplay || redelivered.TransactionID != first.TransactionID {
		t.Errorf("redelivery = %+v", redelivered)
	}

	completed := count(t, s.pool, `
		SELECT COUNT(*) FROM inbox_messages
		 WHERE consumer_name = 'wager-consumer' AND message_id = $1
		   AND completed_at IS NOT NULL AND transaction_id = $2`,
		messageID, first.TransactionID)
	if completed != 1 {
		t.Errorf("completed inbox rows = %d, want 1", completed)
	}

	tampered := inboxCommand(s, messageID, "bet-1")
	tampered.Inbox.PayloadHash = "body-hash-2"
	if _, err := s.app.Submit(ctx, tampered); !errors.Is(err, ErrMessageConflict) {
		t.Errorf("tampered message error = %v, want ErrMessageConflict", err)
	}

	s.reconciled("75.00", 2)
}

func TestSameOperationThroughHTTPAndSQS(t *testing.T) {
	s := newScenario(t, "100.00")
	instances := []*instance{newInstance(t), newInstance(t), newInstance(t)}
	ctx := testContext(t)

	const perChannel = 10

	results := make([]Result, 2*perChannel)
	errs := make([]error, 2*perChannel)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := range results {
		cmd := s.command("bet-1", domain.WagerBet, "25.00", "")
		if i%2 == 1 {
			cmd = inboxCommand(s, "msg-"+uuid.NewString(), "bet-1")
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = instances[i%len(instances)].Submit(ctx, cmd)
		}()
	}
	close(start)
	wg.Wait()

	originals := 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("submission %d: %v", i, errs[i])
		}
		if res.TransactionID != results[0].TransactionID {
			t.Errorf("submission %d returned another transaction", i)
		}
		if !res.IdempotentReplay {
			originals++
		}
	}

	if originals != 1 {
		t.Errorf("non-replay results = %d, want 1", originals)
	}

	completed := count(t, s.pool,
		`SELECT COUNT(*) FROM inbox_messages WHERE transaction_id = $1 AND completed_at IS NOT NULL`,
		results[0].TransactionID)
	if completed != perChannel {
		t.Errorf("completed inbox rows = %d, want %d", completed, perChannel)
	}

	s.reconciled("75.00", 2)
}

func TestLedgerPagination(t *testing.T) {
	s := newScenario(t, "100.00")
	ctx := testContext(t)

	for _, id := range []string{"bet-1", "bet-2", "bet-3", "bet-4"} {
		s.submit(id, domain.WagerBet, "10.00", "")
	}

	var (
		balances []string
		cursor   string
		pages    int
	)

	for {
		page, err := s.app.GetLedger(ctx, s.wallet.ID(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}

		pages++
		for _, entry := range page.Entries {
			balances = append(balances, entry.BalanceAfter().String())
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	want := []string{"100.00", "90.00", "80.00", "70.00", "60.00"}
	if pages != 3 || len(balances) != len(want) {
		t.Fatalf("pages = %d, balances = %v", pages, balances)
	}
	for i := range want {
		if balances[i] != want[i] {
			t.Errorf("entry %d balance after = %s, want %s", i, balances[i], want[i])
		}
	}

	if _, err := s.app.GetLedger(ctx, s.wallet.ID(), "not-a-cursor!", 2); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("invalid cursor error = %v, want ErrInvalidInput", err)
	}
	if _, err := s.app.GetLedger(ctx, s.wallet.ID(), "", MaxLedgerPageSize+1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("oversized limit error = %v, want ErrInvalidInput", err)
	}
	if _, err := s.app.GetLedger(ctx, uuid.NewString(), "", 2); !errors.Is(err, domain.ErrWalletNotFound) {
		t.Errorf("missing wallet error = %v, want ErrWalletNotFound", err)
	}
}

func TestProviderTransactionLookupIsScoped(t *testing.T) {
	s := newScenario(t, "100.00")
	ctx := testContext(t)

	res := s.submit("bet-1", domain.WagerBet, "10.00", "")

	found, err := s.app.GetProviderTransaction(ctx, s.provider, "bet-1")
	if err != nil || found.ID() != res.TransactionID {
		t.Errorf("lookup = %v, %v", found, err)
	}

	_, err = s.app.GetProviderTransaction(ctx, "provider-"+uuid.NewString(), "bet-1")
	if !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("other provider error = %v, want ErrTransactionNotFound", err)
	}
}

func TestReconciliationReportsDivergence(t *testing.T) {
	s := newScenario(t, "100.00")
	ctx := testContext(t)

	if _, err := s.pool.Exec(ctx, `UPDATE wallets SET balance = balance + 150 WHERE id = $1`, s.wallet.ID()); err != nil {
		t.Fatal(err)
	}

	rec, err := s.app.Reconcile(ctx, s.wallet.ID())
	if err != nil {
		t.Fatal(err)
	}

	if rec.Consistent || rec.Difference.String() != "1.50" ||
		rec.StoredBalance != brl(t, "101.50") || rec.CalculatedBalance != brl(t, "100.00") {
		t.Errorf("reconciliation = %+v", rec)
	}

	after, err := s.app.GetWallet(ctx, s.wallet.ID())
	if err != nil {
		t.Fatal(err)
	}
	if after.Balance() != brl(t, "101.50") {
		t.Errorf("reconciliation changed the balance to %s", after.Balance())
	}
}

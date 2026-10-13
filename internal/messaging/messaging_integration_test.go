//go:build integration

package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
	"github.com/VictorGrano/backend-challenge-go/internal/metrics"
	"github.com/VictorGrano/backend-challenge-go/internal/postgres"
	"github.com/VictorGrano/backend-challenge-go/internal/wagering"
)

const defaultTestDSN = "postgres://jungle:devpassword@localhost:5432/jungle_challenge?sslmode=disable"

func setDefaultEnv(t *testing.T) {
	t.Helper()

	for key, value := range map[string]string{
		"AWS_REGION":            "us-east-1",
		"AWS_ACCESS_KEY_ID":     "test",
		"AWS_SECRET_ACCESS_KEY": "test",
		"AWS_ENDPOINT_URL":      "http://localhost:4566",
	} {
		if os.Getenv(key) == "" {
			t.Setenv(key, value)
		}
	}
}

func testDSN() string {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}

	return defaultTestDSN
}

var testConsumerConfig = ConsumerConfig{
	Concurrency:       4,
	WaitTime:          time.Second,
	VisibilityTimeout: 4 * time.Second,
	HandlerTimeout:    2 * time.Second,
	RetryBaseDelay:    time.Second,
	RetryMaxDelay:     2 * time.Second,
}

var testPublisherConfig = PublisherConfig{
	BatchSize:      10,
	PollInterval:   100 * time.Millisecond,
	Lease:          2 * time.Second,
	RetryBaseDelay: time.Second,
	RetryMaxDelay:  2 * time.Second,
}

const testMaxReceiveCount = 2

type fixture struct {
	t        *testing.T
	client   *sqs.Client
	queues   *Queues
	pool     *pgxpool.Pool
	wallet   *domain.Wallet
	provider string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	setDefaultEnv(t)

	ctx := testContext(t)

	client, err := NewClient(ctx)
	if err != nil {
		t.Fatal(err)
	}

	prefix := "t-" + uuid.NewString()[:8]
	names := QueueNames{
		Input:      prefix + "-in.fifo",
		DeadLetter: prefix + "-dlq.fifo",
		Events:     prefix + "-events.fifo",
	}

	fifo := map[string]string{"FifoQueue": "true"}

	dlq, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(names.DeadLetter), Attributes: fifo})
	if err != nil {
		t.Fatalf("create dlq: %v", err)
	}

	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       dlq.QueueUrl,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatal(err)
	}

	redrive := fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":"%d"}`,
		attrs.Attributes[string(types.QueueAttributeNameQueueArn)], testMaxReceiveCount)

	if _, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(names.Input),
		Attributes: map[string]string{"FifoQueue": "true", "RedrivePolicy": redrive},
	}); err != nil {
		t.Fatalf("create input queue: %v", err)
	}

	if _, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(names.Events), Attributes: fifo}); err != nil {
		t.Fatalf("create events queue: %v", err)
	}

	queues, err := NewQueues(client, names)
	if err != nil {
		t.Fatal(err)
	}
	if err := queues.Resolve(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		for _, url := range []string{queues.InputURL(), queues.DeadLetterURL(), queues.EventsURL()} {
			_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
		}
	})

	pool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	f := &fixture{t: t, client: client, queues: queues, pool: pool, provider: "provider-" + uuid.NewString()}

	money, err := domain.ParseMoney("100.00", domain.BRL)
	if err != nil {
		t.Fatal(err)
	}

	f.wallet, err = f.newService().OpenWallet(ctx, uuid.NewString(), money, "")
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}

	return f
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

type instance struct {
	service *wagering.Service
	outbox  *postgres.OutboxRepository
	metrics *metrics.Metrics
}

func (f *fixture) newInstance() instance {
	f.t.Helper()

	db, err := postgres.Connect(testContext(f.t), testDSN())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(db.Close)

	outbox := postgres.NewOutboxRepository(db)

	service, err := wagering.NewService(
		db,
		postgres.NewWalletRepository(db),
		postgres.NewWagerTransactionRepository(db),
		postgres.NewLedgerRepository(db),
		postgres.NewInboxRepository(db),
		outbox,
		wagering.DefaultConfig(),
	)
	if err != nil {
		f.t.Fatal(err)
	}

	return instance{service: service, outbox: outbox, metrics: metrics.New()}
}

func (f *fixture) newService() *wagering.Service { return f.newInstance().service }

func (f *fixture) newConsumer(inst instance) *Consumer {
	f.t.Helper()

	consumer, err := NewConsumer(f.client, f.queues, inst.service, inst.metrics, quietLogger(), testConsumerConfig)
	if err != nil {
		f.t.Fatal(err)
	}

	return consumer
}

func (f *fixture) start(consumer *Consumer) (stop func()) {
	pollCtx, stopPolling := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		consumer.Run(pollCtx, context.Background())
	}()

	var once sync.Once

	stop = func() {
		once.Do(func() {
			stopPolling()

			select {
			case <-done:
			case <-time.After(15 * time.Second):
				f.t.Error("consumer did not stop")
			}
		})
	}

	f.t.Cleanup(stop)

	return stop
}

type messageData struct {
	ProviderID                     string            `json:"providerId"`
	ExternalTransactionID          string            `json:"externalTransactionId"`
	IdempotencyKey                 string            `json:"idempotencyKey"`
	PlayerID                       string            `json:"playerId"`
	WalletID                       string            `json:"walletId"`
	RoundID                        string            `json:"roundId"`
	GameID                         string            `json:"gameId"`
	Kind                           string            `json:"kind"`
	Money                          map[string]string `json:"money"`
	ReferenceExternalTransactionID string            `json:"referenceExternalTransactionId,omitempty"`
}

func (f *fixture) data(externalID, amount string) messageData {
	return messageData{
		ProviderID:            f.provider,
		ExternalTransactionID: externalID,
		IdempotencyKey:        f.provider + ":" + externalID,
		PlayerID:              f.wallet.PlayerID(),
		WalletID:              f.wallet.ID(),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  "BET",
		Money:                 map[string]string{"amount": amount, "currency": "BRL"},
	}
}

func body(t *testing.T, messageID string, data any) string {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"messageId":  messageID,
		"type":       MessageTypeWagerTransactionRequested,
		"occurredAt": "2026-09-08T12:00:00.000Z",
		"data":       data,
	})
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

func (f *fixture) send(messageID, body string) {
	f.t.Helper()

	_, err := f.client.SendMessage(testContext(f.t), &sqs.SendMessageInput{
		QueueUrl:               aws.String(f.queues.InputURL()),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String(f.wallet.ID()),
		MessageDeduplicationId: aws.String(messageID + "-" + uuid.NewString()[:8]),
	})
	if err != nil {
		f.t.Fatalf("send: %v", err)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for: %s", what)
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()

	var n int
	if err := f.pool.QueryRow(testContext(f.t), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}

	return n
}

func (f *fixture) ledgerEntries() int {
	return f.count(`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, f.wallet.ID())
}

func (f *fixture) balance() string {
	f.t.Helper()

	var cents int64
	if err := f.pool.QueryRow(testContext(f.t), `SELECT balance FROM wallets WHERE id = $1`, f.wallet.ID()).Scan(&cents); err != nil {
		f.t.Fatal(err)
	}

	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func (f *fixture) queueDepth(url string) int {
	f.t.Helper()

	out, err := f.client.GetQueueAttributes(testContext(f.t), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}

	total := 0
	for _, value := range out.Attributes {
		var n int
		fmt.Sscan(value, &n)
		total += n
	}

	return total
}

func (f *fixture) drain(url string, wait time.Duration) []types.Message {
	f.t.Helper()

	var all []types.Message

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		out, err := f.client.ReceiveMessage(testContext(f.t), &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(url),
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			f.t.Fatal(err)
		}

		for _, msg := range out.Messages {
			all = append(all, msg)
			_, _ = f.client.DeleteMessage(testContext(f.t), &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: msg.ReceiptHandle})
		}
	}

	return all
}

func counter(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	total := 0.0
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
	}

	return total
}

func TestConsumerProcessesAndDeletesMessage(t *testing.T) {
	f := newFixture(t)
	inst := f.newInstance()
	f.start(f.newConsumer(inst))

	messageID := "msg-" + uuid.NewString()
	f.send(messageID, body(t, messageID, f.data("bet-1", "25.00")))

	eventually(t, 15*time.Second, "bet debited and message removed", func() bool {
		return f.ledgerEntries() == 2 && f.queueDepth(f.queues.InputURL()) == 0
	})

	if got := f.balance(); got != "75.00" {
		t.Errorf("balance = %s, want 75.00", got)
	}

	completed := f.count(`
		SELECT COUNT(*) FROM inbox_messages
		 WHERE consumer_name = $1 AND message_id = $2 AND completed_at IS NOT NULL`, ConsumerName, messageID)
	if completed != 1 {
		t.Errorf("completed inbox rows = %d, want 1", completed)
	}

	caused := f.count(`SELECT COUNT(*) FROM outbox_events WHERE payload->>'causationId' = $1`, messageID)
	if caused != 2 {
		t.Errorf("events caused by the message = %d, want 2", caused)
	}
}

func TestConsumerCrashAfterCommitIsRedelivered(t *testing.T) {
	f := newFixture(t)

	crashed := make(chan struct{})

	dying := f.newConsumer(f.newInstance())
	dying.beforeDelete = func() error {
		select {
		case <-crashed:
		default:
			close(crashed)
		}

		return errors.New("simulated crash before delete")
	}
	stopDying := f.start(dying)

	messageID := "msg-" + uuid.NewString()
	f.send(messageID, body(t, messageID, f.data("bet-1", "25.00")))

	select {
	case <-crashed:
	case <-time.After(15 * time.Second):
		t.Fatal("message was not processed by the first consumer")
	}

	stopDying()

	if f.ledgerEntries() != 2 || f.queueDepth(f.queues.InputURL()) != 1 {
		t.Fatalf("after crash: ledger entries = %d, queue depth = %d", f.ledgerEntries(), f.queueDepth(f.queues.InputURL()))
	}

	survivor := f.newInstance()
	f.start(f.newConsumer(survivor))

	eventually(t, 20*time.Second, "redelivered message removed", func() bool {
		return f.queueDepth(f.queues.InputURL()) == 0
	})

	if n := counter(t, survivor.metrics, "wager_duplicates_total"); n != 1 {
		t.Errorf("duplicates recognized = %v, want 1", n)
	}
	if f.ledgerEntries() != 2 || f.balance() != "75.00" {
		t.Errorf("after redelivery: ledger entries = %d, balance = %s", f.ledgerEntries(), f.balance())
	}
	if n := f.count(`SELECT COUNT(*) FROM wager_transactions WHERE provider_id = $1`, f.provider); n != 1 {
		t.Errorf("transactions = %d, want 1", n)
	}
	if f.queueDepth(f.queues.DeadLetterURL()) != 0 {
		t.Error("redelivered message must not reach the DLQ")
	}
}

func TestConsumerDeduplicatesRepeatedMessages(t *testing.T) {
	f := newFixture(t)

	instances := []instance{f.newInstance(), f.newInstance(), f.newInstance()}
	for _, inst := range instances {
		f.start(f.newConsumer(inst))
	}

	const copies = 20

	messageID := "msg-" + uuid.NewString()
	payload := body(t, messageID, f.data("bet-1", "25.00"))

	for range copies {
		f.send(messageID, payload)
	}

	eventually(t, 30*time.Second, "all copies consumed", func() bool {
		return f.queueDepth(f.queues.InputURL()) == 0
	})

	duplicates := 0.0
	for _, inst := range instances {
		duplicates += counter(t, inst.metrics, "wager_duplicates_total")
	}

	if duplicates != copies-1 {
		t.Errorf("duplicates recognized = %v, want %d", duplicates, copies-1)
	}
	if f.ledgerEntries() != 2 || f.balance() != "75.00" {
		t.Errorf("ledger entries = %d, balance = %s; want 2 and 75.00", f.ledgerEntries(), f.balance())
	}
}

func TestConsumerSendsPermanentFailuresToDLQ(t *testing.T) {
	f := newFixture(t)
	inst := f.newInstance()
	f.start(f.newConsumer(inst))

	opening := f.data("opening-1", "10.00")
	opening.Kind = "OPENING"

	float := map[string]any{
		"providerId": f.provider, "externalTransactionId": "float-1", "idempotencyKey": "k",
		"playerId": f.wallet.PlayerID(), "walletId": f.wallet.ID(), "roundId": "r", "gameId": "g",
		"kind": "BET", "money": map[string]any{"amount": 25.5, "currency": "BRL"},
	}

	f.send("msg-ok", body(t, "msg-ok-"+f.provider, f.data("bet-1", "25.00")))
	eventually(t, 15*time.Second, "valid bet processed", func() bool { return f.ledgerEntries() == 2 })

	cases := map[string]string{
		"invalid_message":          `{"not":"an envelope"`,
		"invalid_input":            body(t, "msg-opening-"+f.provider, opening),
		"idempotency_key_conflict": body(t, "msg-conflict-"+f.provider, f.data("bet-1", "26.00")),
	}

	for reason, payload := range cases {
		f.send("msg-"+reason, payload)
	}
	f.send("msg-float", body(t, "msg-float-"+f.provider, float))

	eventually(t, 20*time.Second, "permanent failures moved to the DLQ", func() bool {
		return f.queueDepth(f.queues.InputURL()) == 0 && f.queueDepth(f.queues.DeadLetterURL()) == 4
	})

	reasons := map[string]int{}
	for _, msg := range f.drain(f.queues.DeadLetterURL(), 3*time.Second) {
		reasons[aws.ToString(msg.MessageAttributes["failureReason"].StringValue)]++
	}

	want := map[string]int{"invalid_message": 2, "invalid_input": 1, "idempotency_key_conflict": 1}
	for reason, n := range want {
		if reasons[reason] != n {
			t.Errorf("DLQ messages with reason %s = %d, want %d (all: %v)", reason, reasons[reason], n, reasons)
		}
	}

	if n := counter(t, inst.metrics, "sqs_dlq_total"); n != 4 {
		t.Errorf("sqs_dlq_total = %v, want 4", n)
	}

	if f.ledgerEntries() != 2 || f.balance() != "75.00" {
		t.Errorf("ledger entries = %d, balance = %s", f.ledgerEntries(), f.balance())
	}
}

func TestConsumerRetriesThenRedrivesToDLQ(t *testing.T) {
	f := newFixture(t)
	inst := f.newInstance()
	f.start(f.newConsumer(inst))

	data := f.data("bet-1", "25.00")
	data.WalletID = uuid.NewString()

	messageID := "msg-" + uuid.NewString()
	f.send(messageID, body(t, messageID, data))

	eventually(t, 40*time.Second, "message redriven to the DLQ", func() bool {
		return f.queueDepth(f.queues.DeadLetterURL()) == 1 && f.queueDepth(f.queues.InputURL()) == 0
	})

	if n := counter(t, inst.metrics, "sqs_retries_total"); n != testMaxReceiveCount {
		t.Errorf("retries = %v, want %d", n, testMaxReceiveCount)
	}
	if n := f.count(`SELECT COUNT(*) FROM wager_transactions WHERE provider_id = $1`, f.provider); n != 0 {
		t.Errorf("transactions = %d, want 0", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM inbox_messages WHERE message_id = $1`, messageID); n != 0 {
		t.Errorf("inbox rows = %d, want 0", n)
	}
}

func TestConsumerShutdownFinishesInFlightWork(t *testing.T) {
	f := newFixture(t)

	inFlight := make(chan struct{})
	proceed := make(chan struct{})

	consumer := f.newConsumer(f.newInstance())
	consumer.beforeDelete = func() error {
		close(inFlight)
		<-proceed

		return nil
	}

	pollCtx, stopPolling := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		consumer.Run(pollCtx, context.Background())
	}()

	messageID := "msg-" + uuid.NewString()
	f.send(messageID, body(t, messageID, f.data("bet-1", "25.00")))

	select {
	case <-inFlight:
	case <-time.After(15 * time.Second):
		t.Fatal("message was not picked up")
	}

	stopPolling()

	select {
	case <-done:
		t.Fatal("Run returned while a message was still in flight")
	case <-time.After(500 * time.Millisecond):
	}

	close(proceed)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the in-flight message finished")
	}

	if depth := f.queueDepth(f.queues.InputURL()); depth != 0 {
		t.Errorf("queue depth = %d, want 0", depth)
	}
}

func (f *fixture) clearOutbox() {
	f.t.Helper()

	_, err := f.pool.Exec(testContext(f.t), `
		UPDATE outbox_events SET published_at = now()
		 WHERE published_at IS NULL
		   AND event_id NOT IN (
			SELECT o.event_id FROM outbox_events o
			  LEFT JOIN wager_transactions w ON w.id = o.aggregate_id
			 WHERE o.aggregate_id = $1 OR w.wallet_id = $1
		   )`, f.wallet.ID())
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) pendingEvents() int {
	return f.count(`
		SELECT COUNT(*) FROM outbox_events o
		  LEFT JOIN wager_transactions w ON w.id = o.aggregate_id
		 WHERE (o.aggregate_id = $1 OR w.wallet_id = $1) AND o.published_at IS NULL`, f.wallet.ID())
}

func (f *fixture) newPublisher(inst instance) *Publisher {
	f.t.Helper()

	publisher, err := NewPublisher(f.client, f.queues, inst.outbox, inst.metrics, quietLogger(), testPublisherConfig)
	if err != nil {
		f.t.Fatal(err)
	}

	return publisher
}

func (f *fixture) run(publisher *Publisher) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		publisher.Run(ctx)
	}()

	var once sync.Once

	stop = func() {
		once.Do(func() {
			cancel()

			select {
			case <-done:
			case <-time.After(15 * time.Second):
				f.t.Error("publisher did not stop")
			}
		})
	}

	f.t.Cleanup(stop)

	return stop
}

func TestPublisherConcurrencyAndRecovery(t *testing.T) {
	f := newFixture(t)
	f.clearOutbox()

	inst := f.newInstance()
	ctx := testContext(t)

	for i := range 5 {
		money, _ := domain.ParseMoney("10.00", domain.BRL)

		_, err := inst.service.Submit(ctx, wagering.SubmitCommand{
			ProviderID:            f.provider,
			ExternalTransactionID: fmt.Sprintf("bet-%d", i),
			IdempotencyKey:        fmt.Sprintf("%s:bet-%d", f.provider, i),
			PlayerID:              f.wallet.PlayerID(),
			WalletID:              f.wallet.ID(),
			RoundID:               "round-1",
			GameID:                "game-1",
			Kind:                  domain.WagerBet,
			Money:                 money,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	const total = 12

	if pending := f.pendingEvents(); pending != total {
		t.Fatalf("pending events = %d, want %d", pending, total)
	}

	var (
		mu      sync.Mutex
		dropped string
	)

	flaky := f.newPublisher(f.newInstance())
	flaky.afterSend = func(eventID string) error {
		mu.Lock()
		defer mu.Unlock()

		if dropped == "" {
			dropped = eventID
			return errors.New("simulated crash after send")
		}

		return nil
	}

	f.run(flaky)
	f.run(f.newPublisher(f.newInstance()))

	eventually(t, 30*time.Second, "all events confirmed as published", func() bool {
		return f.pendingEvents() == 0
	})

	mu.Lock()
	droppedID := dropped
	mu.Unlock()

	if droppedID == "" {
		t.Fatal("the simulated crash never happened")
	}

	if attempts := f.count(`SELECT attempts FROM outbox_events WHERE event_id = $1`, droppedID); attempts != 2 {
		t.Errorf("attempts of the abandoned event = %d, want 2", attempts)
	}

	delivered := map[string]int{}
	for _, msg := range f.drain(f.queues.EventsURL(), 3*time.Second) {
		var event struct {
			EventID string `json:"eventId"`
		}
		if err := json.Unmarshal([]byte(aws.ToString(msg.Body)), &event); err != nil {
			t.Fatal(err)
		}

		if attr := aws.ToString(msg.MessageAttributes["eventId"].StringValue); attr != event.EventID {
			t.Errorf("eventId attribute %q differs from body %q", attr, event.EventID)
		}

		delivered[event.EventID]++
	}

	if len(delivered) != total {
		t.Errorf("distinct events delivered = %d, want %d", len(delivered), total)
	}
	for id, n := range delivered {
		if n != 1 {
			t.Errorf("event %s delivered %d times", id, n)
		}
	}
	if delivered[droppedID] != 1 {
		t.Errorf("abandoned event delivered %d times, want 1", delivered[droppedID])
	}
}

func TestPublisherRetriesWhenDestinationIsUnavailable(t *testing.T) {
	f := newFixture(t)
	f.clearOutbox()

	broken, err := NewQueues(f.client, QueueNames{Input: "x", DeadLetter: "y", Events: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	broken.urls["missing"] = f.queues.EventsURL() + "-does-not-exist"

	inst := f.newInstance()

	failing, err := NewPublisher(f.client, broken, inst.outbox, inst.metrics, quietLogger(), testPublisherConfig)
	if err != nil {
		t.Fatal(err)
	}

	stopFailing := f.run(failing)

	eventually(t, 15*time.Second, "publish failures recorded", func() bool {
		return counter(t, inst.metrics, "outbox_publish_total") >= 2
	})
	stopFailing()

	if pending := f.pendingEvents(); pending != 2 {
		t.Fatalf("pending events = %d, want 2 (nothing may be lost)", pending)
	}
	if n := f.count(`
		SELECT COUNT(*) FROM outbox_events
		 WHERE aggregate_id = $1 AND last_error IS NOT NULL AND next_attempt_at > occurred_at`, f.wallet.ID()); n != 1 {
		t.Errorf("events with recorded failure and backoff = %d, want 1", n)
	}

	f.run(f.newPublisher(f.newInstance()))

	eventually(t, 20*time.Second, "events published after recovery", func() bool {
		return f.pendingEvents() == 0
	})

	if got := len(f.drain(f.queues.EventsURL(), 2*time.Second)); got != 2 {
		t.Errorf("events delivered = %d, want 2", got)
	}
}

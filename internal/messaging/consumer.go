package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
	"github.com/VictorGrano/backend-challenge-go/internal/metrics"
	"github.com/VictorGrano/backend-challenge-go/internal/wagering"
)

const MessageTypeWagerTransactionRequested = "WagerTransactionRequested"

const ConsumerName = "wager-transactions-consumer"

type ConsumerConfig struct {
	Concurrency       int
	WaitTime          time.Duration
	VisibilityTimeout time.Duration
	HandlerTimeout    time.Duration
	RetryBaseDelay    time.Duration
	RetryMaxDelay     time.Duration
}

func DefaultConsumerConfig() ConsumerConfig {
	return ConsumerConfig{
		Concurrency:       8,
		WaitTime:          10 * time.Second,
		VisibilityTimeout: 30 * time.Second,
		HandlerTimeout:    15 * time.Second,
		RetryBaseDelay:    2 * time.Second,
		RetryMaxDelay:     time.Minute,
	}
}

func (c ConsumerConfig) validate() error {
	if c.Concurrency < 1 || c.WaitTime < 0 || c.WaitTime > 20*time.Second ||
		c.HandlerTimeout <= 0 || c.VisibilityTimeout <= c.HandlerTimeout ||
		c.RetryBaseDelay <= 0 || c.RetryMaxDelay < c.RetryBaseDelay {
		return fmt.Errorf("messaging: invalid consumer config: %+v", c)
	}

	return nil
}

type Consumer struct {
	client  *sqs.Client
	queues  *Queues
	service *wagering.Service
	metrics *metrics.Metrics
	logger  *slog.Logger
	cfg     ConsumerConfig

	beforeDelete func() error
}

func NewConsumer(
	client *sqs.Client,
	queues *Queues,
	service *wagering.Service,
	m *metrics.Metrics,
	logger *slog.Logger,
	cfg ConsumerConfig,
) (*Consumer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &Consumer{client: client, queues: queues, service: service, metrics: m, logger: logger, cfg: cfg}, nil
}

func (c *Consumer) Run(pollCtx, workCtx context.Context) {
	var (
		wg       sync.WaitGroup
		slots    = make(chan struct{}, c.cfg.Concurrency)
		failures int
	)

	defer wg.Wait()

	for pollCtx.Err() == nil {
		out, err := c.client.ReceiveMessage(pollCtx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queues.InputURL()),
			MaxNumberOfMessages: int32(min(c.cfg.Concurrency, 10)),
			WaitTimeSeconds:     int32(c.cfg.WaitTime / time.Second),
			VisibilityTimeout:   int32(c.cfg.VisibilityTimeout / time.Second),
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameApproximateReceiveCount,
				types.MessageSystemAttributeNameMessageGroupId,
			},
		})
		if err != nil {
			if pollCtx.Err() != nil {
				return
			}

			failures++
			c.logger.Warn("sqs receive failed", "error", err.Error(), "consecutiveFailures", failures)
			sleep(pollCtx, backoff(time.Second, 30*time.Second, failures))

			continue
		}

		failures = 0

		for i, msg := range out.Messages {
			select {
			case slots <- struct{}{}:
			case <-pollCtx.Done():
				for _, pending := range out.Messages[i:] {
					c.release(pending, 0)
				}

				return
			}

			wg.Add(1)

			go func() {
				defer wg.Done()
				defer func() { <-slots }()

				c.handle(workCtx, msg)
			}()
		}
	}
}

type outcome int

const (
	outcomeDone outcome = iota
	outcomeRetry
	outcomeDeadLetter
)

func (c *Consumer) handle(workCtx context.Context, msg types.Message) {
	ctx, cancel := context.WithTimeout(workCtx, c.cfg.HandlerTimeout)
	defer cancel()

	logger := c.logger.With("sqsMessageId", aws.ToString(msg.MessageId))
	result, reason, logger := c.process(ctx, msg, logger)

	switch result {
	case outcomeDone:
		if c.beforeDelete != nil {
			if err := c.beforeDelete(); err != nil {
				return
			}
		}

		if err := c.delete(msg); err != nil {
			logger.Warn("sqs delete failed; message will be redelivered", "error", err.Error())
		}
	case outcomeDeadLetter:
		c.metrics.SQSDeadLetter(reason)

		if err := c.deadLetter(msg, reason); err != nil {
			logger.Error("sqs dead-letter failed", "error", err.Error())
			c.release(msg, c.retryDelay(msg))

			return
		}

		logger.Warn("message sent to dead-letter queue", "reason", reason)
	case outcomeRetry:
		c.metrics.SQSRetry()

		delay := c.retryDelay(msg)
		if workCtx.Err() != nil {
			delay = 0
		}

		c.release(msg, delay)
	}
}

func (c *Consumer) process(ctx context.Context, msg types.Message, logger *slog.Logger) (outcome, string, *slog.Logger) {
	start := time.Now()
	body := aws.ToString(msg.Body)

	cmd, err := parseMessage(body)
	if err != nil {
		c.metrics.SQSMessage("invalid")
		logger.Warn("invalid message", "error", err.Error())

		return outcomeDeadLetter, "invalid_message", logger
	}

	logger = logger.With(
		"messageId", cmd.Inbox.MessageID,
		"correlationId", cmd.CorrelationID,
		"providerId", cmd.ProviderID,
		"walletId", cmd.WalletID,
	)

	res, err := c.service.Submit(ctx, cmd)

	switch {
	case err == nil:
		c.metrics.SQSMessage("processed")
		c.metrics.Operation(metrics.SourceSQS, string(res.Status), string(cmd.Kind),
			res.IdempotentReplay, res.Conflicts, time.Since(start))

		logger.Info("message processed",
			"transactionId", res.TransactionID,
			"status", string(res.Status),
			"failureCode", string(res.FailureCode),
			"idempotentReplay", res.IdempotentReplay,
		)

		return outcomeDone, "", logger
	case errors.Is(err, wagering.ErrInvalidInput):
		c.metrics.SQSMessage("invalid")
		logger.Warn("message rejected as invalid", "error", err.Error())

		return outcomeDeadLetter, "invalid_input", logger
	case errors.Is(err, wagering.ErrIdempotencyConflict):
		c.metrics.SQSMessage("conflict")
		c.metrics.Conflict("idempotency_key")

		return outcomeDeadLetter, "idempotency_key_conflict", logger
	case errors.Is(err, wagering.ErrExternalIDConflict):
		c.metrics.SQSMessage("conflict")
		c.metrics.Conflict("external_transaction")

		return outcomeDeadLetter, "external_transaction_conflict", logger
	case errors.Is(err, wagering.ErrMessageConflict):
		c.metrics.SQSMessage("conflict")
		c.metrics.Conflict("message_id")

		return outcomeDeadLetter, "message_id_conflict", logger
	default:
		c.metrics.SQSMessage("failed")
		logger.Warn("message processing failed; will retry", "error", err.Error())

		return outcomeRetry, "", logger
	}
}

type envelope struct {
	MessageID  string `json:"messageId"`
	Type       string `json:"type"`
	OccurredAt string `json:"occurredAt"`
	Data       *struct {
		ProviderID                     string           `json:"providerId"`
		ExternalTransactionID          string           `json:"externalTransactionId"`
		IdempotencyKey                 string           `json:"idempotencyKey"`
		PlayerID                       string           `json:"playerId"`
		WalletID                       string           `json:"walletId"`
		RoundID                        string           `json:"roundId"`
		GameID                         string           `json:"gameId"`
		Kind                           domain.WagerKind `json:"kind"`
		Money                          *domain.Money    `json:"money"`
		ReferenceExternalTransactionID string           `json:"referenceExternalTransactionId"`
	} `json:"data"`
}

func parseMessage(body string) (wagering.SubmitCommand, error) {
	var env envelope

	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return wagering.SubmitCommand{}, fmt.Errorf("malformed JSON: %w", err)
	}

	switch {
	case env.MessageID == "":
		return wagering.SubmitCommand{}, errors.New("messageId is required")
	case env.Type != MessageTypeWagerTransactionRequested:
		return wagering.SubmitCommand{}, fmt.Errorf("unsupported type %q", env.Type)
	case env.Data == nil:
		return wagering.SubmitCommand{}, errors.New("data is required")
	case env.Data.Money == nil:
		return wagering.SubmitCommand{}, errors.New("data.money is required")
	}

	if _, err := time.Parse(time.RFC3339, env.OccurredAt); err != nil {
		return wagering.SubmitCommand{}, fmt.Errorf("occurredAt must be RFC 3339: %w", err)
	}

	sum := sha256.Sum256([]byte(body))

	return wagering.SubmitCommand{
		ProviderID:                     env.Data.ProviderID,
		ExternalTransactionID:          env.Data.ExternalTransactionID,
		IdempotencyKey:                 env.Data.IdempotencyKey,
		PlayerID:                       env.Data.PlayerID,
		WalletID:                       env.Data.WalletID,
		RoundID:                        env.Data.RoundID,
		GameID:                         env.Data.GameID,
		Kind:                           env.Data.Kind,
		Money:                          *env.Data.Money,
		ReferenceExternalTransactionID: env.Data.ReferenceExternalTransactionID,
		CorrelationID:                  env.MessageID,
		CausationID:                    env.MessageID,
		Inbox: &wagering.InboxRef{
			ConsumerName: ConsumerName,
			MessageID:    env.MessageID,
			PayloadHash:  hex.EncodeToString(sum[:]),
		},
	}, nil
}

const cleanupTimeout = 5 * time.Second

func (c *Consumer) delete(msg types.Message) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	_, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queues.InputURL()),
		ReceiptHandle: msg.ReceiptHandle,
	})

	return err
}

func (c *Consumer) release(msg types.Message, delay time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	_, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(c.queues.InputURL()),
		ReceiptHandle:     msg.ReceiptHandle,
		VisibilityTimeout: int32(delay / time.Second),
	})
	if err != nil {
		c.logger.Warn("sqs change visibility failed", "error", err.Error())
	}
}

func (c *Consumer) deadLetter(msg types.Message, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	group := msg.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
	if group == "" {
		group = "dead-letter"
	}

	_, err := c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.queues.DeadLetterURL()),
		MessageBody:            msg.Body,
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: msg.MessageId,
		MessageAttributes: map[string]types.MessageAttributeValue{
			"failureReason": {DataType: aws.String("String"), StringValue: aws.String(reason)},
		},
	})
	if err != nil {
		return err
	}

	return c.delete(msg)
}

func (c *Consumer) retryDelay(msg types.Message) time.Duration {
	receives, err := strconv.Atoi(msg.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	if err != nil || receives < 1 {
		receives = 1
	}

	return backoff(c.cfg.RetryBaseDelay, c.cfg.RetryMaxDelay, receives)
}

func backoff(base, limit time.Duration, attempt int) time.Duration {
	delay := base

	for i := 1; i < attempt && delay < limit; i++ {
		delay *= 2
	}

	return min(delay, limit)
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/VictorGrano/backend-challenge-go/internal/metrics"
	"github.com/VictorGrano/backend-challenge-go/internal/postgres"
)

type PublisherConfig struct {
	BatchSize      int
	PollInterval   time.Duration
	Lease          time.Duration
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

func DefaultPublisherConfig() PublisherConfig {
	return PublisherConfig{
		BatchSize:      20,
		PollInterval:   500 * time.Millisecond,
		Lease:          30 * time.Second,
		RetryBaseDelay: time.Second,
		RetryMaxDelay:  time.Minute,
	}
}

func (c PublisherConfig) validate() error {
	if c.BatchSize < 1 || c.PollInterval <= 0 || c.Lease <= 0 ||
		c.RetryBaseDelay <= 0 || c.RetryMaxDelay < c.RetryBaseDelay {
		return fmt.Errorf("messaging: invalid publisher config: %+v", c)
	}

	return nil
}

type Publisher struct {
	client  *sqs.Client
	queues  *Queues
	outbox  *postgres.OutboxRepository
	metrics *metrics.Metrics
	logger  *slog.Logger
	cfg     PublisherConfig
	now     func() time.Time

	afterSend func(eventID string) error
}

func NewPublisher(
	client *sqs.Client,
	queues *Queues,
	outbox *postgres.OutboxRepository,
	m *metrics.Metrics,
	logger *slog.Logger,
	cfg PublisherConfig,
) (*Publisher, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &Publisher{
		client:  client,
		queues:  queues,
		outbox:  outbox,
		metrics: m,
		logger:  logger,
		cfg:     cfg,
		now:     func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
	}, nil
}

func (p *Publisher) Run(ctx context.Context) {
	failures := 0

	for ctx.Err() == nil {
		published, err := p.PublishBatch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			failures++
			p.logger.Warn("outbox publish round failed", "error", err.Error(), "consecutiveFailures", failures)
			sleep(ctx, backoff(time.Second, 30*time.Second, failures))

			continue
		}

		failures = 0
		p.reportLag(ctx)

		if published == 0 {
			sleep(ctx, p.cfg.PollInterval)
		}
	}
}

func (p *Publisher) PublishBatch(ctx context.Context) (int, error) {
	records, err := p.outbox.Claim(ctx, p.now(), p.cfg.BatchSize, p.cfg.Lease)
	if err != nil {
		return 0, err
	}

	for i, record := range records {
		if ctx.Err() != nil {
			p.releaseRecords(records[i:])
			break
		}

		p.publish(ctx, record)
	}

	return len(records), nil
}

func (p *Publisher) publish(ctx context.Context, record postgres.OutboxRecord) {
	logger := p.logger.With("eventId", record.EventID, "eventType", record.EventType, "aggregateId", record.AggregateID)

	_, err := p.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(p.queues.EventsURL()),
		MessageBody:            aws.String(string(record.Payload)),
		MessageGroupId:         aws.String(record.AggregateID),
		MessageDeduplicationId: aws.String(record.EventID),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"eventType": {DataType: aws.String("String"), StringValue: aws.String(record.EventType)},
			"eventId":   {DataType: aws.String("String"), StringValue: aws.String(record.EventID)},
		},
	})
	if err != nil {
		p.metrics.OutboxPublished("failed")

		retryAt := p.now().Add(backoff(p.cfg.RetryBaseDelay, p.cfg.RetryMaxDelay, record.Attempts))
		logger.Warn("event publish failed", "error", err.Error(), "attempts", record.Attempts)

		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()

		if err := p.outbox.MarkFailed(cleanupCtx, record.EventID, retryAt, err.Error()); err != nil {
			logger.Warn("outbox mark failed failed; lease will expire", "error", err.Error())
		}

		return
	}

	if p.afterSend != nil {
		if err := p.afterSend(record.EventID); err != nil {
			return
		}
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if err := p.outbox.MarkPublished(cleanupCtx, record.EventID, p.now()); err != nil {
		logger.Warn("outbox mark published failed; event will be republished", "error", err.Error())
		return
	}

	p.metrics.OutboxPublished("published")
}

func (p *Publisher) releaseRecords(records []postgres.OutboxRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	for _, record := range records {
		if err := p.outbox.MarkFailed(ctx, record.EventID, p.now(), "released on shutdown"); err != nil {
			p.logger.Warn("outbox release failed; lease will expire", "eventId", record.EventID, "error", err.Error())
		}
	}
}

func (p *Publisher) reportLag(ctx context.Context) {
	lag, err := p.outbox.OldestPendingAge(ctx, p.now())
	if err != nil {
		return
	}

	p.metrics.OutboxLag(lag)
}

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
)

type OutboxRepository struct {
	db *DB
}

func NewOutboxRepository(db *DB) *OutboxRepository {
	return &OutboxRepository{db: db}
}

type OutboxRecord struct {
	EventID     string
	AggregateID string
	EventType   string
	Payload     []byte
	OccurredAt  time.Time
	Attempts    int
}

func (r *OutboxRepository) Insert(ctx context.Context, events ...domain.Event) error {
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("postgres: marshal event %s: %w", event.ID(), err)
		}

		_, err = r.db.q(ctx).Exec(ctx, `
			INSERT INTO outbox_events
				(event_id, aggregate_id, event_type, payload, occurred_at, next_attempt_at)
			VALUES ($1, $2, $3, $4, $5, $5)`,
			event.ID(), event.AggregateID(), string(event.Type()), string(payload), event.OccurredAt(),
		)
		if err != nil {
			if uniqueViolation(err) == "outbox_events_pkey" {
				return domain.ErrDuplicateEvent
			}

			return fmt.Errorf("postgres: insert outbox event: %w", err)
		}
	}

	return nil
}

func (r *OutboxRepository) Claim(
	ctx context.Context,
	now time.Time,
	limit int,
	lease time.Duration,
) ([]OutboxRecord, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		UPDATE outbox_events o
		   SET attempts = o.attempts + 1, next_attempt_at = $3
		  FROM (
			SELECT event_id
			  FROM outbox_events
			 WHERE published_at IS NULL AND next_attempt_at <= $1
			 ORDER BY next_attempt_at, seq
			 LIMIT $2
			   FOR UPDATE SKIP LOCKED
		  ) due
		 WHERE o.event_id = due.event_id
		RETURNING o.seq, o.event_id, o.aggregate_id, o.event_type, o.payload, o.occurred_at, o.attempts`,
		now, limit, now.Add(lease),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: claim outbox: %w", err)
	}
	defer rows.Close()

	type claimed struct {
		seq    int64
		record OutboxRecord
	}

	var batch []claimed

	for rows.Next() {
		var c claimed

		if err := rows.Scan(&c.seq, &c.record.EventID, &c.record.AggregateID, &c.record.EventType,
			&c.record.Payload, &c.record.OccurredAt, &c.record.Attempts); err != nil {
			return nil, fmt.Errorf("postgres: claim outbox: %w", err)
		}

		batch = append(batch, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: claim outbox: %w", err)
	}

	sort.Slice(batch, func(i, j int) bool { return batch[i].seq < batch[j].seq })

	records := make([]OutboxRecord, len(batch))
	for i, c := range batch {
		records[i] = c.record
	}

	return records, nil
}

func (r *OutboxRepository) MarkPublished(ctx context.Context, eventID string, publishedAt time.Time) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		UPDATE outbox_events
		   SET published_at = $2, last_error = NULL
		 WHERE event_id = $1 AND published_at IS NULL`,
		eventID, publishedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: mark outbox published: %w", err)
	}

	return nil
}

func (r *OutboxRepository) MarkFailed(
	ctx context.Context,
	eventID string,
	nextAttemptAt time.Time,
	lastError string,
) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		UPDATE outbox_events
		   SET next_attempt_at = $2, last_error = $3
		 WHERE event_id = $1 AND published_at IS NULL`,
		eventID, nextAttemptAt, lastError,
	)
	if err != nil {
		return fmt.Errorf("postgres: mark outbox failed: %w", err)
	}

	return nil
}

func (r *OutboxRepository) OldestPendingAge(ctx context.Context, now time.Time) (time.Duration, error) {
	var oldest *time.Time

	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT MIN(occurred_at) FROM outbox_events WHERE published_at IS NULL`,
	).Scan(&oldest)
	if err != nil {
		return 0, fmt.Errorf("postgres: outbox lag: %w", err)
	}

	if oldest == nil || now.Before(*oldest) {
		return 0, nil
	}

	return now.Sub(*oldest), nil
}

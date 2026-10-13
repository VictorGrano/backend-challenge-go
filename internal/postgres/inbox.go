package postgres

import (
	"context"
	"fmt"
	"time"
)

type InboxRepository struct {
	db *DB
}

func NewInboxRepository(db *DB) *InboxRepository {
	return &InboxRepository{db: db}
}

type InboxMessage struct {
	ConsumerName  string
	MessageID     string
	PayloadHash   string
	TransactionID string
	ReceivedAt    time.Time
	CompletedAt   time.Time
}

func (m InboxMessage) Completed() bool { return !m.CompletedAt.IsZero() }

func (r *InboxRepository) Receive(
	ctx context.Context,
	consumerName string,
	messageID string,
	payloadHash string,
	receivedAt time.Time,
) (msg InboxMessage, inserted bool, err error) {
	tag, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		consumerName, messageID, payloadHash, receivedAt,
	)
	if err != nil {
		return InboxMessage{}, false, fmt.Errorf("postgres: receive inbox message: %w", err)
	}

	var (
		transactionID *string
		completedAt   *time.Time
	)

	msg = InboxMessage{ConsumerName: consumerName, MessageID: messageID}

	err = r.db.q(ctx).QueryRow(ctx, `
		SELECT payload_hash, transaction_id, received_at, completed_at
		  FROM inbox_messages
		 WHERE consumer_name = $1 AND message_id = $2
		   FOR UPDATE`,
		consumerName, messageID,
	).Scan(&msg.PayloadHash, &transactionID, &msg.ReceivedAt, &completedAt)
	if err != nil {
		return InboxMessage{}, false, fmt.Errorf("postgres: read inbox message: %w", err)
	}

	msg.TransactionID = deref(transactionID)
	if completedAt != nil {
		msg.CompletedAt = *completedAt
	}

	return msg, tag.RowsAffected() == 1, nil
}

func (r *InboxRepository) Complete(
	ctx context.Context,
	consumerName string,
	messageID string,
	transactionID string,
	completedAt time.Time,
) error {
	tag, err := r.db.q(ctx).Exec(ctx, `
		UPDATE inbox_messages
		   SET completed_at = $4, transaction_id = $3
		 WHERE consumer_name = $1 AND message_id = $2 AND completed_at IS NULL`,
		consumerName, messageID, nullable(transactionID), completedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: complete inbox message: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: inbox message %s/%s not pending", consumerName, messageID)
	}

	return nil
}

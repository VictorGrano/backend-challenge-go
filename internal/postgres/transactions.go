package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
)

type WagerTransactionRepository struct {
	db *DB
}

func NewWagerTransactionRepository(db *DB) *WagerTransactionRepository {
	return &WagerTransactionRepository{db: db}
}

type ReferenceSchedule struct {
	Attempts      int
	NextAttemptAt time.Time
}

func (s ReferenceSchedule) nextAttemptAt() any {
	if s.NextAttemptAt.IsZero() {
		return nil
	}

	return s.NextAttemptAt
}

type PendingReference struct {
	Transaction *domain.WagerTransaction
	Attempts    int
}

const transactionColumns = `id, kind, status, wallet_id, player_id, amount, currency,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id, failure_code, result_balance,
	created_at, updated_at`

func (r *WagerTransactionRepository) Insert(
	ctx context.Context,
	tx *domain.WagerTransaction,
	schedule ReferenceSchedule,
) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO wager_transactions (`+transactionColumns+`,
			reference_attempts, next_reference_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
			$18, $19, $20, $21)`,
		tx.ID(), string(tx.Kind()), string(tx.Status()), tx.WalletID(), tx.PlayerID(),
		tx.Money().Amount(), string(tx.Money().Currency()),
		nullable(tx.ProviderID()), nullable(tx.ExternalTransactionID()),
		nullable(tx.IdempotencyKey()), nullable(tx.PayloadHash()),
		nullable(tx.RoundID()), nullable(tx.GameID()),
		nullable(tx.ReferenceExternalTransactionID()), nullable(tx.ReferenceTransactionID()),
		nullable(string(tx.FailureCode())), resultBalance(tx),
		tx.CreatedAt(), tx.UpdatedAt(),
		schedule.Attempts, schedule.nextAttemptAt(),
	)
	if err != nil {
		return mapTransactionError("insert", err)
	}

	return nil
}

func (r *WagerTransactionRepository) Update(
	ctx context.Context,
	tx *domain.WagerTransaction,
	schedule ReferenceSchedule,
) error {
	tag, err := r.db.q(ctx).Exec(ctx, `
		UPDATE wager_transactions
		   SET status = $2,
		       reference_transaction_id = $3,
		       failure_code = $4,
		       result_balance = $5,
		       reference_attempts = $6,
		       next_reference_attempt_at = $7,
		       updated_at = $8
		 WHERE id = $1`,
		tx.ID(), string(tx.Status()), nullable(tx.ReferenceTransactionID()),
		nullable(string(tx.FailureCode())), resultBalance(tx),
		schedule.Attempts, schedule.nextAttemptAt(), tx.UpdatedAt(),
	)
	if err != nil {
		return mapTransactionError("update", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrTransactionNotFound
	}

	return nil
}

func (r *WagerTransactionRepository) Get(ctx context.Context, id string) (*domain.WagerTransaction, error) {
	return r.getOne(ctx, `WHERE id = $1`, id)
}

func (r *WagerTransactionRepository) GetByExternalID(
	ctx context.Context,
	providerID string,
	externalTransactionID string,
) (*domain.WagerTransaction, error) {
	return r.getOne(ctx,
		`WHERE provider_id = $1 AND external_transaction_id = $2`,
		providerID, externalTransactionID,
	)
}

func (r *WagerTransactionRepository) GetByIdempotencyKey(
	ctx context.Context,
	providerID string,
	idempotencyKey string,
) (*domain.WagerTransaction, error) {
	return r.getOne(ctx,
		`WHERE provider_id = $1 AND idempotency_key = $2`,
		providerID, idempotencyKey,
	)
}

func (r *WagerTransactionRepository) FindSubmitted(
	ctx context.Context,
	providerID string,
	idempotencyKey string,
	externalTransactionID string,
) ([]*domain.WagerTransaction, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT `+transactionColumns+`, reference_attempts
		  FROM wager_transactions
		 WHERE provider_id = $1
		   AND (idempotency_key = $2 OR external_transaction_id = $3)`,
		providerID, idempotencyKey, externalTransactionID,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: find submitted transactions: %w", err)
	}
	defer rows.Close()

	var found []*domain.WagerTransaction

	for rows.Next() {
		p, err := scanTransaction(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: find submitted transactions: %w", err)
		}

		found = append(found, p.Transaction)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: find submitted transactions: %w", err)
	}

	return found, nil
}

func (r *WagerTransactionRepository) HasProcessedReversal(ctx context.Context, referenceID string) (bool, error) {
	var exists bool

	err := r.db.q(ctx).QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM wager_transactions
			 WHERE reference_transaction_id = $1
			   AND status = 'PROCESSED'
			   AND kind IN ('REFUND', 'ROLLBACK')
		)`,
		referenceID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("postgres: check reversal: %w", err)
	}

	return exists, nil
}

func (r *WagerTransactionRepository) getOne(
	ctx context.Context,
	where string,
	args ...any,
) (*domain.WagerTransaction, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT `+transactionColumns+`, reference_attempts FROM wager_transactions `+where, args...,
	)

	pending, err := scanTransaction(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTransactionNotFound
		}

		return nil, fmt.Errorf("postgres: get wager transaction: %w", err)
	}

	return pending.Transaction, nil
}

func (r *WagerTransactionRepository) ClaimDuePendingReferences(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]PendingReference, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT `+transactionColumns+`, reference_attempts
		  FROM wager_transactions
		 WHERE status = 'PENDING_REFERENCE' AND next_reference_attempt_at <= $1
		 ORDER BY next_reference_attempt_at
		 LIMIT $2
		   FOR UPDATE SKIP LOCKED`,
		now, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: claim pending references: %w", err)
	}
	defer rows.Close()

	var pending []PendingReference

	for rows.Next() {
		p, err := scanTransaction(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: claim pending references: %w", err)
		}

		pending = append(pending, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: claim pending references: %w", err)
	}

	return pending, nil
}

func scanTransaction(row pgx.Row) (PendingReference, error) {
	var (
		s                    domain.WagerTransactionState
		kind, status         string
		amount               int64
		currency             string
		providerID           *string
		externalID           *string
		idempotencyKey       *string
		payloadHash          *string
		roundID, gameID      *string
		referenceExternalID  *string
		referenceID          *string
		failureCode          *string
		resultBalanceAmount  *int64
		createdAt, updatedAt time.Time
		attempts             int
	)

	err := row.Scan(
		&s.ID, &kind, &status, &s.WalletID, &s.PlayerID, &amount, &currency,
		&providerID, &externalID, &idempotencyKey, &payloadHash, &roundID, &gameID,
		&referenceExternalID, &referenceID, &failureCode, &resultBalanceAmount,
		&createdAt, &updatedAt, &attempts,
	)
	if err != nil {
		return PendingReference{}, err
	}

	money, err := domain.NewMoney(amount, domain.Currency(currency))
	if err != nil {
		return PendingReference{}, err
	}

	s.Kind = domain.WagerKind(kind)
	s.Status = domain.WagerStatus(status)
	s.Money = money
	s.ProviderID = deref(providerID)
	s.ExternalTransactionID = deref(externalID)
	s.IdempotencyKey = deref(idempotencyKey)
	s.PayloadHash = deref(payloadHash)
	s.RoundID = deref(roundID)
	s.GameID = deref(gameID)
	s.ReferenceExternalTransactionID = deref(referenceExternalID)
	s.ReferenceTransactionID = deref(referenceID)
	s.FailureCode = domain.FailureCode(deref(failureCode))
	s.CreatedAt = createdAt
	s.UpdatedAt = updatedAt

	if resultBalanceAmount != nil {
		balance, err := domain.NewMoney(*resultBalanceAmount, domain.Currency(currency))
		if err != nil {
			return PendingReference{}, err
		}

		s.ResultBalance = &balance
	}

	tx, err := domain.RehydrateWagerTransaction(s)
	if err != nil {
		return PendingReference{}, err
	}

	return PendingReference{Transaction: tx, Attempts: attempts}, nil
}

func resultBalance(tx *domain.WagerTransaction) any {
	balance, ok := tx.ResultBalance()
	if !ok {
		return nil
	}

	return balance.Amount()
}

func mapTransactionError(op string, err error) error {
	switch uniqueViolation(err) {
	case "wager_transactions_provider_external_key":
		return domain.ErrDuplicateTransaction
	case "wager_transactions_provider_idempotency_key":
		return domain.ErrDuplicateIdempotencyKey
	case "wager_transactions_opening_per_wallet":
		return domain.ErrOpeningAlreadyExists
	case "wager_transactions_single_reversal":
		return domain.ErrReferenceAlreadyReversed
	}

	return fmt.Errorf("postgres: %s wager transaction: %w", op, err)
}

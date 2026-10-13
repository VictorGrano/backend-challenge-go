package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
)

type LedgerRepository struct {
	db *DB
}

func NewLedgerRepository(db *DB) *LedgerRepository {
	return &LedgerRepository{db: db}
}

type LedgerRecord struct {
	Seq   int64
	Entry *domain.WalletLedgerEntry
}

type LedgerTotals struct {
	Balance int64
	Entries int64
}

func (r *LedgerRepository) Insert(ctx context.Context, e *domain.WalletLedgerEntry) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount, currency,
			 balance_before, balance_after, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.ID(), e.WalletID(), e.TransactionID(), string(e.Direction()),
		e.Money().Amount(), string(e.Money().Currency()),
		e.BalanceBefore().Amount(), e.BalanceAfter().Amount(), e.CreatedAt(),
	)
	if err != nil {
		if uniqueViolation(err) == "wallet_ledger_entries_wallet_transaction_key" {
			return domain.ErrDuplicateLedgerEntry
		}

		return fmt.Errorf("postgres: insert ledger entry: %w", err)
	}

	return nil
}

func (r *LedgerRepository) ListByWallet(
	ctx context.Context,
	walletID string,
	afterSeq int64,
	limit int,
) ([]LedgerRecord, error) {
	rows, err := r.db.q(ctx).Query(ctx, `
		SELECT seq, id, wallet_id, transaction_id, direction, amount, currency,
		       balance_before, balance_after, created_at
		  FROM wallet_ledger_entries
		 WHERE wallet_id = $1 AND seq > $2
		 ORDER BY seq
		 LIMIT $3`,
		walletID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: list ledger: %w", err)
	}
	defer rows.Close()

	var records []LedgerRecord

	for rows.Next() {
		var (
			seq                            int64
			id, wallet, transaction        string
			direction, currency            string
			amount, balanceBefore, balance int64
			createdAt                      time.Time
		)

		if err := rows.Scan(&seq, &id, &wallet, &transaction, &direction, &amount, &currency,
			&balanceBefore, &balance, &createdAt); err != nil {
			return nil, fmt.Errorf("postgres: list ledger: %w", err)
		}

		entry, err := rehydrateEntry(id, wallet, transaction, direction, currency,
			amount, balanceBefore, balance, createdAt)
		if err != nil {
			return nil, err
		}

		records = append(records, LedgerRecord{Seq: seq, Entry: entry})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list ledger: %w", err)
	}

	return records, nil
}

func (r *LedgerRepository) Totals(ctx context.Context, walletID string) (LedgerTotals, error) {
	var totals LedgerTotals

	err := r.db.q(ctx).QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount ELSE -amount END), 0)::BIGINT,
		       COUNT(*)
		  FROM wallet_ledger_entries
		 WHERE wallet_id = $1`,
		walletID,
	).Scan(&totals.Balance, &totals.Entries)
	if err != nil {
		return LedgerTotals{}, fmt.Errorf("postgres: ledger totals: %w", err)
	}

	return totals, nil
}

func rehydrateEntry(
	id, walletID, transactionID, direction, currency string,
	amount, balanceBefore, balanceAfter int64,
	createdAt time.Time,
) (*domain.WalletLedgerEntry, error) {
	cur := domain.Currency(currency)

	money, err := domain.NewMoney(amount, cur)
	if err != nil {
		return nil, err
	}

	before, err := domain.NewMoney(balanceBefore, cur)
	if err != nil {
		return nil, err
	}

	after, err := domain.NewMoney(balanceAfter, cur)
	if err != nil {
		return nil, err
	}

	return domain.RehydrateWalletLedgerEntry(
		id, walletID, transactionID, domain.LedgerDirection(direction),
		money, before, after, createdAt,
	)
}

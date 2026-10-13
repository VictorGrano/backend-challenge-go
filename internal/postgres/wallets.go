package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
)

type WalletRepository struct {
	db *DB
}

func NewWalletRepository(db *DB) *WalletRepository {
	return &WalletRepository{db: db}
}

const walletColumns = `id, player_id, currency, balance, version, created_at, updated_at`

func (r *WalletRepository) Insert(ctx context.Context, w *domain.Wallet) error {
	_, err := r.db.q(ctx).Exec(ctx, `
		INSERT INTO wallets (`+walletColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), string(w.Currency()), w.Balance().Amount(),
		w.Version(), w.CreatedAt(), w.UpdatedAt(),
	)
	if err != nil {
		if uniqueViolation(err) == "wallets_player_currency_key" {
			return domain.ErrWalletAlreadyExists
		}

		return fmt.Errorf("postgres: insert wallet: %w", err)
	}

	return nil
}

func (r *WalletRepository) Get(ctx context.Context, id string) (*domain.Wallet, error) {
	return r.get(ctx, id, "")
}

func (r *WalletRepository) GetForUpdate(ctx context.Context, id string) (*domain.Wallet, error) {
	return r.get(ctx, id, " FOR UPDATE")
}

func (r *WalletRepository) get(ctx context.Context, id, lock string) (*domain.Wallet, error) {
	var (
		walletID, playerID, currency string
		balance, version             int64
		createdAt, updatedAt         time.Time
	)

	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE id = $1`+lock, id,
	).Scan(&walletID, &playerID, &currency, &balance, &version, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWalletNotFound
		}

		return nil, fmt.Errorf("postgres: get wallet: %w", err)
	}

	money, err := domain.NewMoney(balance, domain.Currency(currency))
	if err != nil {
		return nil, err
	}

	return domain.RehydrateWallet(walletID, playerID, money, version, createdAt, updatedAt)
}

func (r *WalletRepository) UpdateBalance(ctx context.Context, w *domain.Wallet) error {
	tag, err := r.db.q(ctx).Exec(ctx, `
		UPDATE wallets
		   SET balance = $2, version = $3, updated_at = $4
		 WHERE id = $1 AND version = $3 - 1`,
		w.ID(), w.Balance().Amount(), w.Version(), w.UpdatedAt(),
	)
	if err != nil {
		return fmt.Errorf("postgres: update wallet: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrConcurrentUpdate
	}

	return nil
}

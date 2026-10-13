package wagering

import (
	"context"
	"fmt"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
	"github.com/VictorGrano/backend-challenge-go/internal/postgres"
)

func (s *Service) OpenWallet(
	ctx context.Context,
	playerID string,
	initialBalance domain.Money,
	correlationID string,
) (*domain.Wallet, error) {
	playerID, err := canonicalUUID("playerId", playerID)
	if err != nil {
		return nil, err
	}

	at := s.now()

	wallet, err := domain.NewWallet(s.newID(), playerID, initialBalance, at)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	if initialBalance.IsZero() {
		if err := s.wallets.Insert(ctx, wallet); err != nil {
			return nil, err
		}

		return wallet, nil
	}

	opening, err := domain.NewOpeningTransaction(s.newID(), wallet.ID(), playerID, initialBalance, at)
	if err != nil {
		return nil, err
	}

	if err := opening.MarkProcessed(wallet.Balance(), at); err != nil {
		return nil, err
	}

	zero, err := domain.ZeroMoney(initialBalance.Currency())
	if err != nil {
		return nil, err
	}

	entry, err := domain.NewWalletLedgerEntry(
		s.newID(), wallet.ID(), opening.ID(), domain.LedgerCredit, initialBalance, zero, initialBalance, at,
	)
	if err != nil {
		return nil, err
	}

	if correlationID == "" {
		correlationID = opening.ID()
	}

	meta := func() domain.EventMeta {
		return domain.EventMeta{EventID: s.newID(), CorrelationID: correlationID, OccurredAt: at}
	}

	processed, err := domain.NewWagerTransactionProcessedEvent(meta(), opening)
	if err != nil {
		return nil, err
	}

	changed, err := domain.NewWalletBalanceChangedEvent(meta(), wallet, entry)
	if err != nil {
		return nil, err
	}

	err = s.db.WithinTx(ctx, func(ctx context.Context) error {
		if err := s.wallets.Insert(ctx, wallet); err != nil {
			return err
		}

		if err := s.txs.Insert(ctx, opening, postgres.ReferenceSchedule{}); err != nil {
			return err
		}

		if err := s.ledger.Insert(ctx, entry); err != nil {
			return err
		}

		return s.outbox.Insert(ctx, processed, changed)
	})
	if err != nil {
		return nil, err
	}

	return wallet, nil
}

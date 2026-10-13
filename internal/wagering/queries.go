package wagering

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
)

const (
	DefaultLedgerPageSize = 50
	MaxLedgerPageSize     = 200
)

func (s *Service) GetWallet(ctx context.Context, walletID string) (*domain.Wallet, error) {
	walletID, err := canonicalUUID("walletId", walletID)
	if err != nil {
		return nil, err
	}

	return s.wallets.Get(ctx, walletID)
}

func (s *Service) GetTransaction(ctx context.Context, transactionID string) (*domain.WagerTransaction, error) {
	transactionID, err := canonicalUUID("transactionId", transactionID)
	if err != nil {
		return nil, err
	}

	return s.txs.Get(ctx, transactionID)
}

func (s *Service) GetProviderTransaction(
	ctx context.Context,
	providerID string,
	externalTransactionID string,
) (*domain.WagerTransaction, error) {
	return s.txs.GetByExternalID(ctx, providerID, externalTransactionID)
}

type LedgerPage struct {
	Entries    []*domain.WalletLedgerEntry
	NextCursor string
}

func (s *Service) GetLedger(ctx context.Context, walletID, cursor string, limit int) (LedgerPage, error) {
	walletID, err := canonicalUUID("walletId", walletID)
	if err != nil {
		return LedgerPage{}, err
	}

	if limit == 0 {
		limit = DefaultLedgerPageSize
	}

	if limit < 1 || limit > MaxLedgerPageSize {
		return LedgerPage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidInput, MaxLedgerPageSize)
	}

	afterSeq, err := decodeCursor(cursor)
	if err != nil {
		return LedgerPage{}, err
	}

	if _, err := s.wallets.Get(ctx, walletID); err != nil {
		return LedgerPage{}, err
	}

	records, err := s.ledger.ListByWallet(ctx, walletID, afterSeq, limit+1)
	if err != nil {
		return LedgerPage{}, err
	}

	var page LedgerPage

	if len(records) > limit {
		records = records[:limit]
		page.NextCursor = encodeCursor(records[limit-1].Seq)
	}

	page.Entries = make([]*domain.WalletLedgerEntry, len(records))
	for i, record := range records {
		page.Entries[i] = record.Entry
	}

	return page, nil
}

const cursorPrefix = "v1:"

func encodeCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatInt(seq, 10)))
}

func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || !strings.HasPrefix(string(raw), cursorPrefix) {
		return 0, fmt.Errorf("%w: invalid cursor", ErrInvalidInput)
	}

	seq, err := strconv.ParseInt(strings.TrimPrefix(string(raw), cursorPrefix), 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("%w: invalid cursor", ErrInvalidInput)
	}

	return seq, nil
}

type Reconciliation struct {
	WalletID          string
	StoredBalance     domain.Money
	CalculatedBalance domain.Money
	Difference        domain.Money
	Consistent        bool
	CheckedEntries    int64
}

func (s *Service) Reconcile(ctx context.Context, walletID string) (Reconciliation, error) {
	walletID, err := canonicalUUID("walletId", walletID)
	if err != nil {
		return Reconciliation{}, err
	}

	var rec Reconciliation

	err = s.db.WithinSnapshot(ctx, func(ctx context.Context) error {
		wallet, err := s.wallets.Get(ctx, walletID)
		if err != nil {
			return err
		}

		totals, err := s.ledger.Totals(ctx, walletID)
		if err != nil {
			return err
		}

		calculated, err := domain.NewMoney(totals.Balance, wallet.Currency())
		if err != nil {
			return err
		}

		difference, err := wallet.Balance().Sub(calculated)
		if err != nil {
			return err
		}

		rec = Reconciliation{
			WalletID:          wallet.ID(),
			StoredBalance:     wallet.Balance(),
			CalculatedBalance: calculated,
			Difference:        difference,
			Consistent:        difference.IsZero(),
			CheckedEntries:    totals.Entries,
		}

		return nil
	})
	if err != nil {
		return Reconciliation{}, err
	}

	return rec, nil
}

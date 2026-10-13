package wagering

import (
	"context"
	"errors"
	"fmt"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
	"github.com/VictorGrano/backend-challenge-go/internal/postgres"
)

type SubmitCommand struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           domain.WagerKind
	Money                          domain.Money
	ReferenceExternalTransactionID string

	CorrelationID string
	CausationID   string

	Inbox *InboxRef
}

type InboxRef struct {
	ConsumerName string
	MessageID    string
	PayloadHash  string
}

type Result struct {
	TransactionID    string
	Status           domain.WagerStatus
	Balance          *domain.Money
	FailureCode      domain.FailureCode
	IdempotentReplay bool
	Conflicts        int
}

func resultOf(tx *domain.WagerTransaction, replay bool) Result {
	res := Result{
		TransactionID:    tx.ID(),
		Status:           tx.Status(),
		FailureCode:      tx.FailureCode(),
		IdempotentReplay: replay,
	}

	if balance, ok := tx.ResultBalance(); ok {
		res.Balance = &balance
	}

	return res
}

const submitAttempts = 3

func (s *Service) Submit(ctx context.Context, cmd SubmitCommand) (Result, error) {
	cmd, hash, err := s.normalize(cmd)
	if err != nil {
		return Result{}, err
	}

	for attempt := 1; ; attempt++ {
		var res Result

		err := s.db.WithinTx(ctx, func(ctx context.Context) error {
			var err error
			res, err = s.submitTx(ctx, cmd, hash)

			return err
		})
		if err == nil {
			res.Conflicts = attempt - 1
			return res, nil
		}

		duplicate := errors.Is(err, domain.ErrDuplicateTransaction) ||
			errors.Is(err, domain.ErrDuplicateIdempotencyKey)
		if !duplicate || attempt == submitAttempts {
			return Result{}, err
		}
	}
}

func (s *Service) normalize(cmd SubmitCommand) (SubmitCommand, string, error) {
	var err error

	if cmd.PlayerID, err = canonicalUUID("playerId", cmd.PlayerID); err != nil {
		return cmd, "", err
	}

	if cmd.WalletID, err = canonicalUUID("walletId", cmd.WalletID); err != nil {
		return cmd, "", err
	}

	if _, err := s.newTransaction(cmd, "validation"); err != nil {
		return cmd, "", fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	if cmd.Inbox != nil && (cmd.Inbox.ConsumerName == "" || cmd.Inbox.MessageID == "" || cmd.Inbox.PayloadHash == "") {
		return cmd, "", fmt.Errorf("%w: incomplete inbox reference", ErrInvalidInput)
	}

	hash, err := domain.WagerPayload{
		ProviderID:                     cmd.ProviderID,
		ExternalTransactionID:          cmd.ExternalTransactionID,
		PlayerID:                       cmd.PlayerID,
		WalletID:                       cmd.WalletID,
		RoundID:                        cmd.RoundID,
		GameID:                         cmd.GameID,
		Kind:                           cmd.Kind,
		Money:                          cmd.Money,
		ReferenceExternalTransactionID: cmd.ReferenceExternalTransactionID,
	}.Hash()
	if err != nil {
		return cmd, "", fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	return cmd, hash, nil
}

func (s *Service) newTransaction(cmd SubmitCommand, hash string) (*domain.WagerTransaction, error) {
	return domain.NewWagerTransaction(domain.NewWagerTransactionParams{
		ID:                             s.newID(),
		ProviderID:                     cmd.ProviderID,
		ExternalTransactionID:          cmd.ExternalTransactionID,
		IdempotencyKey:                 cmd.IdempotencyKey,
		PayloadHash:                    hash,
		WalletID:                       cmd.WalletID,
		PlayerID:                       cmd.PlayerID,
		RoundID:                        cmd.RoundID,
		GameID:                         cmd.GameID,
		Kind:                           cmd.Kind,
		Money:                          cmd.Money,
		ReferenceExternalTransactionID: cmd.ReferenceExternalTransactionID,
		Now:                            s.now(),
	})
}

func (s *Service) submitTx(ctx context.Context, cmd SubmitCommand, hash string) (Result, error) {
	completeInbox := false

	if cmd.Inbox != nil {
		msg, inserted, err := s.inbox.Receive(
			ctx, cmd.Inbox.ConsumerName, cmd.Inbox.MessageID, cmd.Inbox.PayloadHash, s.now(),
		)
		if err != nil {
			return Result{}, err
		}

		if !inserted && msg.PayloadHash != cmd.Inbox.PayloadHash {
			return Result{}, ErrMessageConflict
		}

		if msg.Completed() && msg.TransactionID != "" {
			tx, err := s.txs.Get(ctx, msg.TransactionID)
			if err != nil {
				return Result{}, err
			}

			return resultOf(tx, true), nil
		}

		completeInbox = !msg.Completed()
	}

	finish := func(tx *domain.WagerTransaction, replay bool) (Result, error) {
		if completeInbox {
			err := s.inbox.Complete(ctx, cmd.Inbox.ConsumerName, cmd.Inbox.MessageID, tx.ID(), s.now())
			if err != nil {
				return Result{}, err
			}
		}

		return resultOf(tx, replay), nil
	}

	existing, err := s.findExisting(ctx, cmd, hash)
	if err != nil {
		return Result{}, err
	}

	if existing != nil {
		return finish(existing, true)
	}

	wallet, err := s.wallets.GetForUpdate(ctx, cmd.WalletID)
	if err != nil {
		return Result{}, err
	}

	existing, err = s.findExisting(ctx, cmd, hash)
	if err != nil {
		return Result{}, err
	}

	if existing != nil {
		return finish(existing, true)
	}

	tx, err := s.newTransaction(cmd, hash)
	if err != nil {
		return Result{}, err
	}

	correlationID := cmd.CorrelationID
	if correlationID == "" {
		correlationID = tx.ID()
	}

	fx, err := s.process(ctx, wallet, tx, 0, correlationID, cmd.CausationID)
	if err != nil {
		return Result{}, err
	}

	if err := s.persist(ctx, true, wallet, tx, fx); err != nil {
		return Result{}, err
	}

	return finish(tx, false)
}

func (s *Service) findExisting(ctx context.Context, cmd SubmitCommand, hash string) (*domain.WagerTransaction, error) {
	found, err := s.txs.FindSubmitted(ctx, cmd.ProviderID, cmd.IdempotencyKey, cmd.ExternalTransactionID)
	if err != nil || len(found) == 0 {
		return nil, err
	}

	for _, tx := range found {
		if tx.IdempotencyKey() != cmd.IdempotencyKey {
			continue
		}

		if tx.PayloadHash() != hash {
			return nil, ErrIdempotencyConflict
		}

		return tx, nil
	}

	return nil, ErrExternalIDConflict
}

type effects struct {
	schedule postgres.ReferenceSchedule
	entry    *domain.WalletLedgerEntry
	events   []domain.Event
}

func (s *Service) process(
	ctx context.Context,
	wallet *domain.Wallet,
	tx *domain.WagerTransaction,
	attempts int,
	correlationID string,
	causationID string,
) (effects, error) {
	at := s.now()
	retry := tx.Status() == domain.WagerPendingReference

	if retry {
		attempts++
	}

	fx := effects{schedule: postgres.ReferenceSchedule{Attempts: attempts}}

	meta := func() domain.EventMeta {
		return domain.EventMeta{
			EventID:       s.newID(),
			CorrelationID: correlationID,
			CausationID:   causationID,
			OccurredAt:    at,
		}
	}

	reject := func(code domain.FailureCode) (effects, error) {
		if err := tx.Reject(code, at); err != nil {
			return effects{}, err
		}

		event, err := domain.NewWagerTransactionRejectedEvent(meta(), tx)
		if err != nil {
			return effects{}, err
		}

		fx.events = append(fx.events, event)

		return fx, nil
	}

	wait := func(code domain.FailureCode) (effects, error) {
		if !retry {
			if err := tx.MarkPendingReference(at); err != nil {
				return effects{}, err
			}

			event, err := domain.NewWagerTransactionPendingReferenceEvent(meta(), tx)
			if err != nil {
				return effects{}, err
			}

			fx.events = append(fx.events, event)
			fx.schedule.NextAttemptAt = at.Add(s.backoff(0))

			return fx, nil
		}

		if attempts >= s.cfg.ReferenceMaxAttempts {
			return reject(code)
		}

		fx.schedule.NextAttemptAt = at.Add(s.backoff(attempts))

		return fx, nil
	}

	if wallet.PlayerID() != tx.PlayerID() {
		return reject(domain.FailureWalletMismatch)
	}

	if wallet.Currency() != tx.Money().Currency() {
		return reject(domain.FailureCurrencyMismatch)
	}

	var reference *domain.WagerTransaction

	if tx.ReferenceExternalTransactionID() != "" {
		var err error

		reference, err = s.txs.GetByExternalID(ctx, tx.ProviderID(), tx.ReferenceExternalTransactionID())
		if errors.Is(err, domain.ErrTransactionNotFound) {
			return wait(domain.FailureReferenceNotFound)
		}

		if err != nil {
			return effects{}, err
		}

		switch reference.Status() {
		case domain.WagerRejected, domain.WagerFailed:
			return reject(domain.FailureReferenceNotProcessed)
		case domain.WagerPending, domain.WagerPendingReference:
			return wait(domain.FailureReferenceNotProcessed)
		}

		if !referenceMatches(tx, reference) {
			return reject(domain.FailureReferenceMismatch)
		}

		if tx.Kind().RequiresReference() {
			reversed, err := s.txs.HasProcessedReversal(ctx, reference.ID())
			if err != nil {
				return effects{}, err
			}

			if reversed {
				return reject(domain.FailureReferenceAlreadyReversed)
			}
		}

		if err := tx.ResolveReference(reference.ID(), at); err != nil {
			return effects{}, err
		}
	}

	var err error

	switch movement(tx.Kind(), reference) {
	case domain.LedgerDebit:
		fx.entry, err = wallet.Debit(s.newID(), tx.ID(), tx.Money(), at)
		if errors.Is(err, domain.ErrInsufficientFunds) {
			if tx.Kind() == domain.WagerBet {
				return reject(domain.FailureInsufficientFunds)
			}

			return reject(domain.FailureReversalInsufficientFunds)
		}
	case domain.LedgerCredit:
		fx.entry, err = wallet.Credit(s.newID(), tx.ID(), tx.Money(), at)
	}

	if err != nil {
		return effects{}, err
	}

	if err := tx.MarkProcessed(wallet.Balance(), at); err != nil {
		return effects{}, err
	}

	processed, err := domain.NewWagerTransactionProcessedEvent(meta(), tx)
	if err != nil {
		return effects{}, err
	}

	fx.events = append(fx.events, processed)

	if fx.entry != nil {
		changed, err := domain.NewWalletBalanceChangedEvent(meta(), wallet, fx.entry)
		if err != nil {
			return effects{}, err
		}

		fx.events = append(fx.events, changed)
	}

	return fx, nil
}

func referenceMatches(tx, reference *domain.WagerTransaction) bool {
	if reference.PlayerID() != tx.PlayerID() ||
		reference.WalletID() != tx.WalletID() ||
		reference.RoundID() != tx.RoundID() ||
		reference.Money().Currency() != tx.Money().Currency() {
		return false
	}

	switch tx.Kind() {
	case domain.WagerRollback:
		switch reference.Kind() {
		case domain.WagerBet, domain.WagerWin, domain.WagerRefund:
		default:
			return false
		}
	default:
		if reference.Kind() != domain.WagerBet {
			return false
		}
	}

	if tx.Kind().RequiresReference() && reference.Money() != tx.Money() {
		return false
	}

	return true
}

func movement(kind domain.WagerKind, reference *domain.WagerTransaction) domain.LedgerDirection {
	switch kind {
	case domain.WagerBet:
		return domain.LedgerDebit
	case domain.WagerWin, domain.WagerRefund:
		return domain.LedgerCredit
	case domain.WagerRollback:
		if reference.Kind() == domain.WagerBet {
			return domain.LedgerCredit
		}

		return domain.LedgerDebit
	default:
		return ""
	}
}

func (s *Service) persist(
	ctx context.Context,
	isNew bool,
	wallet *domain.Wallet,
	tx *domain.WagerTransaction,
	fx effects,
) error {
	var err error

	if isNew {
		err = s.txs.Insert(ctx, tx, fx.schedule)
	} else {
		err = s.txs.Update(ctx, tx, fx.schedule)
	}

	if err != nil {
		return err
	}

	if fx.entry != nil {
		if err := s.ledger.Insert(ctx, fx.entry); err != nil {
			return err
		}

		if err := s.wallets.UpdateBalance(ctx, wallet); err != nil {
			return err
		}
	}

	return s.outbox.Insert(ctx, fx.events...)
}

func (s *Service) ResolvePendingReferences(ctx context.Context, limit int) (int, error) {
	handled := 0

	for handled < limit {
		found := false

		err := s.db.WithinTx(ctx, func(ctx context.Context) error {
			pending, err := s.txs.ClaimDuePendingReferences(ctx, s.now(), 1)
			if err != nil || len(pending) == 0 {
				return err
			}

			found = true
			tx := pending[0].Transaction

			wallet, err := s.wallets.GetForUpdate(ctx, tx.WalletID())
			if err != nil {
				return err
			}

			fx, err := s.process(ctx, wallet, tx, pending[0].Attempts, tx.ID(), "")
			if err != nil {
				return err
			}

			return s.persist(ctx, false, wallet, tx, fx)
		})
		if err != nil {
			return handled, err
		}

		if !found {
			break
		}

		handled++
	}

	return handled, nil
}

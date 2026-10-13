package wagering

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/VictorGrano/backend-challenge-go/internal/postgres"
)

var (
	ErrInvalidInput        = errors.New("invalid input")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different payload")
	ErrExternalIDConflict  = errors.New("external transaction already submitted with another idempotency key")
	ErrMessageConflict     = errors.New("message id reused with a different payload")
)

type Config struct {
	ReferenceMaxAttempts int
	ReferenceBaseBackoff time.Duration
	ReferenceMaxBackoff  time.Duration
}

func DefaultConfig() Config {
	return Config{
		ReferenceMaxAttempts: 6,
		ReferenceBaseBackoff: 2 * time.Second,
		ReferenceMaxBackoff:  time.Minute,
	}
}

func (c Config) validate() error {
	if c.ReferenceMaxAttempts < 1 || c.ReferenceBaseBackoff <= 0 || c.ReferenceMaxBackoff < c.ReferenceBaseBackoff {
		return fmt.Errorf("wagering: invalid reference retry config: %+v", c)
	}

	return nil
}

type Service struct {
	db      *postgres.DB
	wallets *postgres.WalletRepository
	txs     *postgres.WagerTransactionRepository
	ledger  *postgres.LedgerRepository
	inbox   *postgres.InboxRepository
	outbox  *postgres.OutboxRepository
	cfg     Config
	now     func() time.Time
	newID   func() string
}

func NewService(
	db *postgres.DB,
	wallets *postgres.WalletRepository,
	txs *postgres.WagerTransactionRepository,
	ledger *postgres.LedgerRepository,
	inbox *postgres.InboxRepository,
	outbox *postgres.OutboxRepository,
	cfg Config,
) (*Service, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &Service{
		db:      db,
		wallets: wallets,
		txs:     txs,
		ledger:  ledger,
		inbox:   inbox,
		outbox:  outbox,
		cfg:     cfg,
		now:     systemNow,
		newID:   newUUIDv7,
	}, nil
}

func (s *Service) WithClock(now func() time.Time) *Service {
	clone := *s
	clone.now = now

	return &clone
}

func systemNow() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func newUUIDv7() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}

	return id.String()
}

func canonicalUUID(field, value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || len(value) != 36 {
		return "", fmt.Errorf("%w: %s must be a UUID", ErrInvalidInput, field)
	}

	return id.String(), nil
}

func (s *Service) backoff(attempt int) time.Duration {
	delay := s.cfg.ReferenceBaseBackoff

	for i := 0; i < attempt && delay < s.cfg.ReferenceMaxBackoff; i++ {
		delay *= 2
	}

	return min(delay, s.cfg.ReferenceMaxBackoff)
}

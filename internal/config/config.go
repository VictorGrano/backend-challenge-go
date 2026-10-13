package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr        string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration

	DatabaseURL string

	OIDCIssuer   string
	OIDCJWKSURL  string
	OIDCAudience string

	ReferenceMaxAttempts  int
	ReferenceBaseBackoff  time.Duration
	ReferenceMaxBackoff   time.Duration
	ReferencePollInterval time.Duration

	SQSInputQueue        string
	SQSDeadLetterQueue   string
	SQSEventsQueue       string
	SQSVisibilityTimeout time.Duration
	SQSConcurrency       int

	OutboxLease time.Duration
}

func Load() (Config, error) {
	l := loader{}

	cfg := Config{
		HTTPAddr:             l.str("HTTP_ADDR", ":8080"),
		RequestTimeout:       l.duration("HTTP_REQUEST_TIMEOUT", 10*time.Second),
		ShutdownTimeout:      l.duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		DatabaseURL:          l.required("DATABASE_URL"),
		OIDCIssuer:           l.required("OIDC_ISSUER"),
		OIDCJWKSURL:          l.required("OIDC_JWKS_URL"),
		OIDCAudience:         l.required("OIDC_AUDIENCE"),
		ReferenceMaxAttempts: l.integer("REFERENCE_MAX_ATTEMPTS", 6),
		ReferenceBaseBackoff: l.duration("REFERENCE_BASE_BACKOFF", 2*time.Second),
		ReferenceMaxBackoff:  l.duration("REFERENCE_MAX_BACKOFF", time.Minute),

		ReferencePollInterval: l.duration("REFERENCE_POLL_INTERVAL", time.Second),
		SQSInputQueue:         l.str("SQS_INPUT_QUEUE", "wager-transactions.fifo"),
		SQSDeadLetterQueue:    l.str("SQS_DEAD_LETTER_QUEUE", "wager-transactions-dlq.fifo"),
		SQSEventsQueue:        l.str("SQS_EVENTS_QUEUE", "wager-events.fifo"),
		SQSVisibilityTimeout:  l.duration("SQS_VISIBILITY_TIMEOUT", 30*time.Second),
		SQSConcurrency:        l.integer("SQS_CONCURRENCY", 8),
		OutboxLease:           l.duration("OUTBOX_LEASE", 30*time.Second),
	}

	if cfg.ReferencePollInterval <= 0 {
		l.fail("REFERENCE_POLL_INTERVAL must be positive")
	}

	if cfg.SQSVisibilityTimeout < 2*time.Second {
		l.fail("SQS_VISIBILITY_TIMEOUT must be at least 2s")
	}

	if cfg.SQSConcurrency < 1 {
		l.fail("SQS_CONCURRENCY must be at least 1")
	}

	if cfg.OutboxLease <= 0 {
		l.fail("OUTBOX_LEASE must be positive")
	}

	if os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
		l.fail("AWS_REGION is required")
	}

	if cfg.RequestTimeout <= 0 {
		l.fail("HTTP_REQUEST_TIMEOUT must be positive")
	}

	if cfg.ShutdownTimeout <= 0 {
		l.fail("SHUTDOWN_TIMEOUT must be positive")
	}

	if len(l.problems) > 0 {
		return Config{}, fmt.Errorf("config: %w", errors.New(strings.Join(l.problems, "; ")))
	}

	return cfg, nil
}

type loader struct {
	problems []string
}

func (l *loader) fail(problem string) {
	l.problems = append(l.problems, problem)
}

func (l *loader) str(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}

	return fallback
}

func (l *loader) required(key string) string {
	value := l.str(key, "")
	if value == "" {
		l.fail(key + " is required")
	}

	return value
}

func (l *loader) duration(key string, fallback time.Duration) time.Duration {
	raw := l.str(key, "")
	if raw == "" {
		return fallback
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		l.fail(fmt.Sprintf("%s must be a duration like 10s: %v", key, err))
		return fallback
	}

	return value
}

func (l *loader) integer(key string, fallback int) int {
	raw := l.str(key, "")
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		l.fail(fmt.Sprintf("%s must be an integer: %v", key, err))
		return fallback
	}

	return value
}

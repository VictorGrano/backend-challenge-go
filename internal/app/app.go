package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/VictorGrano/backend-challenge-go/internal/auth"
	"github.com/VictorGrano/backend-challenge-go/internal/config"
	"github.com/VictorGrano/backend-challenge-go/internal/httpapi"
	"github.com/VictorGrano/backend-challenge-go/internal/messaging"
	"github.com/VictorGrano/backend-challenge-go/internal/metrics"
	"github.com/VictorGrano/backend-challenge-go/internal/postgres"
	"github.com/VictorGrano/backend-challenge-go/internal/wagering"
)

var Module = fx.Options(
	ConfigModule,
	ObservabilityModule,
	PostgresModule,
	WageringModule,
	AuthModule,
	MessagingModule,
	HTTPModule,
	WorkersModule,
)

func New(extra ...fx.Option) *fx.App {
	return fx.New(append([]fx.Option{
		Module,
		fx.WithLogger(func(logger *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: logger}
		}),
	}, extra...)...)
}

var ConfigModule = fx.Module("config",
	fx.Provide(config.Load),
)

var ObservabilityModule = fx.Module("observability",
	fx.Provide(
		func() *slog.Logger {
			return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		},
		metrics.New,
	),
)

var PostgresModule = fx.Module("postgres",
	fx.Provide(
		newDB,
		postgres.NewWalletRepository,
		postgres.NewWagerTransactionRepository,
		postgres.NewLedgerRepository,
		postgres.NewInboxRepository,
		postgres.NewOutboxRepository,
	),
)

func newDB(lc fx.Lifecycle, cfg config.Config) (*postgres.DB, error) {
	db, err := postgres.Open(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := db.Ping(ctx); err != nil {
				return fmt.Errorf("postgres unavailable: %w", err)
			}

			return nil
		},
		OnStop: func(context.Context) error {
			db.Close()
			return nil
		},
	})

	return db, nil
}

var WageringModule = fx.Module("wagering",
	fx.Provide(
		func(cfg config.Config) wagering.Config {
			return wagering.Config{
				ReferenceMaxAttempts: cfg.ReferenceMaxAttempts,
				ReferenceBaseBackoff: cfg.ReferenceBaseBackoff,
				ReferenceMaxBackoff:  cfg.ReferenceMaxBackoff,
			}
		},
		wagering.NewService,
	),
)

var AuthModule = fx.Module("auth",
	fx.Provide(newAuthenticator),
)

func newAuthenticator(lc fx.Lifecycle, cfg config.Config) (auth.Authenticator, error) {
	ctx, cancel := context.WithCancel(context.Background())

	authenticator, err := auth.NewOIDCAuthenticator(ctx, cfg.OIDCIssuer, cfg.OIDCJWKSURL, cfg.OIDCAudience)
	if err != nil {
		cancel()
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			cancel()
			return nil
		},
	})

	return authenticator, nil
}

var MessagingModule = fx.Module("messaging",
	fx.Provide(
		func() (*sqs.Client, error) { return messaging.NewClient(context.Background()) },
		newQueues,
		func(cfg config.Config) messaging.ConsumerConfig {
			consumer := messaging.DefaultConsumerConfig()
			consumer.Concurrency = cfg.SQSConcurrency
			consumer.VisibilityTimeout = cfg.SQSVisibilityTimeout
			consumer.HandlerTimeout = cfg.SQSVisibilityTimeout / 2

			return consumer
		},
		func(cfg config.Config) messaging.PublisherConfig {
			publisher := messaging.DefaultPublisherConfig()
			publisher.Lease = cfg.OutboxLease

			return publisher
		},
		messaging.NewConsumer,
		messaging.NewPublisher,
	),
)

func newQueues(lc fx.Lifecycle, client *sqs.Client, cfg config.Config) (*messaging.Queues, error) {
	queues, err := messaging.NewQueues(client, messaging.QueueNames{
		Input:      cfg.SQSInputQueue,
		DeadLetter: cfg.SQSDeadLetterQueue,
		Events:     cfg.SQSEventsQueue,
	})
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{OnStart: queues.Resolve})

	return queues, nil
}

var HTTPModule = fx.Module("http",
	fx.Provide(
		func(db *postgres.DB, queues *messaging.Queues) []httpapi.ReadinessCheck {
			return []httpapi.ReadinessCheck{
				{Name: "postgres", Pinger: db},
				{Name: "sqs", Pinger: queues},
			}
		},
		func(
			service *wagering.Service,
			authenticator auth.Authenticator,
			m *metrics.Metrics,
			logger *slog.Logger,
			cfg config.Config,
			checks []httpapi.ReadinessCheck,
		) *httpapi.API {
			return httpapi.New(service, authenticator, m, logger, cfg.RequestTimeout, checks)
		},
	),
	fx.Invoke(startHTTPServer),
)

func startHTTPServer(
	lc fx.Lifecycle,
	shutdowner fx.Shutdowner,
	api *httpapi.API,
	cfg config.Config,
	logger *slog.Logger,
) {
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.RequestTimeout,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	done := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr)
			if err != nil {
				return fmt.Errorf("http listen: %w", err)
			}

			logger.Info("http server listening", "addr", listener.Addr().String())

			go func() {
				defer close(done)

				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("http server failed", "error", err.Error())
					_ = shutdowner.Shutdown(fx.ExitCode(1))
				}
			}()

			return nil
		},
		OnStop: func(ctx context.Context) error {
			err := server.Shutdown(ctx)

			select {
			case <-done:
			case <-ctx.Done():
			}

			logger.Info("http server stopped")

			return err
		},
	})
}

var WorkersModule = fx.Module("workers",
	fx.Invoke(startConsumer, startPublisher, startReferenceWorker),
)

func startConsumer(lc fx.Lifecycle, consumer *messaging.Consumer, logger *slog.Logger) {
	pollCtx, stopPolling := context.WithCancel(context.Background())
	workCtx, cancelWork := context.WithCancel(context.Background())
	done := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				consumer.Run(pollCtx, workCtx)
			}()

			logger.Info("worker started", "worker", "sqs-consumer")

			return nil
		},
		OnStop: func(ctx context.Context) error {
			defer cancelWork()
			stopPolling()

			grace := 10 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				grace = time.Until(deadline) / 2
			}

			timer := time.NewTimer(grace)
			defer timer.Stop()

			select {
			case <-done:
				logger.Info("worker stopped", "worker", "sqs-consumer", "drained", true)
				return nil
			case <-timer.C:
				logger.Warn("worker drain deadline reached; releasing in-flight messages", "worker", "sqs-consumer")
				cancelWork()
			case <-ctx.Done():
				cancelWork()
			}

			return waitWorker(ctx, done, "sqs-consumer", logger)
		},
	})
}

func startPublisher(lc fx.Lifecycle, publisher *messaging.Publisher, logger *slog.Logger) {
	runWorker(lc, "outbox-publisher", logger, publisher.Run)
}

func startReferenceWorker(
	lc fx.Lifecycle,
	service *wagering.Service,
	m *metrics.Metrics,
	cfg config.Config,
	logger *slog.Logger,
) {
	const batch = 50

	runWorker(lc, "pending-references", logger, func(ctx context.Context) {
		ticker := time.NewTicker(cfg.ReferencePollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			handled, err := service.ResolvePendingReferences(ctx, batch)
			m.ReferenceRetries(handled)

			if err != nil && ctx.Err() == nil {
				logger.Warn("pending references round failed", "error", err.Error())
			}
		}
	})
}

func runWorker(lc fx.Lifecycle, name string, logger *slog.Logger, run func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				run(ctx)
			}()

			logger.Info("worker started", "worker", name)

			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			return waitWorker(stopCtx, done, name, logger)
		},
	})
}

func waitWorker(ctx context.Context, done <-chan struct{}, name string, logger *slog.Logger) error {
	select {
	case <-done:
		logger.Info("worker stopped", "worker", name)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker %s did not stop in time: %w", name, ctx.Err())
	}
}

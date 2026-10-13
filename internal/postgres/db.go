package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}

type DB struct {
	pool *pgxpool.Pool
}

func Open(dsn string) (*DB, error) {
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}

	return &DB{pool: pool}, nil
}

func Connect(ctx context.Context, dsn string) (*DB, error) {
	db, err := Open(dsn)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	return db, nil
}

func IsTransient(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}

	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) == 5 {
		switch pgErr.Code[:2] {
		case "08", "40", "53", "57":
			return true
		}

		return false
	}

	var netErr net.Error

	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func (db *DB) Close() { db.pool.Close() }

func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

func (db *DB) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.withinTx(ctx, pgx.TxOptions{}, fn)
}

func (db *DB) WithinSnapshot(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.withinTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	}, fn)
}

func (db *DB) withinTx(ctx context.Context, opts pgx.TxOptions, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := db.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}

	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}

	return nil
}

func (db *DB) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}

	return db.pool
}

func uniqueViolation(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}

	return ""
}

func nullable(s string) any {
	if s == "" {
		return nil
	}

	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

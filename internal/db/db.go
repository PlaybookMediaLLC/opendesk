// Package db owns the Postgres pools and migrations.
package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/useopendesk/opendesk/migrations"
)

// Querier is satisfied by *pgxpool.Pool, pgx.Tx and *pgx.Conn.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type DB struct {
	// Pool is the pooled connection for request handlers (PgBouncer in transaction mode).
	Pool *pgxpool.Pool
	// Direct is unpooled, for River, LISTEN/NOTIFY and the event relay.
	Direct *pgxpool.Pool
	// Locks holds transaction-scoped advisory locks while handlers use Pool for their work.
	Locks *pgxpool.Pool
}

func Open(ctx context.Context, pooledURL, directURL string) (*DB, error) {
	pooledCfg, err := pgxpool.ParseConfig(pooledURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if pooledURL != directURL {
		// Transaction-mode poolers cannot keep named prepared statements.
		pooledCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	}
	pooledCfg.AfterConnect = scanUTC
	pool, err := pgxpool.NewWithConfig(ctx, pooledCfg)
	if err != nil {
		return nil, err
	}
	direct := pool
	if pooledURL != directURL {
		directCfg, err := pgxpool.ParseConfig(directURL)
		if err != nil {
			return nil, fmt.Errorf("parse DATABASE_URL_DIRECT: %w", err)
		}
		directCfg.MaxConns = 10
		directCfg.AfterConnect = scanUTC
		if direct, err = pgxpool.NewWithConfig(ctx, directCfg); err != nil {
			return nil, err
		}
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	lockCfg := pooledCfg.Copy()
	lockCfg.MaxConns = 4
	lockCfg.MinConns = 0
	lockCfg.MinIdleConns = 0
	locks, err := pgxpool.NewWithConfig(ctx, lockCfg)
	if err != nil {
		if direct != pool {
			direct.Close()
		}
		pool.Close()
		return nil, fmt.Errorf("open advisory lock pool: %w", err)
	}
	return &DB{Pool: pool, Direct: direct, Locks: locks}, nil
}

func (d *DB) Close() {
	d.Locks.Close()
	if d.Direct != d.Pool {
		d.Direct.Close()
	}
	d.Pool.Close()
}

// Tx runs fn in a transaction on the pooled connection. It runs fn once: fn may change
// state outside the transaction (a captured result, a NATS message), so a transaction that
// Postgres aborted is not retried here (see TxRetry).
func (d *DB) Tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, d.Pool, fn)
}

// txAttempts bounds TxRetry: the first run and two retries.
const txAttempts = 3

// TxRetry runs fn in a transaction like Tx, and runs it again, up to txAttempts times in
// all, when Postgres aborts the transaction for a deadlock or a serialization failure
// (IsRetryable). fn must be safe to run again from the start: everything it computes
// outside the transaction is reset at its top, and it sends nothing outside the database.
func (d *DB) TxRetry(ctx context.Context, fn func(tx pgx.Tx) error) error {
	var err error
	for attempt := 1; ; attempt++ {
		err = pgx.BeginFunc(ctx, d.Pool, fn)
		if err == nil || attempt >= txAttempts || !IsRetryable(err) {
			return err
		}
		// A short, growing, jittered pause lets the transaction that won finish.
		pause := time.Duration(attempt*attempt)*10*time.Millisecond + time.Duration(rand.Int64N(int64(10*time.Millisecond)))
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
	}
}

// IsRetryable reports an error that aborted a transaction which can succeed when run
// again: a deadlock (40P01) or a serialization failure (40001).
func IsRetryable(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "40P01" || pg.Code == "40001")
}

// Migrate applies app migrations and River's queue tables over the direct connection.
func Migrate(ctx context.Context, directURL string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, toPgx5Scheme(directURL))
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}

	pool, err := pgxpool.New(ctx, directURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("apply river migrations: %w", err)
	}
	return nil
}

func toPgx5Scheme(u string) string {
	for _, p := range []string{"postgresql://", "postgres://"} {
		if strings.HasPrefix(u, p) {
			return "pgx5://" + strings.TrimPrefix(u, p)
		}
	}
	return u
}

// IsUniqueViolation reports a unique constraint error, optionally for a named constraint.
func IsUniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return constraint == "" || pg.ConstraintName == constraint
	}
	return false
}

// IsNotFound reports pgx.ErrNoRows.
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// scanUTC makes timestamptz values scan as UTC, whatever the server or session zone.
func scanUTC(_ context.Context, c *pgx.Conn) error {
	c.TypeMap().RegisterType(&pgtype.Type{Name: "timestamptz", OID: pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC}})
	return nil
}

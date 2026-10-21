// Package pg owns the PostgreSQL connection pool.
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options configures the pool.
type Options struct {
	DSN            string
	MaxConns       int32
	MinConns       int32
	ConnectTimeout time.Duration
}

// DB is a thin wrapper around pgxpool.Pool.
type DB struct {
	pool *pgxpool.Pool
}

// Open creates a pool and verifies connectivity.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.DSN == "" {
		return nil, errors.New("database.dsn is required")
	}
	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns >= 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = opts.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	db := &DB{pool: pool}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout(opts.ConnectTimeout))
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

func connectTimeout(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return 8 * time.Second
}

// Pool exposes the underlying pool for packages that need transactions.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Ping checks that the database answers.
func (d *DB) Ping(ctx context.Context) error {
	return d.pool.Ping(ctx)
}

// AppliedMigration returns the highest applied schema version, or 0 when the
// migration bookkeeping table does not exist yet.
func (d *DB) AppliedMigration(ctx context.Context) (int64, error) {
	var version int64
	err := d.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P01" // undefined_table
	}
	return errors.Is(err, pgx.ErrNoRows)
}

// Close releases the pool.
func (d *DB) Close() {
	if d.pool != nil {
		d.pool.Close()
	}
}

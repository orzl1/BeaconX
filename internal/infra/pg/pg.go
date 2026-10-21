// Package pg 负责 PostgreSQL 连接池。
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

// Options 配置连接池。
type Options struct {
	DSN            string
	MaxConns       int32
	MinConns       int32
	ConnectTimeout time.Duration
}

// DB 是 pgxpool.Pool 的轻量封装。
type DB struct {
	pool *pgxpool.Pool
}

// Open 创建连接池并验证连通性。
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.DSN == "" {
		return nil, errors.New("database.dsn 为必填项")
	}
	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("解析 database dsn 失败：%w", err)
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
		return nil, fmt.Errorf("创建数据库连接池失败：%w", err)
	}
	db := &DB{pool: pool}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout(opts.ConnectTimeout))
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping 数据库失败：%w", err)
	}
	return db, nil
}

func connectTimeout(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return 8 * time.Second
}

// Pool 暴露底层连接池，供需要事务的包使用。
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Ping 检查数据库是否应答。
func (d *DB) Ping(ctx context.Context) error {
	return d.pool.Ping(ctx)
}

// AppliedMigration 返回已应用的最高 schema 版本；当迁移记录表尚不存在时
// 返回 0。
func (d *DB) AppliedMigration(ctx context.Context) (int64, error) {
	var version int64
	err := d.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取 schema 版本失败：%w", err)
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

// Close 释放连接池。
func (d *DB) Close() {
	if d.pool != nil {
		d.pool.Close()
	}
}

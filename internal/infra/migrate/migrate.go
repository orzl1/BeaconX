// Package migrate applies versioned SQL migrations.
//
// Rules:
//   - migrations are immutable once released; editing one is reported as drift
//   - files are applied inside a transaction each, in ascending version order
//   - a session advisory lock serialises concurrent starts (including several
//     replicas pointed at the same database)
//   - a migration that fails aborts startup; there is no "best effort" mode
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey serialises migration runs. Value is arbitrary but stable.
const advisoryLockKey int64 = 0x4243584D // "BCXM"

var filenamePattern = regexp.MustCompile(`^(\d{4})_([a-zA-Z0-9_\-]+)\.sql$`)

// Migration is one embedded SQL file.
type Migration struct {
	Version  int64
	Name     string
	SQL      string
	Checksum string
}

// Result reports what a run did.
type Result struct {
	Applied int
	Current int64
}

// Load reads and validates every migration in fsys.
func Load(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("no migrations found")
	}

	out := make([]Migration, 0, len(names))
	seen := map[int64]string{}
	for _, name := range names {
		base := path.Base(name)
		m := filenamePattern.FindStringSubmatch(base)
		if m == nil {
			return nil, fmt.Errorf("migration %q must match NNNN_name.sql", base)
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q has invalid version", base)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", version, prev, base)
		}
		seen[version] = base

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", base, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     base,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Latest returns the highest version in a loaded set.
func Latest(ms []Migration) int64 {
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].Version
}

// Apply brings the database up to date.
func Apply(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) (Result, error) {
	ms, err := Load(fsys)
	if err != nil {
		return Result{}, err
	}
	if pool == nil {
		return Result{}, errors.New("nil database pool")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return Result{}, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Best effort: the session ends anyway when the connection is released,
		// but unlocking makes the intent explicit and returns the lock sooner.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version     BIGINT PRIMARY KEY,
		    name        TEXT NOT NULL,
		    checksum    TEXT NOT NULL,
		    applied_at  timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return Result{}, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int64]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return Result{}, fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int64
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return Result{}, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("iterate schema_migrations: %w", err)
	}

	res := Result{Current: 0}
	for _, m := range ms {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return Result{}, fmt.Errorf(
					"migration %s checksum drift: database has %s, binary has %s (migrations are immutable)",
					m.Name, short(sum), short(m.Checksum))
			}
			res.Current = m.Version
			continue
		}
		if err := applyOne(ctx, conn, m); err != nil {
			return Result{}, err
		}
		res.Applied++
		res.Current = m.Version
	}
	return res, nil
}

// short truncates a checksum for log/error output without risking a panic on
// malformed or truncated stored values.
func short(sum string) string {
	const n = 12
	if len(sum) <= n {
		return sum
	}
	return sum[:n]
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("apply migration %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.Version, m.Name, m.Checksum); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("record migration %s: %w", m.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.Name, err)
	}
	return nil
}

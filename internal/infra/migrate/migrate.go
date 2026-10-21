// Package migrate 应用带版本的 SQL 迁移。
//
// 规则：
//   - 迁移一经发布即不可变更；修改已发布的迁移会被报告为漂移（drift）
//   - 每个文件在各自的事务中按版本升序执行
//   - 会话级 advisory lock 使并发启动串行化（包括多个副本指向同一数据库的情况）
//   - 某个迁移失败会中止启动；不存在“尽力而为”模式
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

// advisoryLockKey 使迁移执行串行化。取值任意，但需保持稳定。
const advisoryLockKey int64 = 0x4243584D // "BCXM"

var filenamePattern = regexp.MustCompile(`^(\d{4})_([a-zA-Z0-9_\-]+)\.sql$`)

// Migration 是一个内嵌的 SQL 文件。
type Migration struct {
	Version  int64
	Name     string
	SQL      string
	Checksum string
}

// Result 报告一次执行的结果。
type Result struct {
	Applied int
	Current int64
}

// Load 读取并校验 fsys 中的所有迁移。
func Load(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("列出迁移失败：%w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("未找到任何迁移")
	}

	out := make([]Migration, 0, len(names))
	seen := map[int64]string{}
	for _, name := range names {
		base := path.Base(name)
		m := filenamePattern.FindStringSubmatch(base)
		if m == nil {
			return nil, fmt.Errorf("迁移 %q 必须匹配 NNNN_name.sql", base)
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("迁移 %q 的版本号无效", base)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("迁移版本 %d 重复：%q 与 %q", version, prev, base)
		}
		seen[version] = base

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("读取迁移 %q 失败：%w", base, err)
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

// Latest 返回已加载集合中的最高版本。
func Latest(ms []Migration) int64 {
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].Version
}

// Apply 将数据库升级到最新版本。
func Apply(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) (Result, error) {
	ms, err := Load(fsys)
	if err != nil {
		return Result{}, err
	}
	if pool == nil {
		return Result{}, errors.New("数据库连接池为 nil")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("获取迁移连接失败：%w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return Result{}, fmt.Errorf("获取迁移锁失败：%w", err)
	}
	defer func() {
		// 尽力而为：连接释放时会话本身就会结束，但显式解锁能表达意图，
		// 并更早归还锁。
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
		return Result{}, fmt.Errorf("创建 schema_migrations 表失败：%w", err)
	}

	applied := map[int64]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return Result{}, fmt.Errorf("读取 schema_migrations 失败：%w", err)
	}
	for rows.Next() {
		var v int64
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return Result{}, fmt.Errorf("扫描 schema_migrations 失败：%w", err)
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("遍历 schema_migrations 失败：%w", err)
	}

	res := Result{Current: 0}
	for _, m := range ms {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return Result{}, fmt.Errorf(
					"迁移 %s 校验和漂移：数据库为 %s，二进制为 %s（迁移不可变更）",
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

// short 截断校验和以便日志/错误输出，同时避免因格式错误或被截断的存储值
// 触发 panic。
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
		return fmt.Errorf("开启迁移 %s 的事务失败：%w", m.Name, err)
	}
	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("执行迁移 %s 失败：%w", m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.Version, m.Name, m.Checksum); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("记录迁移 %s 失败：%w", m.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交迁移 %s 失败：%w", m.Name, err)
	}
	return nil
}

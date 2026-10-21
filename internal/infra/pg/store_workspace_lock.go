package pg

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
)

// 本文件实现「跨进程工作区锁」：用 PostgreSQL advisory lock 把同一
// (tenant, mission) 的工作区配额「统计 + 写入」包成临界区。
//
// 为什么需要它：workspace.Service 里的 per-mission mutex 只在单进程内有效，
// 多实例部署时两个进程会各自统计到「还有空间」然后同时写入，造成超卖。
// advisory lock 由数据库统一仲裁，锁随事务提交/回滚自动释放，无需额外的
// 锁表或迁移。
//
// 使用约定：
//   - fn 内只放「配额统计 + 文件写入」，不要放长时间等待（锁会占用连接）；
//   - 锁是 bigint 命名空间外的全局键，因此用 FNV-1a(tenant:mission) 生成，
//     不同任务之间碰撞概率可忽略；
//   - 事务失败/函数返回错误都会回滚并释放锁。

// ErrWorkspaceLockUnavailable 表示未配置数据库（无法提供跨进程锁）。
var ErrWorkspaceLockUnavailable = errors.New("数据库未就绪，无法获取跨进程工作区锁")

// WithWorkspaceLock 在事务内持有 advisory lock 执行 fn。
//
// 锁的获取使用 pg_advisory_xact_lock（事务级）：提交或回滚时自动释放，避免
// 应用崩溃后锁残留。fn 收到的是原 ctx，可在内部继续使用数据库（会从连接池
// 另取连接），但请避免在 fn 里再获取同一把锁。
func (s *Store) WithWorkspaceLock(ctx context.Context, tenantID, missionID int64, fn func(ctx context.Context) error) error {
	if tenantID <= 0 || missionID <= 0 {
		return fmt.Errorf("%w：租户或任务标识非法", ErrInvalidInput)
	}
	if s == nil || s.db == nil || s.db.pool == nil {
		return ErrWorkspaceLockUnavailable
	}
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启工作区锁事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	key := workspaceAdvisoryKey(tenantID, missionID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		return fmt.Errorf("获取工作区锁失败：%w", err)
	}
	if fn != nil {
		if err := fn(ctx); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交工作区锁事务失败：%w", err)
	}
	return nil
}

// workspaceAdvisoryKey 把 (tenant, mission) 映射为 advisory lock 的 bigint 键。
//
// 用 FNV-1a 而不是位拼接：tenant/mission 都是 int64，位拼接会丢掉高位；
// 哈希后的碰撞只影响「两个任务短暂串行」，不影响正确性。
func workspaceAdvisoryKey(tenantID, missionID int64) int64 {
	hasher := fnv.New64a()
	// 用固定分隔符避免 (1, 23) 与 (12, 3) 拼成同一个字符串。
	_, _ = fmt.Fprintf(hasher, "%d:%d", tenantID, missionID)
	return int64(hasher.Sum64())
}

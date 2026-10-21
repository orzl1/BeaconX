package pg

import (
	"context"
	"fmt"
	"time"
)

// 启动维护：LLM 正文保留期清理与 sync pending 记录恢复（B8）。
//
// 这些操作只在进程启动时执行一次（main 调用 RunStartupMaintenance），属于运维
// 兜底而非在线任务：失败只记录告警，不阻塞启动。

// MaintenanceResult 是一次启动维护的结果。
type MaintenanceResult struct {
	// LLMDeleted 是删除的过期 LLM 正文记录数。
	LLMDeleted int64
	// SyncPendingRecovered 是恢复为 failed 的超时 pending 同步运行数。
	SyncPendingRecovered int64
}

// RunStartupMaintenance 执行启动维护：清理过期 LLM 正文 + 恢复超时 pending 同步。
//
// 参数为 0 表示跳过对应操作；负数视为非法输入。
func (s *Store) RunStartupMaintenance(ctx context.Context, tenantID int64, llmRetention, syncPendingAfter time.Duration) (MaintenanceResult, error) {
	if err := checkTenant(tenantID); err != nil {
		return MaintenanceResult{}, err
	}
	var result MaintenanceResult
	if llmRetention < 0 || syncPendingAfter < 0 {
		return MaintenanceResult{}, fmt.Errorf("%w：维护时长不能为负数", ErrInvalidInput)
	}
	if llmRetention > 0 {
		deleted, err := s.DeleteOldLLMExchanges(ctx, tenantID, llmRetention)
		if err != nil {
			return result, err
		}
		result.LLMDeleted = deleted
	}
	if syncPendingAfter > 0 {
		recovered, err := s.RecoverStalePendingSyncRuns(ctx, tenantID, syncPendingAfter)
		if err != nil {
			return result, err
		}
		result.SyncPendingRecovered = recovered
	}
	return result, nil
}

// DeleteOldLLMExchanges 删除早于保留期的 LLM 正文采集记录。
//
// 只删 llm_exchanges（重正文），不动 llm_usage 计量；retention<=0 表示保留全部。
func (s *Store) DeleteOldLLMExchanges(ctx context.Context, tenantID int64, retention time.Duration) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	if retention <= 0 {
		return 0, nil
	}
	tag, err := s.db.pool.Exec(ctx, `
		DELETE FROM llm_exchanges
		WHERE tenant_id = $1
		  AND created_at < now() - make_interval(secs => $2::double precision)`,
		tenantID, retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("清理过期 LLM 正文失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RecoverStalePendingSyncRuns 把超时仍停在 pending 的同步运行标记为 failed。
//
// CreateSyncRun 直接写 running，pending 主要来自外部/历史数据；进程重启后这些
// 记录不会再被调度，统一收敛为 failed 避免前台永久显示「排队中」。
func (s *Store) RecoverStalePendingSyncRuns(ctx context.Context, tenantID int64, staleAfter time.Duration) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	if staleAfter <= 0 {
		return 0, fmt.Errorf("%w：恢复阈值必须大于 0", ErrInvalidInput)
	}
	tag, err := s.db.pool.Exec(ctx, `
		UPDATE sync_runs
		SET status = 'failed', error = $2, finished_at = now()
		WHERE tenant_id = $1 AND status = 'pending'
		  AND created_at < now() - make_interval(secs => $3::double precision)`,
		tenantID, "进程重启恢复", staleAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("恢复超时 pending 同步运行失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

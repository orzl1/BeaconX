-- 0003_runtime：任务生命周期、事件流与动作租约。
--
-- 说明：
--   * missions 增加排队/运行时间与结束原因，供准入调度与恢复使用；
--   * operation_nodes 增加优先级与租约列，worker 通过「租约 + SKIP LOCKED」领取动作，
--     进程崩溃后过期租约可被回收，避免永久 running；
--   * mission_events 是只追加事件流：SSE 回放、审计与状态追溯都以它为事实来源。

-- ---------- 任务生命周期 ----------

-- 任务状态词表在 M2 收敛为 queued/running/paused/finalizing/done/failed/cancelled。
-- 0001/0002 中的 draft/active 需要先迁移存量数据，再替换 CHECK 约束。
ALTER TABLE missions DROP CONSTRAINT IF EXISTS missions_status_check;
UPDATE missions SET status = 'queued' WHERE status = 'draft';
UPDATE missions SET status = 'running' WHERE status = 'active';
ALTER TABLE missions
    ADD CONSTRAINT missions_status_check
    CHECK (status IN ('queued', 'running', 'paused', 'finalizing', 'done', 'failed', 'cancelled'));
ALTER TABLE missions ALTER COLUMN status SET DEFAULT 'queued';

ALTER TABLE missions
    ADD COLUMN IF NOT EXISTS queued      boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS queued_at   timestamptz,
    ADD COLUMN IF NOT EXISTS started_at  timestamptz,
    ADD COLUMN IF NOT EXISTS finished_at timestamptz,
    ADD COLUMN IF NOT EXISTS reason      text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS deadline_at timestamptz;

-- 排队任务按进入队列时间先进先出；部分索引只覆盖真正排队的行。
CREATE INDEX IF NOT EXISTS idx_missions_queue
    ON missions (tenant_id, queued_at)
    WHERE queued;

CREATE INDEX IF NOT EXISTS idx_missions_status
    ON missions (tenant_id, status, updated_at DESC);

-- ---------- 动作优先级与租约 ----------

ALTER TABLE operation_nodes
    ADD COLUMN IF NOT EXISTS priority    integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS lease_until timestamptz,
    ADD COLUMN IF NOT EXISTS leased_by   text,
    ADD COLUMN IF NOT EXISTS attempts    integer NOT NULL DEFAULT 0;

-- 领取动作的查询路径：按优先级取最早的一条。
CREATE INDEX IF NOT EXISTS idx_nodes_claim
    ON operation_nodes (tenant_id, mission_id, priority DESC, id)
    WHERE state IN ('open', 'running');

-- ---------- 事件流 ----------

CREATE TABLE IF NOT EXISTS mission_events (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id BIGINT NOT NULL,
    type       TEXT NOT NULL,
    actor      TEXT NOT NULL DEFAULT 'system',
    reason     TEXT NOT NULL DEFAULT '',
    payload    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- SSE 回放按 (tenant, mission, id) 递增读取。
CREATE INDEX IF NOT EXISTS idx_mission_events_stream
    ON mission_events (tenant_id, mission_id, id);

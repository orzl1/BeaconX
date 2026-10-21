-- 0014_b5_evidence：证据读取/校验/导出审计（B5）。
--
-- 设计要点：
--   * evidence_access_log 是只追加的审计台账：谁（actor）、在什么时间、对哪条
--     证据快照做了 read / verify / export；
--   * 证据快照被删除时其审计记录随之级联删除（没有快照的审计无法定位对象）；
--   * 索引按「快照维度看最近记录」与「按时间浏览」两条路径建立。
--
-- 为什么单独建表而不是复用 audit_events：
--   audit_events 记录的是任务内工具调用审计（mission 维度、payload 结构不同）；
--   证据访问是跨任务的全局对象，需要按 snapshot_id 反查且不能被任务删除级联清掉
--   （快照本身也独立于任务生命周期），因此单独一张轻量表更贴合语义。

CREATE TABLE IF NOT EXISTS evidence_access_log (
    tenant_id   BIGINT NOT NULL,
    id          BIGINT GENERATED ALWAYS AS IDENTITY,
    snapshot_id BIGINT NOT NULL,
    action      TEXT NOT NULL
                CHECK (action IN ('read', 'verify', 'export')),
    actor       TEXT NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, snapshot_id) REFERENCES evidence_snapshots (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 按快照查看最近访问（证据详情页 / API 列表）。
CREATE INDEX IF NOT EXISTS idx_evidence_access_snapshot
    ON evidence_access_log (tenant_id, snapshot_id, id DESC);

-- 按时间浏览/清理（后续审计面板与保留策略）。
CREATE INDEX IF NOT EXISTS idx_evidence_access_time
    ON evidence_access_log (tenant_id, created_at DESC);

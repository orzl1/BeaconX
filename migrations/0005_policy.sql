-- 0005_policy：策略规则、人工审批与审计。
--
-- 设计要点：
--   * policy_rules 由「代码内置默认规则 + 人工维护」组成；内置规则 immutable，
--     不允许在 API 层删除，避免安全底线被静默移除；
--   * approvals 是 ask 决策的待办项，带 expires_at；worker 轮询等待，超时按规则动作处理；
--   * audit_events 记录每次工具调用的策略结果与执行结果，作为可追溯证据。

CREATE TABLE IF NOT EXISTS policy_rules (
    tenant_id       BIGINT NOT NULL,
    id              BIGINT GENERATED ALWAYS AS IDENTITY,
    name            TEXT NOT NULL,
    tool            TEXT NOT NULL,
    arg_pattern     TEXT NOT NULL DEFAULT '',
    action          TEXT NOT NULL CHECK (action IN ('allow', 'ask', 'deny')),
    priority        integer NOT NULL DEFAULT 0,
    timeout_seconds integer NOT NULL DEFAULT 0,
    enabled         boolean NOT NULL DEFAULT true,
    immutable       boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 匹配路径：启用中的规则按优先级倒序。
CREATE INDEX IF NOT EXISTS idx_policy_rules_lookup
    ON policy_rules (tenant_id, enabled, priority DESC, id);

CREATE TABLE IF NOT EXISTS approvals (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id BIGINT NOT NULL,
    node_id    BIGINT NOT NULL,
    tool       TEXT NOT NULL,
    args       JSONB NOT NULL DEFAULT '{}'::jsonb,
    status     TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending', 'approved', 'rejected', 'timeout', 'cancelled')),
    note       TEXT NOT NULL DEFAULT '',
    decided_by TEXT NOT NULL DEFAULT '',
    decided_at timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, node_id) REFERENCES operation_nodes (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_approvals_pending
    ON approvals (tenant_id, created_at)
    WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS audit_events (
    tenant_id   BIGINT NOT NULL,
    id          BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id  BIGINT NOT NULL,
    node_id     BIGINT,
    tool        TEXT NOT NULL,
    action      TEXT NOT NULL,
    rule_name   TEXT NOT NULL DEFAULT '',
    args_digest TEXT NOT NULL DEFAULT '',
    outcome     TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL DEFAULT 'system',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_audit_events_mission
    ON audit_events (tenant_id, mission_id, id);

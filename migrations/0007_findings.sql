-- 0007_findings：发现、发现证据关联与复测。
--
-- 设计要点：
--   * 每张业务表首列为 tenant_id，主键/唯一键均以 tenant_id 打头，所有查询都带
--     租户条件；外键统一指向 tenants(tenant_id) 以支持级联清理；
--   * findings 是「发现」的长期生命周期记录：severity/status 用 CHECK 约束收敛，
--     非法取值在写入时即失败；资产/节点关联只做应用层校验（见下）；
--   * finding_evidence 把发现绑定到证据快照（evidence_snapshots），正文仍由
--     evidence 包加密落盘，这里只存引用；
--   * retests 记录复测请求与结果，状态机（pending → running → passed/failed/error）
--     由 internal/domain/finding 校验，数据库 CHECK 只做最终兜底。
--
-- 资产/节点为什么不做复合外键：
--   资产与作战图节点都允许被删除，而 ON DELETE SET NULL 会连带把 tenant_id 置空，
--   与「tenant_id NOT NULL」冲突；因此 findings.asset_id / node_id 保持裸 BIGINT，
--   由写入路径校验「同租户存在」（节点还要求属于同一任务），删除资产/节点不会
--   破坏发现记录。

CREATE TABLE IF NOT EXISTS findings (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id BIGINT NOT NULL,
    asset_id   BIGINT,
    node_id    BIGINT,
    title      TEXT NOT NULL,
    severity   TEXT NOT NULL DEFAULT 'info'
               CHECK (severity IN ('info', 'low', 'medium', 'high', 'critical')),
    status     TEXT NOT NULL DEFAULT 'open'
               CHECK (status IN ('open', 'confirmed', 'fixed', 'retest', 'closed', 'false_positive')),
    detail     TEXT NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_findings_mission
    ON findings (tenant_id, mission_id, id);

-- 资产维度查询（「该资产有哪些发现」）只对真正关联了资产的行建索引。
CREATE INDEX IF NOT EXISTS idx_findings_asset
    ON findings (tenant_id, asset_id)
    WHERE asset_id IS NOT NULL;

-- 发现 ↔ 证据快照：一条发现可绑定多条证据，同一快照重复绑定是幂等的（主键冲突）。
CREATE TABLE IF NOT EXISTS finding_evidence (
    tenant_id   BIGINT NOT NULL,
    finding_id  BIGINT NOT NULL,
    snapshot_id BIGINT NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, finding_id, snapshot_id),
    FOREIGN KEY (tenant_id, finding_id) REFERENCES findings (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, snapshot_id) REFERENCES evidence_snapshots (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 反查「某条证据被哪些发现引用」以及证据删除时的级联检查。
CREATE INDEX IF NOT EXISTS idx_finding_evidence_snapshot
    ON finding_evidence (tenant_id, snapshot_id);

-- 复测：一次请求一行，pending → running → passed/failed/error。
-- started_at/finished_at 由状态迁移写入，便于前端展示耗时。
CREATE TABLE IF NOT EXISTS retests (
    tenant_id    BIGINT NOT NULL,
    id           BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id   BIGINT NOT NULL,
    finding_id   BIGINT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'running', 'passed', 'failed', 'error')),
    note         TEXT NOT NULL DEFAULT '',
    requested_by TEXT NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, finding_id) REFERENCES findings (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_retests_mission
    ON retests (tenant_id, mission_id, id);

-- findings 的 updated_at 复用 0002 定义的 set_updated_at()。
DROP TRIGGER IF EXISTS trg_findings_updated_at ON findings;
CREATE TRIGGER trg_findings_updated_at
    BEFORE UPDATE ON findings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 0002_core_graph：资产图与作战图核心表。
--
-- 统一约定：
--   * 每张业务表首列为 tenant_id，主键/唯一键均以 tenant_id 打头；
--   * 所有 tenant_id 外键指向 tenants(id) ON DELETE CASCADE，便于测试与后续隔离；
--   * 时间列统一 timestamptz，默认 now()；更新路径由 set_updated_at() 触发器维护；
--   * 枚举值用 CHECK 约束表达，非法值在写入时即失败，而不是留到应用层。
--
-- 覆盖度语义（同时写入代码注释，避免两处漂移）：
--   分母 = 任务范围解析并登记到 mission_asset_links 的资产
--          ∪ 本任务中 role='target' 锚定的资产；
--   分子 = 本任务 kind='fact' 节点上 role='tested' 锚定的资产。
--   hint/touched 锚点不进入分母，因此「给提示挂资产」不会拉低覆盖度。

-- ---------- 公司（资产归属） ----------

CREATE TABLE IF NOT EXISTS companies (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL,
    attrs      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, slug),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 公司级范围：允许登记的资产范围条目，后续解析为任务范围的基础。
CREATE TABLE IF NOT EXISTS company_scope (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    company_id BIGINT NOT NULL,
    kind       TEXT NOT NULL
               CHECK (kind IN ('root_domain', 'subdomain', 'ip', 'cidr', 'url', 'keyword', 'icp')),
    value      TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'manual',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, company_id, kind, value),
    FOREIGN KEY (tenant_id, company_id) REFERENCES companies (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- ---------- 资产图 ----------

CREATE TABLE IF NOT EXISTS assets (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    type       TEXT NOT NULL
               CHECK (type IN ('root_domain', 'subdomain', 'ip', 'service', 'app', 'endpoint')),
    label      TEXT NOT NULL,
    -- 父资产由应用层写入；这里不做复合外键，因为 ON DELETE SET NULL 会连带
    -- 把 tenant_id 置空（违反 NOT NULL）。层级一致性由写入路径保证。
    parent_id  BIGINT,
    attrs      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 资产身份：一行资产可拥有多条身份键（如同时以域名与 IP 建立身份）。
CREATE TABLE IF NOT EXISTS asset_identity (
    tenant_id  BIGINT NOT NULL,
    asset_id   BIGINT NOT NULL,
    kind       TEXT NOT NULL
               CHECK (kind IN ('root_domain', 'subdomain', 'ip', 'service', 'app', 'endpoint')),
    key        TEXT NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, asset_id, kind, key),
    -- 同一租户内，一个身份键只能对应一个资产；upsert 的并发正确性依赖此约束。
    UNIQUE (tenant_id, kind, key),
    FOREIGN KEY (tenant_id, asset_id) REFERENCES assets (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- ---------- 任务与范围 ----------

CREATE TABLE IF NOT EXISTS missions (
    tenant_id   BIGINT NOT NULL,
    id          BIGINT GENERATED ALWAYS AS IDENTITY,
    name        TEXT NOT NULL,
    goal        TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'draft'
                CHECK (status IN ('draft', 'active', 'paused', 'done', 'failed', 'cancelled')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 任务范围：机器可读的授权边界；v1 用于资产解析与展示，后续编译为执行期策略。
CREATE TABLE IF NOT EXISTS mission_scope (
    tenant_id       BIGINT NOT NULL,
    id              BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id      BIGINT NOT NULL,
    kind            TEXT NOT NULL
                    CHECK (kind IN ('root_domain', 'subdomain', 'ip', 'cidr', 'url', 'keyword')),
    value           TEXT NOT NULL,
    allowed_actions TEXT[] NOT NULL DEFAULT '{}',
    note            TEXT NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, mission_id, kind, value),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 任务 ↔ 资产：覆盖度分母的主来源；source 记录资产进入任务的路径。
CREATE TABLE IF NOT EXISTS mission_asset_links (
    tenant_id  BIGINT NOT NULL,
    mission_id BIGINT NOT NULL,
    asset_id   BIGINT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'scope'
               CHECK (source IN ('scope', 'discovered', 'manual')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, mission_id, asset_id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, asset_id) REFERENCES assets (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- ---------- 作战图（每任务私有） ----------

CREATE TABLE IF NOT EXISTS operation_nodes (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id BIGINT NOT NULL,
    kind       TEXT NOT NULL
               CHECK (kind IN ('goal', 'hypothesis', 'action', 'fact', 'finding', 'hint', 'digest')),
    state      TEXT NOT NULL DEFAULT 'open'
               CHECK (state IN ('open', 'running', 'done', 'exhausted', 'blocked')),
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    payload    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    -- 供 operation_edges 的复合外键引用，保证边两端必须属于同一任务。
    UNIQUE (tenant_id, mission_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS operation_edges (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id BIGINT NOT NULL,
    src_id     BIGINT NOT NULL,
    dst_id     BIGINT NOT NULL,
    rel        TEXT NOT NULL
               CHECK (rel IN ('spawns', 'derived_from', 'yields', 'proves', 'covers')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, mission_id, src_id, dst_id, rel),
    CHECK (src_id <> dst_id),
    FOREIGN KEY (tenant_id, mission_id, src_id)
        REFERENCES operation_nodes (tenant_id, mission_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, mission_id, dst_id)
        REFERENCES operation_nodes (tenant_id, mission_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 锚点：把作战图节点锚定到资产；role 决定它是否参与覆盖度。
CREATE TABLE IF NOT EXISTS operation_anchors (
    tenant_id  BIGINT NOT NULL,
    node_id    BIGINT NOT NULL,
    asset_id   BIGINT NOT NULL,
    role       TEXT NOT NULL
               CHECK (role IN ('target', 'touched', 'tested', 'evidence')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, node_id, asset_id, role),
    FOREIGN KEY (tenant_id, node_id) REFERENCES operation_nodes (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, asset_id) REFERENCES assets (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- ---------- 索引 ----------

CREATE INDEX IF NOT EXISTS idx_assets_type ON assets (tenant_id, type);
CREATE INDEX IF NOT EXISTS idx_assets_parent ON assets (tenant_id, parent_id);
CREATE INDEX IF NOT EXISTS idx_company_scope_company ON company_scope (tenant_id, company_id);
CREATE INDEX IF NOT EXISTS idx_mission_scope_mission ON mission_scope (tenant_id, mission_id);
CREATE INDEX IF NOT EXISTS idx_mission_assets_asset ON mission_asset_links (tenant_id, asset_id);
CREATE INDEX IF NOT EXISTS idx_nodes_mission_kind_state
    ON operation_nodes (tenant_id, mission_id, kind, state);
CREATE INDEX IF NOT EXISTS idx_nodes_mission_created
    ON operation_nodes (tenant_id, mission_id, created_at);
CREATE INDEX IF NOT EXISTS idx_edges_mission ON operation_edges (tenant_id, mission_id);
CREATE INDEX IF NOT EXISTS idx_anchors_asset_role ON operation_anchors (tenant_id, asset_id, role);

-- ---------- updated_at 维护 ----------

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_tenants_updated_at ON tenants;
CREATE TRIGGER trg_tenants_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_companies_updated_at ON companies;
CREATE TRIGGER trg_companies_updated_at
    BEFORE UPDATE ON companies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_assets_updated_at ON assets;
CREATE TRIGGER trg_assets_updated_at
    BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_missions_updated_at ON missions;
CREATE TRIGGER trg_missions_updated_at
    BEFORE UPDATE ON missions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_operation_nodes_updated_at ON operation_nodes;
CREATE TRIGGER trg_operation_nodes_updated_at
    BEFORE UPDATE ON operation_nodes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

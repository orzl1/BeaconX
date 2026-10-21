-- 0015_sync：外部数据源同步（Sync，B5）。
--
-- 设计要点：
--   * 三张表，职责单一：
--       sync_sources  —— 数据源配置（类型 + JSONB config）；
--       sync_projects —— 同步产出的「外部项目」（按 source + external_id 幂等）；
--       sync_runs     —— 每次同步的只追加运行记录（状态 + 统计 + 错误）；
--   * 每张表首列 tenant_id，主键与唯一键均以 tenant_id 打头，外键统一指向
--     tenants(tenant_id) 以便删除租户时级联清理；
--   * sync_sources.name 在同租户内唯一（重名返回 409 的依据）；
--   * sync_projects 用 (tenant_id, source_id, external_id) 唯一键支撑 upsert，
--     重复同步只更新 name/data，不产生重复项目；
--   * sync_runs.project_id 不建外键：与 0002 的 assets.parent_id 同理，
--     复合外键的 ON DELETE SET NULL 会把 tenant_id 置空（违反 NOT NULL），
--     因此「项目必须属于同租户同数据源」由写入路径校验；
--   * 运行状态机：pending → running → succeeded / failed，终态不再迁移
--     （由 Store 用条件 UPDATE 保证，不依赖调用方自觉）。
--
-- config 形状（应用层校验，DB 只保证是 JSON 对象）：
--   manual    —— {}（无外部调用）；
--   http_json —— {"url":"https://…","headers":{"X-API-Key":"…"}}（仅 GET）。

CREATE TABLE IF NOT EXISTS sync_sources (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    name       TEXT NOT NULL,
    type       TEXT NOT NULL
               CHECK (type IN ('manual', 'http_json')),
    config     JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_sync_sources_tenant
    ON sync_sources (tenant_id, id);

DROP TRIGGER IF EXISTS trg_sync_sources_updated_at ON sync_sources;
CREATE TRIGGER trg_sync_sources_updated_at
    BEFORE UPDATE ON sync_sources
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS sync_projects (
    tenant_id   BIGINT NOT NULL,
    id          BIGINT GENERATED ALWAYS AS IDENTITY,
    source_id   BIGINT NOT NULL,
    external_id TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    data        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, source_id, external_id),
    FOREIGN KEY (tenant_id, source_id) REFERENCES sync_sources (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_sync_projects_source
    ON sync_projects (tenant_id, source_id, id);

DROP TRIGGER IF EXISTS trg_sync_projects_updated_at ON sync_projects;
CREATE TRIGGER trg_sync_projects_updated_at
    BEFORE UPDATE ON sync_projects
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS sync_runs (
    tenant_id   BIGINT NOT NULL,
    id          BIGINT GENERATED ALWAYS AS IDENTITY,
    source_id   BIGINT NOT NULL,
    project_id  BIGINT,
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    stats       JSONB NOT NULL DEFAULT '{}'::jsonb,
    error       TEXT NOT NULL DEFAULT '',
    started_at  timestamptz,
    finished_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, source_id) REFERENCES sync_sources (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 数据源维度的运行历史（前端按数据源倒序翻页）。
CREATE INDEX IF NOT EXISTS idx_sync_runs_source
    ON sync_runs (tenant_id, source_id, id DESC);

-- 按状态筛选（如「最近失败的同步」）。
CREATE INDEX IF NOT EXISTS idx_sync_runs_status
    ON sync_runs (tenant_id, status, id DESC);

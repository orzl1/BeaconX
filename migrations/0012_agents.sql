-- 0012_agents：Agent 定义（B5 P-B5-18..23）。
--
-- 设计要点：
--   * 每租户可定义多个 Agent；key 是稳定的对外标识（路径参数用它，不用 id）；
--   * key 的格式在数据库层再兜一层 CHECK，应用层会先校验以给出中文错误；
--   * tools 是字符串数组（JSONB），指向工具注册表里的工具名；与运行时是否
--     真正装配到 executor 无关（本批只做定义 CRUD，文档标 🟡）；
--   * role 限定为 planner/worker/retester/main，与执行角色对齐；
--   * 主键以 tenant_id 打头，所有查询都能带租户条件。

CREATE TABLE IF NOT EXISTS agents (
    tenant_id     BIGINT NOT NULL,
    id            BIGINT GENERATED ALWAYS AS IDENTITY,
    key           TEXT NOT NULL CHECK (key ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    name          TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'worker'
                  CHECK (role IN ('planner', 'worker', 'retester', 'main')),
    system_prompt TEXT NOT NULL DEFAULT '',
    tools         JSONB NOT NULL DEFAULT '[]'::jsonb
                  CHECK (jsonb_typeof(tools) = 'array'),
    model         TEXT NOT NULL DEFAULT '',
    enabled       BOOLEAN NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, key),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_agents_tenant_role
    ON agents (tenant_id, role, id);

DROP TRIGGER IF EXISTS trg_agents_updated_at ON agents;
CREATE TRIGGER trg_agents_updated_at
    BEFORE UPDATE ON agents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

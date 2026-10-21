-- 0010_mcp：MCP 端点与工具清单（B5-1）。
--
-- 设计要点：
--   * 每张业务表首列为 tenant_id，主键/唯一键均以 tenant_id 打头；外键统一
--     指向 tenants(id) 以支持租户级联清理；
--   * mcp_servers 保存端点配置：transport 收敛为 http/stdio 两种（SSE 传输在
--     当前最小实现中不单独建模，http 传输同时兼容 application/json 与
--     text/event-stream 响应）；
--   * args/env/headers 用 JSONB：args 是字符串数组，env 是 stdio 进程环境变量，
--     headers 是 http 传输附加请求头（如 Authorization）。env/headers 可能含
--     机密；HTTP 层负责在响应里掩码，表中按原值保存以便刷新时复用；
--   * enabled=false 的端点仍然参与列表与详情，但刷新接口允许显式调用（便于
--     启用前先验证连通性）；
--   * last_status/last_error 记录最近一次 refresh 的结果：unknown（从未刷新）/
--     ok / error。last_error 只保存脱敏后的摘要，不写入 env/headers 原值；
--   * mcp_tools 是「发现事实」的物化视图：一次成功的 tools/list 会整体替换该
--     端点的行（先删后插），因此 UNIQUE(tenant_id, server_id, name) 保证同名
--     工具只有一行；
--   * 删除端点级联删除其工具（端点不存在时工具没有意义）。

CREATE TABLE IF NOT EXISTS mcp_servers (
    tenant_id         BIGINT NOT NULL,
    id                BIGINT GENERATED ALWAYS AS IDENTITY,
    name              TEXT NOT NULL,
    transport         TEXT NOT NULL
                      CHECK (transport IN ('http', 'stdio')),
    url               TEXT NOT NULL DEFAULT '',
    command           TEXT NOT NULL DEFAULT '',
    args              JSONB NOT NULL DEFAULT '[]'::jsonb,
    env               JSONB NOT NULL DEFAULT '{}'::jsonb,
    headers           JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled           BOOLEAN NOT NULL DEFAULT true,
    last_status       TEXT NOT NULL DEFAULT 'unknown'
                      CHECK (last_status IN ('unknown', 'ok', 'error')),
    last_error        TEXT NOT NULL DEFAULT '',
    last_refreshed_at timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 列表页按名称排序、详情按主键查找；名称唯一键已覆盖 (tenant_id, name)。
CREATE INDEX IF NOT EXISTS idx_mcp_servers_tenant
    ON mcp_servers (tenant_id, id);

CREATE TABLE IF NOT EXISTS mcp_tools (
    tenant_id     BIGINT NOT NULL,
    id            BIGINT GENERATED ALWAYS AS IDENTITY,
    server_id     BIGINT NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    input_schema  JSONB NOT NULL DEFAULT '{}'::jsonb,
    discovered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, server_id, name),
    FOREIGN KEY (tenant_id, server_id)
        REFERENCES mcp_servers (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 工具列表按端点过滤是最常见路径（?server_id=）。
CREATE INDEX IF NOT EXISTS idx_mcp_tools_server
    ON mcp_tools (tenant_id, server_id, name);

-- updated_at 复用 0002 定义的 set_updated_at()。
DROP TRIGGER IF EXISTS trg_mcp_servers_updated_at ON mcp_servers;
CREATE TRIGGER trg_mcp_servers_updated_at
    BEFORE UPDATE ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

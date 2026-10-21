-- 0017_llm_exchanges：LLM 请求/响应正文采集（B7）。
--
-- 设计要点：
--   * 与 llm_usage 的分工：llm_usage 只记 token 计量（轻量、长期保留）；
--     本表记录「这一轮模型调用到底发了什么、回了什么」，用于排障与审计复盘，
--     数据量更大、保留期应当更短（后续加清理策略）；
--   * 正文是敏感数据：request 里的 API key / password 等键在写入前已脱敏
--     （evidence.RedactJSON），response 里的密钥样式文本也会被脱敏；
--     因此表里不存原始未脱敏内容；
--   * 单条正文上限 64KB（按 UTF-8 字节安全截断），truncated 标记是否发生过截断；
--   * request_bytes / response_bytes 是「截断后」的字节数，供列表展示体积；
--   * error 记录调用失败的错误文本（同样已脱敏、截断），失败时 response 为空；
--   * 迁移只追加、不改历史文件；mission_id 级联删除（任务删了，其调用记录没有
--     独立的审计价值，且这些记录里含有任务提示词）。

CREATE TABLE IF NOT EXISTS llm_exchanges (
    tenant_id         BIGINT NOT NULL,
    id                BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id        BIGINT NOT NULL,
    role              TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    request           JSONB NOT NULL DEFAULT '{}'::jsonb,
    response          TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT '',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    request_bytes     INTEGER NOT NULL DEFAULT 0,
    response_bytes    INTEGER NOT NULL DEFAULT 0,
    truncated         BOOLEAN NOT NULL DEFAULT false,
    redacted          BOOLEAN NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 任务内按 id 倒序取调用记录（详情页/排障）。
CREATE INDEX IF NOT EXISTS idx_llm_exchanges_mission
    ON llm_exchanges (tenant_id, mission_id, id DESC);

-- 全局按时间倒序（清理与「最近调用」视图）。
CREATE INDEX IF NOT EXISTS idx_llm_exchanges_created
    ON llm_exchanges (tenant_id, created_at DESC);

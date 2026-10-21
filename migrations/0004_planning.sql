-- 0004_planning：规划步骤、依赖、LLM 用量与任务预算。
--
-- 设计要点：
--   * plan_steps 是任务的长期计划（跨多次规划轮次保留），依赖关系单独存表，
--     引擎/规划器只派「依赖已全部完成」的步骤对应的动作；
--   * llm_usage 是每次模型调用的用量台账，missions.spent_tokens 是其累计值，
--     预算判定只读 missions，不扫描台账；
--   * max_tokens=0 表示不限制；max_steps=0 表示不限制。

CREATE TABLE IF NOT EXISTS plan_steps (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id BIGINT NOT NULL,
    key        TEXT NOT NULL CHECK (key ~ '^[a-z0-9_-]{1,64}$'),
    title      TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending', 'ready', 'running', 'done', 'skipped', 'failed')),
    priority   integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, mission_id, key),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_plan_steps_mission
    ON plan_steps (tenant_id, mission_id, status, priority DESC, id);

CREATE TABLE IF NOT EXISTS step_dependencies (
    tenant_id           BIGINT NOT NULL,
    step_id             BIGINT NOT NULL,
    depends_on_step_id  BIGINT NOT NULL,
    PRIMARY KEY (tenant_id, step_id, depends_on_step_id),
    CHECK (step_id <> depends_on_step_id),
    FOREIGN KEY (tenant_id, step_id) REFERENCES plan_steps (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, depends_on_step_id) REFERENCES plan_steps (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS llm_usage (
    tenant_id         BIGINT NOT NULL,
    id                BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id        BIGINT,
    role              TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    prompt_tokens     integer NOT NULL DEFAULT 0,
    completion_tokens integer NOT NULL DEFAULT 0,
    total_tokens      integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_llm_usage_mission
    ON llm_usage (tenant_id, mission_id, id);

ALTER TABLE missions
    ADD COLUMN IF NOT EXISTS max_tokens   BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS spent_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_steps    integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS steps_used   integer NOT NULL DEFAULT 0;

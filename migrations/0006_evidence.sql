-- 0006_evidence：流量采集（两阶段）与证据快照。
--
-- 设计要点：
--   * traffic_exchanges 一次调用两行语义：phase=intent（请求已脱敏）→ phase=result
--     （响应、状态码或错误）；中途崩溃会留下 intent 行，可被识别为未完成采集。
--   * 正文不落数据库：request/response 只存 BlobRef（sha256 + size），正文由
--     internal/app/evidence 加密落盘；证据快照的签名同样在文件侧生成与校验。
--   * evidence_snapshots 是冻结的证据版本：篡改数据库字段或正文都必须能被发现。

CREATE TABLE IF NOT EXISTS traffic_exchanges (
    tenant_id     BIGINT NOT NULL,
    id            BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id    BIGINT NOT NULL,
    node_id       BIGINT,
    tool          TEXT NOT NULL,
    phase         TEXT NOT NULL DEFAULT 'intent'
                  CHECK (phase IN ('intent', 'result')),
    request_hash  TEXT NOT NULL DEFAULT '',
    request_size  BIGINT NOT NULL DEFAULT 0,
    response_hash TEXT NOT NULL DEFAULT '',
    response_size BIGINT NOT NULL DEFAULT 0,
    status_code   integer,
    error         TEXT NOT NULL DEFAULT '',
    redaction     TEXT NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_traffic_exchanges_mission
    ON traffic_exchanges (tenant_id, mission_id, id);

CREATE TABLE IF NOT EXISTS evidence_snapshots (
    tenant_id     BIGINT NOT NULL,
    id            BIGINT GENERATED ALWAYS AS IDENTITY,
    mission_id    BIGINT NOT NULL,
    tool          TEXT NOT NULL,
    request_hash  TEXT NOT NULL,
    request_size  BIGINT NOT NULL DEFAULT 0,
    response_hash TEXT NOT NULL,
    response_size BIGINT NOT NULL DEFAULT 0,
    signature     TEXT NOT NULL,
    redaction     TEXT NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, mission_id) REFERENCES missions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_evidence_snapshots_mission
    ON evidence_snapshots (tenant_id, mission_id, id);

-- 0018_notification_push：事件推送去重/游标（B8）。
--
-- 设计要点：
--   * notification_events 是「某个 mission_event 已经投递给某个渠道」的幂等台账；
--     UNIQUE(tenant_id, event_id, channel_id) 由数据库保证同一事件+渠道只投一次，
--     后台 dispatcher 先 INSERT ... ON CONFLICT DO NOTHING，抢到行才发送；
--   * event_id 引用 mission_events.id（该表 id 全局自增、跨租户唯一），不建复合外键：
--     mission_events 的复合外键需要 (tenant_id, id)，而这里同时按 event 与 channel
--     关联，保留外键会引入级联删除顺序问题；事件/渠道删除后该行由租户级联清掉；
--   * delivery_id 指向 notification_deliveries.id（同租户），可为 NULL：先占位去重
--     再发送，发送完成后回填；发送进程崩溃会留下「已占位未回填」的记录，语义为
--     「已尝试过、不再重复发送」（at-most-once）；
--   * 只追加，不更新 event_id/channel_id；dispatcher 的启动游标取 MAX(event_id)。

CREATE TABLE IF NOT EXISTS notification_events (
    tenant_id   BIGINT NOT NULL,
    id          BIGINT GENERATED ALWAYS AS IDENTITY,
    event_id    BIGINT NOT NULL,
    channel_id  BIGINT NOT NULL,
    delivery_id BIGINT,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, event_id, channel_id),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES notification_channels (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_notification_events_event
    ON notification_events (tenant_id, event_id);

CREATE INDEX IF NOT EXISTS idx_notification_events_channel
    ON notification_events (tenant_id, channel_id, id DESC);

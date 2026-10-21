-- 0009_notifications：通知渠道与投递记录（B4）。
--
-- 设计要点：
--   * 每张业务表首列为 tenant_id，主键/唯一键均以 tenant_id 打头；外键统一
--     指向 tenants(tenant_id) 以支持级联清理；
--   * notification_channels 保存渠道配置（config 为 JSONB，形状随 type 不同）；
--     渠道名称在同租户内唯一，避免 UI 上出现无法区分的重名渠道；
--   * notification_deliveries 是「发送事实」的只追加台账：测试发送、将来的
--     事件推送都会在这里留下一条记录，status 收敛为 pending/sent/failed/skipped；
--   * 删除渠道会级联删除其投递记录（渠道不存在时投递记录没有意义）。
--
-- 类型取值：
--   webhook —— 真实 HTTP POST（config.url 必填）；
--   email   —— SMTP 配置（config 为连接参数；当前版本不实际发信，测试记为 skipped）；
--   generic —— 通用占位（不联网，测试记为 skipped）。

CREATE TABLE IF NOT EXISTS notification_channels (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    name       TEXT NOT NULL,
    type       TEXT NOT NULL
               CHECK (type IN ('webhook', 'email', 'generic')),
    config     JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_notification_channels_tenant
    ON notification_channels (tenant_id, id);

CREATE TABLE IF NOT EXISTS notification_deliveries (
    tenant_id  BIGINT NOT NULL,
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    channel_id BIGINT NOT NULL,
    event      TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL
               CHECK (status IN ('pending', 'sent', 'failed', 'skipped')),
    detail     TEXT NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES notification_channels (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 列表与筛选路径：按渠道倒序、按状态倒序。
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_channel
    ON notification_deliveries (tenant_id, channel_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_status
    ON notification_deliveries (tenant_id, status, id DESC);

-- updated_at 复用 0002 定义的 set_updated_at()。
DROP TRIGGER IF EXISTS trg_notification_channels_updated_at ON notification_channels;
CREATE TRIGGER trg_notification_channels_updated_at
    BEFORE UPDATE ON notification_channels
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 0008_settings：租户级运行时设置（键值对）。
--
-- 设计要点：
--   * 业务设置属于租户级配置，与启动配置（config.json / BEACONX_* 环境变量）分离；
--     本表只做持久化与读取，运行时是否热生效由读取方决定（部分键当前仅在重启后生效，
--     详见 API 文档的「实现状态」标注）；
--   * value 用 JSONB 保存：数字、字符串、对象统一存储，读取方按 key 自行解码；
--   * 主键 (tenant_id, key) 保证同一租户同一键只有一行，写入走 UPSERT；
--   * updated_at 复用 0002 定义的 set_updated_at() 触发器函数。

CREATE TABLE IF NOT EXISTS settings (
    tenant_id  BIGINT NOT NULL,
    key        TEXT NOT NULL,
    value      JSONB NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

DROP TRIGGER IF EXISTS trg_settings_updated_at ON settings;
CREATE TRIGGER trg_settings_updated_at
    BEFORE UPDATE ON settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

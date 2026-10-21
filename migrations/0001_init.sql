-- 0001_init：租户根表。
--
-- 后续迁移新增的每张业务表都带有：
--   tenant_id BIGINT NOT NULL DEFAULT 1
-- 且每个主键/唯一键都以 tenant_id 打头。v1 为单租户，但结构现在就固定下来，
-- 以便开启隔离时无需重写数据。
--
-- schema_migrations 由迁移执行器创建，不在本文件中。

CREATE TABLE IF NOT EXISTS tenants (
    id          BIGINT PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'suspended')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

INSERT INTO tenants (id, slug, name)
VALUES (1, 'default', 'Default tenant')
ON CONFLICT (id) DO NOTHING;

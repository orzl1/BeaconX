-- 0001_init: tenant root.
--
-- Every business table added by later migrations carries:
--   tenant_id BIGINT NOT NULL DEFAULT 1
-- and every primary/unique key starts with tenant_id. v1 is single-tenant, but
-- the shape is fixed now so isolation can be enabled without a data rewrite.
--
-- schema_migrations itself is created by the migration runner, not here.

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

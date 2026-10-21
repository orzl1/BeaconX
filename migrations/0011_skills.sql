-- 0011_skills：技能元数据（B5-2）。
--
-- 设计要点：
--   * 技能正文（SKILL.md、脚本、参考文件）存放在租户状态目录
--     <state>/skills/<tenant_id>/<name>/ 下，数据库只保存「元数据 + 文件数」；
--     因此本表不做文件级物化（不建 skill_files），避免磁盘与数据库两套真源；
--   * file_count 是「上一次写入/上传/创建时已知的文件数」，用于列表页展示
--     与缺失统计：HTTP 层把它与磁盘实际文件数比较，磁盘更少即视为缺失；
--   * name 是技能的唯一标识，同时是目录名，因此 HTTP 层强制
--     ^[a-z0-9][a-z0-9_-]{0,63}$，避免大小写/路径字符带来的跨平台问题；
--   * 主键 (tenant_id, name) 直接给出「同租户内重名即冲突」的语义；不同租户
--     可以拥有同名技能（磁盘路径也按租户隔离）；
--   * version 只是展示用字符串（默认 0.1.0），当前不做版本历史；
--   * updated_at 复用 0002 定义的 set_updated_at()。

CREATE TABLE IF NOT EXISTS skills (
    tenant_id   BIGINT NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    version     TEXT NOT NULL DEFAULT '0.1.0',
    file_count  INTEGER NOT NULL DEFAULT 0 CHECK (file_count >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, name),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

-- 列表页按名称升序；主键已覆盖 (tenant_id, name)。
CREATE INDEX IF NOT EXISTS idx_skills_tenant
    ON skills (tenant_id, updated_at DESC);

DROP TRIGGER IF EXISTS trg_skills_updated_at ON skills;
CREATE TRIGGER trg_skills_updated_at
    BEFORE UPDATE ON skills
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

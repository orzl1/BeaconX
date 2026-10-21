-- 0013_workspace：工作区文件管理的版本占位。
--
-- workspace 的内容以文件系统为准（<state.dir>/tenants/<tenantID>/workspace/
-- missions/<missionID>/），数据库只保存任务与租户关系，不需要新增表：
--   * 文件列表、读写、上传、删除全部由 internal/app/workspace 完成；
--   * 任务存在性与租户隔离由 missions 表校验；
--   * 迁移版本保留是为了让「工作区能力」在 schema_migrations 中可见，并为后续
--     可能出现的配额/使用量统计表预留同一条版本线，避免未来插入中间版本时
--     与已发布迁移的序号冲突。
--
-- 本迁移为显式空操作（仅用于记录版本）。

SELECT 1;

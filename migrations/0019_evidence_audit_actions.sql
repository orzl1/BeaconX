-- 0019_evidence_audit_actions：扩展证据访问审计的动作集合（B8）。
--
-- 0014 的 CHECK 只允许 read/verify/export；B8 补两类埋点：
--   * create —— 冻结证据快照（POST /traffic/{id}/evidence）；
--   * rotate —— 密钥轮换时对每条受影响快照记一条（说明该快照签名被重算）。
--
-- 迁移只追加：不修改 0014 已发布文件，而是 drop 旧约束后以同名重建，保持
-- schema_migrations 校验和不变。约束名是 PostgreSQL 对列内联 CHECK 的默认命名。

ALTER TABLE evidence_access_log
    DROP CONSTRAINT IF EXISTS evidence_access_log_action_check;

ALTER TABLE evidence_access_log
    ADD CONSTRAINT evidence_access_log_action_check
    CHECK (action IN ('read', 'verify', 'export', 'create', 'rotate'));

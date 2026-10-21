-- 0016_findings_node_unique：同一作战图节点最多关联一条发现。
--
-- B5 的 POST /api/v1/missions/{id}/findings/from-node 支持「从事实/发现节点创建
-- 发现」。应用层已有 FindingIDForNode 预检查（顺序请求返回 409），但并发请求
-- 仍可能各自通过预检查后插入两条；这里用部分唯一索引做数据库兜底。
--
-- 只对 node_id IS NOT NULL 建索引：人工创建、未绑定节点的发现不受约束。
CREATE UNIQUE INDEX IF NOT EXISTS uq_findings_node
    ON findings (tenant_id, node_id)
    WHERE node_id IS NOT NULL;

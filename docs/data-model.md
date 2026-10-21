# 数据模型（M1–M6.2-B5）

本文描述 BeaconX M1–M6.2-B5 的数据层：资产图与作战图（0002）、任务生命周期（0003）、计划与模型用量（0004）、工具策略/审批/审计（0005）、流量采集与证据快照（0006）、发现/证据关联/复测（0007）、B4/B5 的设置/通知/MCP/Skills/Agents/证据审计/Sync（0008–0015）。

权威来源是 `migrations/0001_init.sql` 至 `migrations/0015_sync.sql` 与 `internal/infra/pg` 的写入路径；本文只做解释，若与迁移或代码不一致，以迁移与代码为准，并回来修正本文。

## 1. 多租户约定

- 每张业务表的**首列**都是 `tenant_id BIGINT NOT NULL`，并且主键/唯一键都以 `tenant_id` 打头（例如 `PRIMARY KEY (tenant_id, id)`、`UNIQUE (tenant_id, kind, key)`）。
- 需要跨表引用时使用以 `tenant_id` 开头的复合外键（如 `(tenant_id, mission_id)` → `missions (tenant_id, id)`），保证引用不会跨租户；所有 `tenant_id` 外键统一指向 `tenants (id) ON DELETE CASCADE`。
- v1 只有一个真实租户（`id = 1`），但存储层每个方法都显式接收 `tenantID`，所有 SQL 都带 `tenant_id = $1` 条件。`Store` 对 `tenantID <= 0` 返回 `ErrInvalidTenant`，不存在“省略租户”的查询入口。
- 集成测试采用「每个测试一个租户」：租户 ID 取时间戳，测试结束 `DELETE FROM tenants WHERE id = $1` 依靠级联清理全部数据；双租户隔离测试用同一身份键分别在两个租户 upsert，断言得到不同资产 ID，且跨租户读取失败。

## 2. 表清单

| 迁移 | 表 | 用途 |
| --- | --- | --- |
| 0001 | `tenants` | 租户根表，所有数据的归属单位 |
| 0002 | `companies` | 资产归属的公司 |
| 0002 | `company_scope` | 公司级允许登记的资产范围条目 |
| 0002 | `assets` | 资产图：跨任务共享的资产真值 |
| 0002 | `asset_identity` | 资产身份键，一行资产可有多条 |
| 0002 | `missions` | 授权测试任务 |
| 0002 | `mission_scope` | 任务范围（机器可读的授权边界） |
| 0002 | `mission_asset_links` | 任务 ↔ 资产登记，覆盖度分母的主来源 |
| 0002 | `operation_nodes` | 作战图节点 |
| 0002 | `operation_edges` | 作战图边，两端必须同任务 |
| 0002 | `operation_anchors` | 节点 ↔ 资产锚点，`role` 决定是否计入覆盖度 |
| 0003 | `mission_events` | 只追加的任务事件流，SSE 回放与状态追溯的事实来源 |
| 0004 | `plan_steps` | 任务的长期计划步骤（跨规划轮次保留） |
| 0004 | `step_dependencies` | 步骤之间的依赖关系，决定可派发顺序 |
| 0004 | `llm_usage` | 每次模型调用的 token 用量台账 |
| 0005 | `policy_rules` | 工具调用策略规则（内置 + 人工维护） |
| 0005 | `approvals` | ask 决策产生的待人工审批项 |
| 0005 | `audit_events` | 每次工具调用的策略与执行审计 |
| 0006 | `traffic_exchanges` | 工具调用的两阶段流量采集（intent → result） |
| 0006 | `evidence_snapshots` | 冻结的证据快照（签名 + 正文引用） |
| 0007 | `findings` | 任务发现（严重级别、处置状态、可选资产/节点关联） |
| 0007 | `finding_evidence` | 发现 ↔ 证据快照的绑定（多对多） |
| 0007 | `retests` | 复测请求与结果（pending → running → 终态） |
| 0008 | `settings` | 租户级运行时设置（点号键 + JSONB） |
| 0009 | `notification_channels` | 通知渠道（webhook/email/generic） |
| 0009 | `notification_deliveries` | 通知投递事实台账 |
| 0010 | `mcp_servers` | MCP 端点（http/stdio） |
| 0010 | `mcp_tools` | 刷新发现到的 MCP 工具 |
| 0011 | `skills` | 技能元数据（正文在磁盘） |
| 0012 | `agents` | Agent 定义（role/tools/prompt） |
| 0014 | `evidence_access_log` | 证据读取/校验/导出审计 |
| 0015 | `sync_sources` | 同步数据源（manual/http_json） |
| 0015 | `sync_projects` | 外部项目（幂等 upsert） |
| 0015 | `sync_runs` | 同步运行记录（pending→running→终态） |

### 2.1 租户根表（0001）

`tenants`：`id` 主键、`slug` 唯一、`name`、`status`（`active` / `suspended`）、`created_at`、`updated_at`；0001 迁移会写入默认租户（`id = 1`、`slug = 'default'`），其余业务表都通过 `tenant_id` 指向它。`tenants` 的 `updated_at` 同样由 `set_updated_at()` 触发器维护（0002 迁移挂载）。

### 2.2 公司与公司范围

- `companies`：`name`、`slug`、`attrs JSONB`；`PRIMARY KEY (tenant_id, id)`，`UNIQUE (tenant_id, slug)`。
- `company_scope`：`company_id`、`kind`（`root_domain` / `subdomain` / `ip` / `cidr` / `url` / `keyword` / `icp`）、`value`、`source`；`UNIQUE (tenant_id, company_id, kind, value)`；复合外键 `(tenant_id, company_id)` → `companies (tenant_id, id)`；索引 `idx_company_scope_company (tenant_id, company_id)`。

### 2.3 资产图

- `assets`：`type`（`root_domain` / `subdomain` / `ip` / `service` / `app` / `endpoint`）、`label`、`parent_id`、`attrs JSONB`；`PRIMARY KEY (tenant_id, id)`；索引 `idx_assets_type (tenant_id, type)`、`idx_assets_parent (tenant_id, parent_id)`。
- `parent_id` 刻意不建外键：`ON DELETE SET NULL` 会连带把复合键里的 `tenant_id` 置空，违反 `NOT NULL`。为保证层级引用有效，`Store.UpsertAsset` 在写入事务内校验父资产存在且属于同一租户，并拒绝把资产自身设为父资产。
- `asset_identity`：`asset_id`、`kind`、`key`；`PRIMARY KEY (tenant_id, asset_id, kind, key)`，`UNIQUE (tenant_id, kind, key)`；复合外键 `(tenant_id, asset_id)` → `assets (tenant_id, id) ON DELETE CASCADE`。唯一约束是资产 upsert 并发正确性的基础：同一租户内一个身份键只能指向一个资产。

### 2.4 任务与范围

- `missions`：`name`、`goal`、`description`、`status`（M2 起为 `queued` / `running` / `paused` / `finalizing` / `done` / `failed` / `cancelled`，默认 `queued`）、`queued`、`queued_at`、`started_at`、`finished_at`、`deadline_at`、`reason`；时间与状态语义见 2.7。
- `mission_scope`：`kind`（`root_domain` / `subdomain` / `ip` / `cidr` / `url` / `keyword`，注意没有 `icp`）、`value`、`allowed_actions TEXT[]`、`note`；`UNIQUE (tenant_id, mission_id, kind, value)`；复合外键 `(tenant_id, mission_id)` → `missions (tenant_id, id)`；索引 `idx_mission_scope_mission (tenant_id, mission_id)`。
- `mission_asset_links`：`mission_id`、`asset_id`、`source`（`scope` / `discovered` / `manual`）；`PRIMARY KEY (tenant_id, mission_id, asset_id)`；同时外键到 `missions` 与 `assets`；索引 `idx_mission_assets_asset (tenant_id, asset_id)`。

### 2.5 作战图（每任务私有）

- `operation_nodes`：`kind`、`state`、`title`、`body`、`payload JSONB`；M2 增加 `priority`、`lease_until`、`leased_by`、`attempts`（见 2.7）；`PRIMARY KEY (tenant_id, id)`，`UNIQUE (tenant_id, mission_id, id)`（专供边的复合外键引用）；索引 `idx_nodes_mission_kind_state (tenant_id, mission_id, kind, state)`、`idx_nodes_mission_created (tenant_id, mission_id, created_at)`。
- `operation_edges`：`src_id`、`dst_id`、`rel`；`UNIQUE (tenant_id, mission_id, src_id, dst_id, rel)`；`CHECK (src_id <> dst_id)`；两个复合外键 `(tenant_id, mission_id, src_id/dst_id)` → `operation_nodes (tenant_id, mission_id, id) ON DELETE CASCADE`，由数据库保证边两端属于同一任务；索引 `idx_edges_mission (tenant_id, mission_id)`。
- `operation_anchors`：`node_id`、`asset_id`、`role`；`PRIMARY KEY (tenant_id, node_id, asset_id, role)`；复合外键到 `operation_nodes (tenant_id, id)` 与 `assets (tenant_id, id)`；索引 `idx_anchors_asset_role (tenant_id, asset_id, role)`。

### 2.6 索引与触发器

所有索引都以 `tenant_id` 打头，覆盖类型过滤、父子查询、任务范围、节点/边/锚点与覆盖度查询路径。`updated_at` 由 `set_updated_at()` 触发器维护，当前挂在 `tenants`、`companies`、`assets`、`missions`、`operation_nodes`、`findings`、`settings`、`notification_channels`、`mcp_servers`、`skills`、`agents`、`sync_sources`、`sync_projects` 共 13 张表上；枚举值一律由 `CHECK` 约束在写入时校验。0007 新增 4 个索引，0008–0015 新增 13 个索引（合计 37 个）。

### 2.7 运行时变更（0003）

- **状态词表收敛**：`missions.status` 的 `CHECK` 改为 `queued` / `running` / `paused` / `finalizing` / `done` / `failed` / `cancelled`，默认值改为 `queued`；迁移时把存量 `draft` 改为 `queued`、`active` 改为 `running`，M2 起不再产生 `draft` / `active`。
- **任务时间与原因**：`queued boolean NOT NULL DEFAULT false`、`queued_at`、`started_at`、`finished_at`、`deadline_at`、`reason text NOT NULL DEFAULT ''`。任务置为排队时写 `queued=true` 与 `queued_at=now()`；首次进入 `running` 时写 `started_at`；进入终态时写 `finished_at`；`reason` 保存最近一次迁移原因（例如「用户暂停」「进程重启恢复」「任务完成」）。
- **任务索引**：`idx_missions_queue (tenant_id, queued_at) WHERE queued`（FIFO 准入的查询路径）、`idx_missions_status (tenant_id, status, updated_at DESC)`。
- **动作租约列**：`operation_nodes` 增加 `priority integer NOT NULL DEFAULT 0`、`lease_until timestamptz`、`leased_by text`、`attempts integer NOT NULL DEFAULT 0`；部分索引 `idx_nodes_claim (tenant_id, mission_id, priority DESC, id) WHERE state IN ('open', 'running')` 支撑按优先级领取动作。
- **事件表 `mission_events`**：`tenant_id`、`id`（自增）、`mission_id`、`type`、`actor`（默认 `system`）、`reason`（默认空串）、`payload JSONB`（默认 `{}`）、`created_at`；`PRIMARY KEY (tenant_id, id)`，外键 `(tenant_id, mission_id)` → `missions (tenant_id, id) ON DELETE CASCADE`、`tenant_id` → `tenants (id) ON DELETE CASCADE`；索引 `idx_mission_events_stream (tenant_id, mission_id, id)` 支撑 SSE 按 `id` 递增回放。
- 状态与事件的语义、SSE 游标与回放规则见 [运行时与引擎](runtime.md)。

### 2.8 规划与用量（0004）

- `plan_steps`：`tenant_id`、`id`（自增）、`mission_id`、`key`（`CHECK (key ~ '^[a-z0-9_-]{1,64}$')`）、`title`、`detail`、`status`（`pending` / `ready` / `running` / `done` / `skipped` / `failed`，默认 `pending`）、`priority`、`created_at`、`updated_at`；`PRIMARY KEY (tenant_id, id)`、`UNIQUE (tenant_id, mission_id, key)`；外键 `(tenant_id, mission_id)` → `missions` 级联删除；索引 `idx_plan_steps_mission (tenant_id, mission_id, status, priority DESC, id)`。
- `step_dependencies`：`step_id`、`depends_on_step_id`；`PRIMARY KEY (tenant_id, step_id, depends_on_step_id)`，`CHECK (step_id <> depends_on_step_id)`；两个外键均级联到 `plan_steps (tenant_id, id)`。
- `llm_usage`：`mission_id`（可空）、`role`（如 planner / planner-retry / planner-followup / worker / worker-tool）、`model`、`prompt_tokens`、`completion_tokens`、`total_tokens`、`created_at`；`PRIMARY KEY (tenant_id, id)`，外键到 `missions` 与 `tenants`；索引 `idx_llm_usage_mission (tenant_id, mission_id, id)`。
- `missions` 增加 `max_tokens BIGINT NOT NULL DEFAULT 0`、`spent_tokens BIGINT NOT NULL DEFAULT 0`、`max_steps integer NOT NULL DEFAULT 0`、`steps_used integer NOT NULL DEFAULT 0`；0 表示不限制。

### 2.9 工具与策略（0005）

- `policy_rules`：`name`、`tool`、`arg_pattern`（默认空串）、`action`（CHECK `allow` / `ask` / `deny`）、`priority`（默认 0）、`timeout_seconds`（默认 0）、`enabled`（默认 true）、`immutable`（默认 false）、`created_at`、`updated_at`；`PRIMARY KEY (tenant_id, id)`、`UNIQUE (tenant_id, name)`；索引 `idx_policy_rules_lookup (tenant_id, enabled, priority DESC, id)`。启动时用 `ON CONFLICT (tenant_id, name) DO NOTHING` 幂等播种内置规则，不覆盖用户改动。
- `approvals`：`mission_id`、`node_id`、`tool`、`args JSONB`（原始参数）、`status`（`pending` / `approved` / `rejected` / `timeout` / `cancelled`，默认 `pending`）、`note`、`decided_by`、`decided_at`、`expires_at`（NOT NULL）、`created_at`；`PRIMARY KEY (tenant_id, id)`；外键 `(tenant_id, mission_id)` → `missions`、`(tenant_id, node_id)` → `operation_nodes`、`tenant_id` → `tenants`，均级联删除；部分索引 `idx_approvals_pending (tenant_id, created_at) WHERE status = 'pending'`。
- `audit_events`：`mission_id`、`node_id`（可空）、`tool`、`action`、`rule_name`（默认空串）、`args_digest`（参数原始 JSON 的 SHA-256）、`outcome`、`detail`、`actor`（默认 `system`）、`created_at`；`PRIMARY KEY (tenant_id, id)`，外键到 `missions` 与 `tenants`；索引 `idx_audit_events_mission (tenant_id, mission_id, id)`。审计不保存原始参数（参数在审批表），只保存摘要。

### 2.10 证据链（0006）

- `traffic_exchanges`：`mission_id`、`node_id`（可空）、`tool`、`phase`（CHECK `intent` / `result`，默认 `intent`）、`request_hash`、`request_size`、`response_hash`、`response_size`、`status_code`（可空）、`error`（默认空串）、`redaction`（默认空串）、`created_at`、`completed_at`（可空）；`PRIMARY KEY (tenant_id, id)`；外键 `(tenant_id, mission_id)` → `missions` 与 `tenant_id` → `tenants`，均级联删除；索引 `idx_traffic_exchanges_mission (tenant_id, mission_id, id)`。一次调用对应一行，先写 intent，再更新为 result；崩溃会留下 `phase='intent'` 的未完成行。
- `evidence_snapshots`：`mission_id`、`tool`、`request_hash`、`request_size`、`response_hash`、`response_size`、`signature`、`redaction`、`created_at`；`PRIMARY KEY (tenant_id, id)`，外键同 `traffic_exchanges`；索引 `idx_evidence_snapshots_mission (tenant_id, mission_id, id)`。
- 正文（请求参数与工具输出）不落数据库：`*_hash` / `*_size` 指向 `internal/app/evidence` 加密存储的 blob（`sha256` 内容寻址、AES-256-GCM、路径 `blobs/<hash 前两位>/<hash>.enc`）；`signature` 是对包含快照 ID 的规范化元数据做 HMAC-SHA256。数据库行 ID 是接口使用的 `evidence_id`，快照 ID 可由元数据重算。
- 表结构与校验细节见 [证据链](evidence.md)。

### 2.11 发现、证据关联与复测（0007）

- `findings`：`mission_id`、`asset_id`（可空）、`node_id`（可空）、`title`、`severity`（CHECK `info` / `low` / `medium` / `high` / `critical`，默认 `info`）、`status`（CHECK `open` / `confirmed` / `fixed` / `retest` / `closed` / `false_positive`，默认 `open`）、`detail`、`created_at`、`updated_at`；`PRIMARY KEY (tenant_id, id)`；外键 `(tenant_id, mission_id)` → `missions` 与 `tenant_id` → `tenants`，均级联删除；索引 `idx_findings_mission (tenant_id, mission_id, id)`、部分索引 `idx_findings_asset (tenant_id, asset_id) WHERE asset_id IS NOT NULL`；`updated_at` 由 `trg_findings_updated_at`（复用 `set_updated_at()`）维护。
- `finding_evidence`：`finding_id`、`snapshot_id`（均非空）、`created_at`；`PRIMARY KEY (tenant_id, finding_id, snapshot_id)`；复合外键 `(tenant_id, finding_id)` → `findings`、`(tenant_id, snapshot_id)` → `evidence_snapshots`、`tenant_id` → `tenants`，均级联删除；索引 `idx_finding_evidence_snapshot (tenant_id, snapshot_id)` 支持「某证据被哪些发现引用」反查。正文仍由 `internal/app/evidence` 加密落盘，这里只存引用；重复绑定由 Store 的 `ON CONFLICT DO NOTHING` + 影响行数检测返回 HTTP 409（迁移注释称“幂等”，实际 API 语义为冲突）。
- `retests`：`mission_id`、`finding_id`、`status`（CHECK `pending` / `running` / `passed` / `failed` / `error`，默认 `pending`）、`note`、`requested_by`（默认空串，API 缺省写 `user`）、`created_at`、`started_at`（可空）、`finished_at`（可空）；`PRIMARY KEY (tenant_id, id)`；外键 `(tenant_id, mission_id)` → `missions`、`(tenant_id, finding_id)` → `findings`、`tenant_id` → `tenants`，均级联删除；索引 `idx_retests_mission (tenant_id, mission_id, id)`。
- **资产/节点为什么不做复合外键**：`assets` 与 `operation_nodes` 允许被删除，而 `ON DELETE SET NULL` 会把复合键中的 `tenant_id` 置空（违反 `NOT NULL`）；因此 `findings.asset_id` / `node_id` 保持裸 `BIGINT`，由写入路径校验「同租户存在」，节点还要求属于同一任务。删除资产/节点不会破坏发现记录，也不会自动清空关联；查询时按存在性解释。
- **状态机**：发现 `open → confirmed/false_positive/closed`、`confirmed → fixed/retest/closed`、`retest → confirmed/fixed/closed`、`fixed → retest/closed`，`closed` 与 `false_positive` 终态；复测 `pending → running`、`running → passed/failed/error`，终态 `passed/failed/error`。领域校验在 `internal/domain/finding`，数据库 CHECK 只做最终兜底。
- 与证据链的关系：`finding_evidence.snapshot_id` 指向 `evidence_snapshots`（同一任务）；快照删除会级联解除绑定，但不会删除发现。报告与前端详情通过该表展示「发现 → 证据」。

### 2.12 计划表（⬜ 未创建，对应 B4/B5）

以下表**尚未创建**（无迁移、无 Store），仅用于前端/后端设计对齐；落地时新增 `migrations/0008_*.sql` 并补充本文件与 `docs/project-map.md` §4/§10：

| 计划表 | 用途 | 关联计划接口 |
| --- | --- | --- |
| `notification_channels` / `notification_deliveries` | 通知渠道与投递记录 | P-B4-11..17 |
| `settings` | 运行时设置（与 config 优先级待定） | P-B4-18/19 |
| `mcp_servers` / `mcp_tools` | MCP 端点与发现到的工具 | P-B5-01..07 |
| `skills` / `skill_files` | 技能包元数据与文件 | P-B5-08..17 |
| `agents` | Agent 定义（key/model/tools/prompt） | P-B5-18..23 |
| `sync_sources` / `sync_projects` / `sync_runs` | 外部数据源、项目映射与运行记录 | P-B5-29..37 |
| `evidence_read_audits` | 证据正文读取审计 | P-B5-44 |
| 游标分页 | 不新增表，为 B1–B3 列表增加索引/查询条件 | P-B4-26 |
| Workspace | 不新增表，使用状态目录沙箱文件 | P-B5-24..28 |

> 计划表的字段/索引/约束在实现时确定；当前 API 与数据库均**没有**这些表。

### 2.13 B4/B5 新增表（0008–0015）

| 迁移 | 表 | 关键列 / 约束 / 索引 | 说明 |
| --- | --- | --- | --- |
| 0008 | `settings` | PK(tenant,key)；`value JSONB`；`updated_at` 触发器；FK tenants CASCADE | 运行时设置；只持久化+可读，运行时未热生效（🟡） |
| 0009 | `notification_channels` | PK(tenant,id)；UNIQUE(tenant,name)；`type` CHECK webhook/email/generic；`config JSONB`；索引 + 触发器 | 通知渠道；webhook 真发、email/generic 测试 skipped |
| 0009 | `notification_deliveries` | PK(tenant,id)；FK channel CASCADE；`status` CHECK pending/sent/failed/skipped；2 索引 | 投递事实（只追加） |
| 0010 | `mcp_servers` | PK(tenant,id)；UNIQUE(tenant,name)；`transport` CHECK http/stdio；`args/env/headers JSONB`；`last_status` CHECK unknown/ok/error；索引 + 触发器 | env/headers 响应掩码、stdio 默认禁用 |
| 0010 | `mcp_tools` | PK(tenant,id)；UNIQUE(tenant,server,name)；FK server CASCADE；`input_schema JSONB`；索引 server | 刷新时整体替换 |
| 0011 | `skills` | PK(tenant,name)；`file_count>=0`；`version` 展示用；索引 + 触发器 | 正文在 `<state>/skills/<tenant>/<name>/`；无 skill_files 表 |
| 0012 | `agents` | PK(tenant,id)；UNIQUE(tenant,key)；`key` 正则 CHECK；`role` CHECK planner/worker/retester/main；`tools JSONB array`；索引 + 触发器 | 定义 CRUD；**未接入 engine（🟡）** |
| 0013 | 无表 | 空操作 `SELECT 1` | 工作区以文件系统为准；仅记录迁移版本 |
| 0014 | `evidence_access_log` | PK(tenant,id)；FK snapshot CASCADE；`action` CHECK read/verify/export；索引 snapshot/time | 证据审计；当前仅部分动作埋点（🟡） |
| 0015 | `sync_sources` | PK(tenant,id)；UNIQUE(tenant,name)；`type` CHECK manual/http_json；`config JSONB`；索引 + 触发器 | SSRF 三层防护、默认拒私网 |
| 0015 | `sync_projects` | PK(tenant,id)；UNIQUE(tenant,source,external_id)；`data JSONB`；FK source CASCADE；索引 + 触发器 | 幂等 upsert |
| 0015 | `sync_runs` | PK(tenant,id)；`status` CHECK pending/running/succeeded/failed；`stats JSONB`；source/status 索引；`project_id` 无 FK（应用层校验） | 运行记录只追加 |

- **资产/节点为什么不做复合 FK**（沿用 findings）：`ON DELETE SET NULL` 会把复合键的 `tenant_id` 置空，违反 `NOT NULL`；`findings.asset_id/node_id`、`sync_runs.project_id` 等由应用层校验同租户/同任务。
- **文件沙箱**：Skills/Workspace 不建文件表，正文在状态目录；路径 Clean+前缀校验、拒绝符号链接/穿越、大小限制（Skills zip≤10MB/≤200 文件；Workspace 单文件≤1MB、上传≤10MB）。
- **SSRF**：Sync 保存/解析/拨号三层校验，默认拒绝 loopback/私网；仅 `BEACONX_SYNC_ALLOW_PRIVATE` 放开。
- 0008–0015 共新增 11 张表（0013 空操作）；索引共 37 个、`updated_at` 触发器共 13 个。

## 3. 资产身份与归一化

资产类型与身份键类型一致：`root_domain`、`subdomain`、`ip`、`service`、`app`、`endpoint`。CIDR 只作为公司/任务范围条目使用（`NormalizeCIDR`），不会成为资产类型。

身份键在写入存储之前**必须**经 `internal/domain/asset` 归一化；存储层只做非空校验，不做二次归一化：

| 类型 | 归一化规则 |
| --- | --- |
| 域名（根域 / 子域） | 去首尾空白、转小写、去掉结尾的 `.`；校验 DNS 标签（字母/数字/连字符，单标签 ≤ 63 字节、总长 ≤ 253 字节，不能以连字符开头/结尾）；拒绝 IP 字面量、协议、端口、路径、查询、用户信息、空格、下划线、通配符，拒绝纯数字顶级标签 |
| IP | 去首尾空白，输出标准形式；IPv6 用 RFC 5952 压缩形式，IPv4 映射的 IPv6 归一为 IPv4；拒绝 CIDR、端口、方括号、zone 后缀 |
| CIDR | 去首尾空白并抹掉主机位（`10.0.0.5/24` 与 `10.0.0.0/24` 同键），IPv6 压缩；必须显式包含前缀长度，拒绝裸 IP |
| 服务（`host:port`） | 去首尾空白，必须是显式 `host:port`；端口十进制 1..65535（前导零去掉）；IPv6 主机保留方括号 |
| 应用 | 仅去首尾空白，保留大小写（应用名与 bundle id 大小写敏感） |
| 端点（URL） | 必须显式 `http` / `https`；scheme 与主机名小写；去掉默认端口（http 80、https 443）；去掉 fragment 与用户信息；保留 path/query；空路径与 `/` 统一为不带结尾斜杠 |

归一化的结果是稳定的字符串键，写进 `asset_identity (tenant_id, kind, key)`，因此同一被测对象无论以何种书写形式、被多少个任务发现，都落到同一行资产。

端点身份**不包含 HTTP 方法**：`GET /a` 与 `POST /a` 是同一个 endpoint 资产，方法属于请求属性（可放在 `attrs` 中），不参与资产去重。

## 4. 资产 upsert 语义

`Store.UpsertAsset` 在一个事务内按以下规则写入：

1. 对输入身份去重，校验类型与键非空。
2. 逐个身份查询 `asset_identity`：
   - 任一身份已存在 → 复用该资产；
   - 同一批身份指向**多个不同资产** → 返回 `ErrIdentityConflict`（需要人工合并，语义上不能自动挑一个）；
   - 身份已存在的资产类型与输入类型不一致 → 同样返回 `ErrIdentityConflict`。
3. 全部身份都不存在 → 新建资产；资产已存在 → 更新 `label`、`attrs`，`parent_id` 仅在输入非空时覆盖（`COALESCE`）。写入前会校验 `parent_id` 指向的父资产存在、属于同一租户，且不是资产自身。
4. 把输入身份逐个插入 `asset_identity`，冲突时忽略（`ON CONFLICT DO NOTHING`），从而“补充身份”是幂等的。

因此一行资产可以有多条身份键（例如同时以子域名和 IP 建立身份），任一身份都能通过 `ResolveAssetByIdentity` 解析回同一资产。

## 5. 作战图语义

节点 `kind` 共七种：`goal`（目标）、`hypothesis`（假设）、`action`（动作）、`fact`（事实）、`finding`（发现）、`hint`（提示）、`digest`（折叠摘要）。状态 `state` 跨类型共享五种：`open`（待开始）、`running`（执行中）、`done`（已完成）、`exhausted`（已穷尽）、`blocked`（受阻）。

类型与状态的组合由 `graph.ValidateNode` 约束（例如知识类节点 `fact` / `finding` / `hint` / `digest` 不允许 `running`，`goal` 不允许 `running` / `exhausted`）。`AddNode` 与 `SetNodeState` 都会调用该函数，因此状态更新也不能绕过领域规则。

领域校验（`internal/domain/graph`）只收紧两类组合：目标不允许 `running` / `exhausted`（目标没有“执行中”，也不会“穷尽”）；事实、发现、提示、折叠摘要不允许 `running`（知识类节点写入即终态，不会被领取执行）。其余组合由上层状态机约定。

边 `rel` 共五种，方向统一为“上游 → 下游”：`spawns`（派生）、`derived_from`（推导自）、`yields`（动作产出事实/发现）、`proves`（证据证明目标）、`covers`（折叠摘要覆盖成员，属派生态而非因果，遍历因果链时必须排除）。

锚点 `role` 共四种：`target`（授权目标，进入覆盖度分母）、`touched`（仅接触过）、`tested`（确实测试并留下事实，进入分子）、`evidence`（漏洞证据引用，不计入已测覆盖度）。锚点通过 `operation_anchors` 把节点挂到资产，作战图与资产图只在这里交汇。

## 6. 覆盖度

定义（与迁移注释、`Store.Coverage` 的 SQL 一致）：

```text
分母 = mission_asset_links 中本任务的资产
     ∪ 本任务中 role = 'target' 的锚点所指向的资产
分子 = 本任务中 kind = 'fact' 的节点上 role = 'tested' 的锚点所指向的资产
```

要点：

- **只有 `fact` 节点上的 `tested` 锚点计入分子**；把 `tested` 挂在假设或动作上不会增加覆盖度。
- **`hint` / `touched` 不进入分母**，这是刻意设计：挂提示、仅接触资产不应把覆盖度拉低或虚增。需要把范围外资产纳入统计时，显式加 `target` 锚点（这会扩充分母）。
- 结果按 `assets.type` 分组返回 `ByType`，并汇总 `Total` / `Tested` / `Percent`；`Percent = Tested × 100 / Total`，`Total = 0` 时为 0，不做四舍五入。
- 例：任务范围 `root_domain = example.com` 解析出根域 + 2 个子域 → `0/3`；`fact` 节点 `tested` 锚定其中一个子域 → `1/3`；`hint` 节点 `touched` 锚定范围外 IP → 仍为 `1/3`；再用 `target` 锚定该 IP → `1/4`。

## 7. 任务范围解析

`Store.LinkMissionAssetsByScope` 把 `mission_scope` 解析为 `mission_asset_links`（`source = 'scope'`，冲突忽略，返回新增链接数）：

- `root_domain`：根域（`type = 'root_domain'` 且 `label = value`）+ 子域（`label = value` 或以 `.value` 结尾，即 `label LIKE '%.' || value`）。
- `subdomain` / `ip` / `url`：按类型精确匹配（`url` 匹配 `endpoint`）。
- `cidr`：`type = 'ip'` 且 `label::inet <<= '<cidr>'::cidr`。
- `keyword`：只保存为范围条目，不解析为资产（避免“看起来生效”的假象）。

资产列表 `Store.ListAssets` 支持按 `type`、`parent_id` 过滤与 `id` 游标分页：`limit` 默认 50、上限 500，按 `id` 升序；列表不加载身份键，避免 N+1 查询，需要身份时用 `GetAsset`。

## 8. 迁移机制

- 文件名必须匹配 `NNNN_name.sql`，版本号唯一且为正；按版本升序应用，随二进制嵌入。
- 已发布迁移**只增不改**：`schema_migrations` 记录每个版本的 SHA-256 校验和，校验和不匹配即视为漂移并拒绝启动。
- 应用前获取会话级 advisory lock，使并发启动（多副本）串行化；每个迁移与它的版本记录在同一事务中提交。
- 任何迁移失败都会中止启动，不存在“尽力而为”模式。

## 9. 性能实测

环境：PostgreSQL 16（本地 Docker），10 万 IP 资产 + 10 万 `mission_asset_links`；每组 20 次取 P95。测试见 `TestAssetQueryPerf100k`，仅在 `BEACONX_PERF=1` 时运行。

| 场景 | P95 实测 | 阈值 |
| --- | --- | --- |
| 资产列表（`type = ip`，`limit = 50`） | ≈ 4.1ms | 200ms |
| 覆盖度查询（10 万资产） | ≈ 83ms | 200ms |

本机复测约为 4.2ms 与 76ms，随机器与数据分布波动；阈值仍是 200ms，超出即测试失败。

## 10. 常用查询与维护片段

按租户清理（级联删除该租户全部资产、任务、作战图数据，谨慎执行）：

```sql
DELETE FROM tenants WHERE id = $1;
```

查看某任务尚未覆盖的资产（在分母中、但没有 fact 节点 tested 锚定）：

```sql
WITH denom AS (
    SELECT asset_id FROM mission_asset_links
    WHERE tenant_id = $1 AND mission_id = $2
    UNION
    SELECT a.asset_id
    FROM operation_anchors a
    JOIN operation_nodes n ON n.tenant_id = a.tenant_id AND n.id = a.node_id
    WHERE a.tenant_id = $1 AND n.mission_id = $2 AND a.role = 'target'
), tested AS (
    SELECT DISTINCT a.asset_id
    FROM operation_anchors a
    JOIN operation_nodes n ON n.tenant_id = a.tenant_id AND n.id = a.node_id
    WHERE a.tenant_id = $1 AND n.mission_id = $2
      AND a.role = 'tested' AND n.kind = 'fact'
)
SELECT ast.id, ast.type, ast.label
FROM denom d
JOIN assets ast ON ast.tenant_id = $1 AND ast.id = d.asset_id
LEFT JOIN tested t ON t.asset_id = d.asset_id
WHERE t.asset_id IS NULL
ORDER BY ast.id;
```

按类型统计租户资产规模：

```sql
SELECT type, COUNT(*) FROM assets WHERE tenant_id = $1 GROUP BY type ORDER BY type;
```

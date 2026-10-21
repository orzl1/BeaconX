# BeaconX 项目地图（唯一真源）

> **本文档是 BeaconX 代码功能结构的唯一真源。** 任何新增/修改 API、数据表、状态机、模块、配置项、运行时机制或前端可用的字段与行为，都必须在**同一次改动**里更新本文件对应章节，并在文末「变更记录」追加一条。只改内部实现、不改变对外行为的重构，也要记一条变更记录。

| 项目 | 值 |
| --- | --- |
| 文档版本 | v1.2 |
| 更新日期 | 2026-10-21 |
| 适用 commit | 本次提交（当前工作区，未提交） |
| 覆盖里程碑 | M0–M6 现状（后端 + 前端 + 交付）+ M6.2-B1（资产/企业/范围/覆盖率）+ M6.2-B2（范围/计划/图/发现/复测/报告）+ M6.2-B3（流量/命令/LLM 记录/用量/Dashboard） |
| 路由数量 | 70 条（M0–M6 共 21 条 + M6.2-B1 共 18 条 + M6.2-B2 共 20 条 + M6.2-B3 共 11 条） |
| 校验命令 | `make docs-check`（校验本文档覆盖全部 HTTP 路由） |
| 文档语言 | 中文；标识符、JSON 字段、错误码、环境变量、SQL、路径保留英文 |

## 1. 文档定位与强制更新规则

### 1.1 定位

- 面向**前端设计与开发**：前端只需要读本文档的 §5（API）、§6（状态机/业务规则）、§8（对接指南）、§9（页面映射与缺口）。
- 面向**后端开发**：§3（代码地图）、§4（数据模型）、§7（运行时机制）、§11（配置）、§13（一致性清单）。
- 面向**评审/交付**：§2（总览）、§12（测试/CI/发布）、§14（变更记录）。

事实来源是当前仓库代码（HEAD `bfb2540`）。本文档描述的是**已实现行为**；未实现或不确定的能力必须标注「未实现 / 缺口 / 待确认」，不得写成已实现。

### 1.2 何时必须更新

| 变更类型 | 必须更新本文档的章节 | 同提交还需更新 |
| --- | --- | --- |
| 新增/删除/修改 HTTP API | §5、§8（若前端可用）、§9（缺口状态） | `api/openapi.yaml`、必要时 README 接口表 |
| 新增数据库迁移/表/字段/索引 | §4、§6（若涉及状态） | `docs/data-model.md` |
| 修改任务/步骤/审批状态机 | §6、§7、§8 操作矩阵 | — |
| 新增/修改配置项或环境变量 | §11 | `docs/configuration.md`、`config.example.json`、`.env.example` |
| 修改运行时机制（调度/租约/规划/执行/事件/路由/自更新） | §7、必要时 §3 | `docs/runtime.md` / `docs/ai.md` / `docs/release.md` |
| 修改前端可用的字段、错误码、SSE 行为 | §5、§8 | `docs/web.md` |
| 修改策略/证据/审批行为 | §6、§7 | `docs/tools-policy.md` / `docs/evidence.md` |
| 仅内部重构、无对外行为变化 | §14 变更记录（注明“无对外行为变化”） | — |

### 1.3 校验与流程

- `make docs-check` 执行 `go test ./internal/httpapi -run TestProjectMapDocumentsAllRoutes`：遍历 `Routes()`，要求每个 `Pattern` 都出现在本文档中（`internal/httpapi/doc_sync_test.go`）。
- CI 的 lint job 已包含 `make docs-check`（`.github/workflows/ci.yml`）；PR 模板也有勾选项。
- PR 模板（`.github/pull_request_template.md`）要求勾选“已更新 project-map 变更记录”。
- 迁移只追加、不修改已发布文件；校验和漂移会拒绝启动（`internal/infra/migrate`）。

### 1.4 配套文档

| 文档 | 内容 | 与本文档关系 |
| --- | --- | --- |
| `docs/data-model.md` | 表结构、双图、覆盖度、证据表 | §4 的展开版 |
| `docs/runtime.md` | 状态机、调度、租约、恢复、SSE、鉴权 | §6/§7 的展开版 |
| `docs/ai.md` | LLM 客户端、计划模型、AI 规划/执行、预算、路由 | §6/§7 的展开版 |
| `docs/tools-policy.md` | 工具注册表、策略规则、审批、审计、安全盲区 | §6/§7 的展开版 |
| `docs/evidence.md` | 两阶段采集、加密 blob、快照签名、篡改校验 | §4/§6/§7 的展开版 |
| `docs/web.md` | 前端架构、开发/构建、功能与限制 | §8 的展开版 |
| `docs/release.md` | 发布流水线、自更新、镜像交付 | §12 的展开版 |
| `docs/configuration.md` | 配置项与环境变量 | §11 的展开版 |

## 2. 项目总览

### 2.1 技术栈

| 层 | 技术 | 说明 |
| --- | --- | --- |
| 语言 | Go 1.24 | 纯 Go、`CGO_ENABLED=0`，交叉编译 |
| HTTP | 标准库 `net/http` + `ServeMux` | 声明式路由表；Go 1.22+ 的 `{id}` 与 `PathValue` |
| 数据库 | PostgreSQL 16 + `pgx/v5` | 迁移文件嵌入二进制；无 ORM |
| 事件流 | 内存 Broker + SSE | 先订阅后回放，`Last-Event-ID`/`?since=` |
| AI | OpenAI 兼容 Chat Completions | 官方 SDK 无依赖，标准库实现；支持工具调用与多 profile 路由 |
| 前端 | Next.js 16（App Router）+ React 19 + TypeScript + 原生 CSS | 静态导出 `output: export`，由 Go `go:embed` 托管 |
| 交付 | Docker 多阶段 + GitHub Actions Release | 单镜像含前后端；自更新 SHA256SUMS |

### 2.2 目录结构

| 路径 | 职责 |
| --- | --- |
| `cmd/beaconx/` | 进程入口：`serve` / `migrate` / `healthcheck` / `version` / `help` |
| `internal/config/` | 配置加载（默认值 → JSON 文件 → 环境变量）、校验 |
| `internal/domain/` | 纯领域模型：`task`（状态机）、`plan`（计划决策）、`policy`（策略）、`graph`（作战图枚举）、`asset`（资产身份归一化） |
| `internal/app/` | 应用层：`runner`（引擎）、`ai`（LLM 规划/执行）、`executor`（工具执行）、`tools`（注册表与内置工具）、`events`（事件总线）、`evidence`（证据存储）、`system`（系统信息） |
| `internal/infra/` | 基础设施：`pg`（Store）、`llm`（客户端/路由/错误分类）、`selfupdate`（自更新）、`migrate`（迁移执行） |
| `internal/httpapi/` | HTTP 路由、中间件、处理器、SSE、鉴权、审批/证据/更新 API |
| `internal/webui/` | 前端静态资源 `go:embed` 与回退逻辑；`dist/` 为构建产物 |
| `internal/logging/` | slog 结构化日志 |
| `internal/platform/` | 状态目录（`$BEACONX_HOME`，按租户分区，0700） |
| `internal/tenant/` | 租户上下文（`tenant.With/From/MustFrom`） |
| `web/` | Next.js 前端源码（单页控制台） |
| `migrations/` | `0001`–`0006` SQL 迁移 + `embed.go` |
| `api/` | `openapi.yaml`（API 契约，生成客户端的来源） |
| `docs/` | 本文档与配套文档 |
| `scripts/` | CI 辅助（`check-brand.sh` 品牌门禁） |

### 2.3 分层与依赖方向

```text
cmd/beaconx
  ├── config / logging / platform / tenant / webui
  ├── httpapi ──► app/* / infra/* / domain/task / tenant
  ├── app/runner ──► domain/{task,graph} + infra/pg
  ├── app/ai ──► domain/{plan,graph} + infra/llm + infra/pg
  ├── app/executor ──► app/tools + domain/policy + domain/task + infra/pg + app/evidence
  ├── app/tools ──► domain/policy
  ├── app/evidence ──► 标准库（crypto/os）
  ├── infra/pg ──► domain/{asset,graph,plan,policy,task} + app/evidence
  ├── infra/llm ──► 标准库
  └── domain/* ──► 仅标准库（不依赖数据库/网络/时钟）
```

**规则**：`domain/*` 是纯函数层；`app/*` 编排；`infra/*` 承载 I/O；`httpapi` 只做协议适配。工具调用必须经 `internal/app/executor`，不得旁路。

### 2.4 启动顺序（`cmd/beaconx/main.go` 的 `serve`）

1. 解析命令行参数与配置路径 → `config.LoadFrom`（默认 `config.json`，不存在则用默认值）→ `config.Validate`。
2. 初始化日志（`logging.New`，json/text）。
3. 建立信号上下文（SIGINT/SIGTERM）。
4. 准备状态目录：`platformstate.New(State.Dir)` → `Ensure()` → `EnsureTenant(DefaultID)`。
5. 连接数据库：`pg.Open`（带重试，便于 compose 同时启动）。
6. 应用迁移：`migrate.Apply`（advisory lock + 校验和；失败拒绝启动）。
7. 组装 `system.Service`（版本、构建信息、schema 版本、启动时间）。
8. 创建事件总线 `events.NewBroker(512)` 与 `pg.NewStore(db)`，并定义 `EventSink`（任务事件 → Broker）。
9. 初始化证据存储：`evidence.key`（配置/状态目录读取或生成）→ `evidence.New`。
10. 幂等播种内置策略规则（`policy.SeedDefaults` → `EnsurePolicyRules`）。
11. 创建工具注册表（`http_probe`/`shell_exec`）与执行器 `executor.New`。
12. 若 `llm.base_url` 与 `llm.model` 同时配置：创建 `llm.Router`（主 profile + fallbacks），构造 `ai.Planner` / `ai.Worker` 并 `SetTools(executor, 工具定义)`；否则使用 `IdlePlanner`/`NoopWorker`。
13. 创建并启动引擎 `runner.New(...).Start(ctx)`：先恢复对账（过期租约、遗留 running/finalizing），再启动调度循环。
14. 解析管理令牌（`auth.admin_token` 或生成临时令牌并写日志）。
15. 创建自更新器 `selfupdate.New`（状态目录 `state.dir/selfupdate`）。
16. 构建 HTTP 处理器 `httpapi.NewHandler`（中间件、`Routes()`、WebUI `/`）→ `http.Server`（监听 `server.addr`，默认 `:6969`）。
17. `ListenAndServe`；收到信号后：`srv.Shutdown` → `engine.Stop` → `broker.Close`。

### 2.5 构建与运行命令

| 命令 | 作用 |
| --- | --- |
| `make build` | `CGO_ENABLED=0 go build -trimpath` → `bin/beaconx` |
| `make web` | `cd web && npm ci && NEXT_EXPORT=1 npm run build` → 复制 `web/out` 到 `internal/webui/dist` |
| `make run` | `go run ./cmd/beaconx serve` |
| `make test` / `make test-race` | 单元/集成测试（无 DB 时 DB 测试跳过） |
| `make lint` | gofmt + go vet + 品牌门禁 |
| `make docs-check` | 校验本文档覆盖全部路由 |
| `make docker-build` | 构建单镜像（含前端） |
| `make up` / `make down` | docker compose 启停 |
| `./bin/beaconx migrate` | 只应用迁移后退出 |
| `./bin/beaconx healthcheck -addr URL` | 探测健康检查端点 |
| `./bin/beaconx version` | 打印版本/commit/构建时间 |

## 3. 代码地图

### 3.1 包职责与关键符号

| 包 | 职责 | 关键类型/函数 | 关键文件 |
| --- | --- | --- | --- |
| `cmd/beaconx` | 进程入口 | `run/serve/migrateCmd/healthcheck`、`loadConfig`、`openDB`、`applyMigrations`、`loadOrCreateEvidenceKey`、`resolveAdminToken` | `main.go` |
| `internal/config` | 配置模型/加载/校验 | `Config`、`Load/LoadFrom`、`Default`、`Validate`、`Duration` | `config.go` |
| `internal/logging` | 结构化日志 | `New(Log)`（slog） | `logging.go` |
| `internal/platform` | 状态目录 | `New`、`Ensure`、`EnsureTenant`、`TenantDir` | `state.go` |
| `internal/tenant` | 租户上下文 | `ID`、`With`、`From`、`MustFrom`、`DefaultID=1` | `tenant.go` |
| `internal/httpapi` | HTTP 路由/中间件/处理器 | `NewHandler`、`Routes`（70 条）、`Options`、`withAuth`、`timeout`、`limitBody`、`writeError/writeJSON`、`recorder`、`newFallbackHandler`（404/405+Allow）、`TokenAuthenticator`、`SSEOptions.Handler`、任务/审批/证据/更新/企业/资产/范围/计划/作战图/节点/发现/复测/报告/洞察处理器 | `handler.go`、`auth.go`、`sse.go`、`missions.go`、`approvals.go`、`evidence_api.go`、`update.go`、`companies.go`、`assets.go`、`graph_api.go`、`findings_api.go`、`report_api.go`、`insights_api.go` |
| `internal/app/runner` | 任务运行时 | `Engine`、`Config`、`Planner`/`Worker` 接口、`IdlePlanner`、`NoopWorker`、`EventSink`；调度/租约/暂停/恢复/取消/删除/恢复对账/预算暂停 | `engine.go` |
| `internal/app/ai` | LLM 规划与执行 | `Planner.Plan`、`Worker.Execute`、`Worker.SetTools`、`runToolLoop`、`parseObservation`、`Config`、`LLM` 接口 | `ai.go`、`planner.go`、`worker.go` |
| `internal/app/executor` | 工具调用唯一入口 | `Executor.Execute/ExecuteText`、`beginCapture/finishCapture`、`waitApproval`、`audit`、`Config` | `executor.go` |
| `internal/app/tools` | 工具注册表与内置工具 | `Registry`、`Definition`、`Tool`、`Result`、`HTTPProbe`、`ShellExec` | `tools.go`、`http_probe.go`、`shell_exec.go` |
| `internal/app/events` | 进程内事件总线 | `Event`、`Broker`（`Subscribe/Publish/Subscribers/Close`） | `broker.go` |
| `internal/app/evidence` | 证据存储 | `Store`（`PutBlob/GetBlob/Seal/Verify`）、`Snapshot`、`BlobRef`、`ErrTampered`、`RedactHeaders/RedactJSON` | `store.go`、`snapshot.go`、`redact.go` |
| `internal/app/system` | 系统信息/就绪 | `Service`、`Info`、`Readiness`、`Check`、`BuildInfo` | `service.go` |
| `internal/infra/pg` | PostgreSQL Store | `DB`、`Store`、`Mission`、`Asset`、`Company`、`CompanyScope`、`AssetIdentity`、`Node/Edge/Anchor`、`MissionEvent`、`PlanStep`、`Approval`、`AuditEvent`、`TrafficExchange`、`EvidenceSnapshot`、`Coverage`、`Finding`、`FindingEvidence`、`Retest`；B1 的企业/资产/任务资产 CRUD；B2 的范围/发现/复测/报告数据；B3 的 `ListTrafficFilter/DeleteTraffic`、`ListAuditFilter/GetAuditEvent`、`ListLLMRecords`、`MissionUsage`、`LLMUsageSummary`、`Dashboard` | `pg.go`、`models.go`、`store*.go`、`store_asset_api.go`、`store_scope_api.go`、`store_finding.go`、`store_insights.go`、`mission_runtime.go`、`plan_store.go`、`policy_store.go`、`evidence_store.go` |
| `internal/infra/llm` | OpenAI 兼容客户端与路由 | `Client`、`Router`、`Profile`、`Message/ToolDef/ToolCall`、`APIError`、`ErrorKind`、`ClassifyError`、`RouteError` | `client.go`、`chat.go`、`tools.go`、`router.go`、`errors.go` |
| `internal/infra/selfupdate` | 自更新 | `Updater`（`Check/Apply/Rollback`）、`Release`、`Config`、`ErrNoUpdate` 等 | `selfupdate.go` |
| `internal/infra/migrate` | 迁移执行 | `Load/Apply/Latest`、`Migration`、`Result` | `migrate.go` |
| `internal/domain/task` | 任务状态机 | `Status`（7）、`EventType`（11）、`Effect`、`Reduce`、错误 `ErrInvalidStatus/ErrInvalidEvent/ErrTerminal/ErrIllegalTransition` | `task.go` |
| `internal/domain/plan` | 计划决策 | `Decision/Step/Action`、`ParseDecision/ParseDecisionWithKnown`、`Validate`、`ReadySteps`、`StepStatus` | `model.go`、`parse.go`、`validate.go`、`ready.go`、`status.go` |
| `internal/domain/policy` | 执行期策略 | `Rule/Action(allow/ask/deny)/Risk(low/medium/high)`、`Evaluate`、`ActiveRules`、`SeedDefaults` | `rule.go`、`evaluate.go`、`defaults.go`、`action.go` |
| `internal/domain/graph` | 作战图枚举 | `NodeKind`（7）、`NodeState`（5）、`EdgeRel`（5）、`AnchorRole`（4）、`ValidateNode/Edge/Anchor`、`ErrNotFound` | `graph.go` |
| `internal/domain/asset` | 资产身份归一化 | `Kind`（6）、`Identity`、`Normalize`、`NormalizeDomain/IP/CIDR/Service/URL` | `asset.go` |
| `internal/domain/finding` | 发现/复测领域模型 | `Severity`（5）、`Status`（6）、`RetestStatus`（5）、`ValidateTransition`、`ValidateRetestTransition`、`Terminal()`、错误 `ErrInvalidStatus/ErrIllegalTransition/ErrTerminalStatus/ErrInvalidRetestStatus/ErrIllegalRetestTransition/ErrRetestTerminal` | `finding.go`、`retest.go` |
| `internal/webui` | 前端托管 | `Handler()`：静态文件、`index.html` 回退、`/api/` JSON 404 | `webui.go` |
| `web/` | 前端源码 | `src/app/page.tsx`、`components/*`、`lib/api.ts`/`types.ts`/`format.ts`/`hooks.ts` | 见 §8 |

### 3.2 调用关系（主要路径）

```text
HTTP 请求
  httpapi.NewHandler → 中间件(recover→requestID→accessLog→tenant→timeout→limitBody)
    → mux: WebUI "/" 或 Routes()[withAuth] → handler
      → pg.Store / runner.Engine / selfupdate.Updater / evidence.Store / events.Broker

任务执行
  runner.Engine.scheduleLoop → pg.ListQueuedMissions → TransitionMission(queued→running)
  runner.runMission → Planner.Plan(ai.Planner) → 计划/动作入库
                    → (planner 周期) / Worker（领取动作）
  runner.claim → pg.ClaimNextIntent → ai.Worker.Execute
      → executor.ExecuteText → pg.ListPolicyRules → policy.Evaluate
        → [allow] → tools.Execute → pg.InsertAudit + begin/finishCapture(evidence)
        → [ask]   → pg.CreateApproval + mission_events(approval_required) → 轮询
        → [deny]  → pg.InsertAudit(denied_by_rule) → error
```

### 3.3 扩展点

| 想扩展 | 需要改哪里 | 注意 |
| --- | --- | --- |
| 新增工具 | 实现 `tools.Tool`（Definition + Execute）→ `main.serve` 注册到 Registry → 声明 `Roles` → 需要时补策略规则 | 工具调用必须走 executor；schema 只是声明，参数校验在 Execute |
| 新增/调整策略规则 | `domain/policy.SeedDefaults`（内置规则）或 `policy_rules` 数据；`executor` 自动读取 | 当前没有规则 CRUD API（M4.1 延期） |
| 新增 HTTP API | `httpapi.Routes()` 加路由 + `bind()` 加处理器 + 新 handler 文件 → 更新 `api/openapi.yaml` + 本文档 §5/§14 | 公开端点要设 `Public: true` |
| 新增数据库迁移 | `migrations/NNNN_*.sql`（只追加）→ 更新 §4 与 `docs/data-model.md` | 不要修改历史迁移 |
| 新增前端页面/组件 | `web/src/app`、`web/src/components` → `make web` 重新嵌入 | 当前是单页，无 URL 路由（M6.1） |
| 新增配置项 | `internal/config` 字段 + Default + applyEnv + Validate → 更新 §11 与三份配置示例 | 空环境变量视为未设置 |
| 新增任务事件类型 | `domain/task.EventType` + 状态机 + 前端 `STREAM_EVENT_TYPES` + §6/§8 | 事件类型必须同时登记前后端 |

## 4. 数据模型全表

迁移文件 `migrations/0001_init.sql` – `0007_findings.sql` 全部由 `embed.go` 嵌入；启动时按版本升序执行，校验和记录在 `schema_migrations`。所有业务表首列 `tenant_id`，主键/唯一键以 `tenant_id` 打头；租户外键指向 `tenants(id) ON DELETE CASCADE`。

### 4.1 迁移清单

| 迁移 | 内容 | 表数量 |
| --- | --- | --- |
| `0001_init` | 租户根表 | 1（tenants） |
| `0002_core_graph` | 公司/资产图/任务/范围/作战图核心表 + 索引 + 触发器 | 10（companies…operation_anchors） |
| `0003_runtime` | 任务状态词表收敛、排队/时间字段、动作租约列、`mission_events` | 1 表 + ALTER |
| `0004_planning` | 计划步骤/依赖/LLM 用量 + 任务预算列 | 3 表 + ALTER |
| `0005_policy` | 策略规则/审批/审计 | 3 表 |
| `0006_evidence` | 流量采集（两阶段）/证据快照 | 2 表 |
| `0007_findings` | 发现 / 发现证据关联 / 复测 + 索引 + 触发器 | 3 表 |

### 4.2 表清单与关键约束

#### 租户与公司

| 表 | 关键列 | 约束/索引 | 语义 |
| --- | --- | --- | --- |
| `tenants` | `id` PK、`slug` UNIQUE、`name`、`status`（active/suspended） | PK(id)；UNIQUE(slug)；`trg_tenants_updated_at` | 租户根表，0001 写入默认租户 id=1 |
| `companies` | `tenant_id,id`、`name`、`slug`、`attrs JSONB` | PK(tenant_id,id)；UNIQUE(tenant_id,slug)；FK tenants CASCADE；updated_at 触发器 | 资产归属公司（B1 已暴露 CRUD，见 §5 #19–#23） |
| `company_scope` | `company_id`、`kind`（root_domain/subdomain/ip/cidr/url/keyword/icp）、`value`、`source`（默认 manual；Store 只允许 manual/import） | PK(tenant_id,id)；UNIQUE(tenant_id,company_id,kind,value)；复合 FK companies CASCADE；索引 `idx_company_scope_company` | 公司级允许范围（B1 已暴露 API，见 §5 #24–#26） |

#### 资产图（Asset Graph）

| 表 | 关键列 | 约束/索引 | 语义 |
| --- | --- | --- | --- |
| `assets` | `type`（root_domain/subdomain/ip/service/app/endpoint）、`label`、`parent_id`、`attrs JSONB` | PK(tenant_id,id)；FK tenants CASCADE；索引 `idx_assets_type`、`idx_assets_parent` | 跨任务资产真值；`parent_id` 无 FK（应用层校验父资产） |
| `asset_identity` | `asset_id`、`kind`、`key` | PK(tenant_id,asset_id,kind,key)；UNIQUE(tenant_id,kind,key)；复合 FK assets CASCADE | 一行资产可多身份；upsert 并发正确性依赖唯一键 |

#### 任务、范围与任务资产

| 表 | 关键列 | 约束/索引 | 语义 |
| --- | --- | --- | --- |
| `missions` | `name/goal/description`、`status`（queued/running/paused/finalizing/done/failed/cancelled）、`queued`、`queued_at/started_at/finished_at/deadline_at`、`reason`、`max_tokens`、`spent_tokens`、`max_steps`、`steps_used` | PK(tenant_id,id)；FK tenants CASCADE；`idx_missions_queue`（`WHERE queued`）、`idx_missions_status`；updated_at 触发器 | 任务主体；状态词表由 0003 收敛，默认 `queued` |
| `mission_scope` | `mission_id`、`kind`（root_domain/subdomain/ip/cidr/url/keyword）、`value`、`allowed_actions TEXT[]`、`note` | PK(tenant_id,id)；UNIQUE(tenant_id,mission_id,kind,value)；复合 FK missions CASCADE；索引 `idx_mission_scope_mission` | 任务授权边界（B2 已暴露 API，见 §5 #37–#39） |
| `mission_asset_links` | `mission_id`、`asset_id`、`source`（scope/discovered/manual） | PK(tenant_id,mission_id,asset_id)；FK missions/assets CASCADE；索引 `idx_mission_assets_asset` | 任务资产登记；覆盖度分母主来源（B1 已暴露 API，见 §5 #33–#35） |

#### 作战图（Operation Graph）

| 表 | 关键列 | 约束/索引 | 语义 |
| --- | --- | --- | --- |
| `operation_nodes` | `mission_id`、`kind`（goal/hypothesis/action/fact/finding/hint/digest）、`state`（open/running/done/exhausted/blocked）、`title/body`、`payload JSONB`、`priority`、`lease_until`、`leased_by`、`attempts` | PK(tenant_id,id)；UNIQUE(tenant_id,mission_id,id)；复合 FK missions CASCADE；`idx_nodes_mission_kind_state`、`idx_nodes_mission_created`、`idx_nodes_claim`（`WHERE state IN ('open','running')`）；updated_at 触发器 | 作战图节点；action 节点被 worker 以租约领取 |
| `operation_edges` | `mission_id`、`src_id/dst_id`、`rel`（spawns/derived_from/yields/proves/covers） | PK(tenant_id,id)；UNIQUE(tenant_id,mission_id,src_id,dst_id,rel)；`CHECK(src_id<>dst_id)`；两个复合 FK → operation_nodes CASCADE；索引 `idx_edges_mission` | 因果边；复合外键保证两端同任务 |
| `operation_anchors` | `node_id`、`asset_id`、`role`（target/touched/tested/evidence） | PK(tenant_id,node_id,asset_id,role)；FK nodes/assets CASCADE；索引 `idx_anchors_asset_role` | 节点↔资产锚点；target 进覆盖度分母、fact+tested 进分子 |

#### 事件、计划与用量

| 表 | 关键列 | 约束/索引 | 语义 |
| --- | --- | --- | --- |
| `mission_events` | `mission_id`、`type`、`actor`(system)、`reason`、`payload JSONB`、`created_at` | PK(tenant_id,id)；FK missions CASCADE；索引 `idx_mission_events_stream(tenant_id,mission_id,id)` | 只追加事件流；SSE 回放与状态追溯来源 |
| `plan_steps` | `mission_id`、`key`（正则 `^[a-z0-9_-]{1,64}$`）、`title/detail`、`status`（pending/ready/running/done/skipped/failed）、`priority` | PK(tenant_id,id)；UNIQUE(tenant_id,mission_id,key)；UNIQUE(tenant_id,id)；FK missions CASCADE；索引 `idx_plan_steps_mission` | 跨规划轮次保留的长期计划（B2 已暴露只读 API，见 §5 #40） |
| `step_dependencies` | `step_id`、`depends_on_step_id` | PK(tenant_id,step_id,depends_on_step_id)；`CHECK(step_id<>depends_on_step_id)`；FK plan_steps CASCADE | 步骤依赖；只有 done 满足依赖 |
| `llm_usage` | `mission_id`（可空）、`role`、`model`、`prompt/completion/total_tokens`、`created_at` | PK(tenant_id,id)；FK missions/tenants CASCADE；索引 `idx_llm_usage_mission` | 逐次调用台账（Store 有 Record，**无查询 API**） |

#### 策略、审批与审计

| 表 | 关键列 | 约束/索引 | 语义 |
| --- | --- | --- | --- |
| `policy_rules` | `name`、`tool`、`arg_pattern`、`action`（allow/ask/deny）、`priority`、`timeout_seconds`、`enabled`、`immutable` | PK(tenant_id,id)；UNIQUE(tenant_id,name)；FK tenants CASCADE；索引 `idx_policy_rules_lookup(tenant_id,enabled,priority DESC,id)` | 策略规则；内置规则启动幂等播种；**无 CRUD API** |
| `approvals` | `mission_id`、`node_id`、`tool`、`args JSONB`、`status`（pending/approved/rejected/timeout/cancelled）、`note`、`decided_by`、`decided_at`、`expires_at` | PK(tenant_id,id)；FK missions/operation_nodes/tenants CASCADE；部分索引 `idx_approvals_pending`（`WHERE status='pending'`） | ask 决策的待办；只有 pending 可决策 |
| `audit_events` | `mission_id`、`node_id`（可空）、`tool`、`action`、`rule_name`、`args_digest`(sha256)、`outcome`、`detail`、`actor`、`created_at` | PK(tenant_id,id)；FK missions/tenants CASCADE；索引 `idx_audit_events_mission` | 每次工具调用一行；不保存原始参数（仅摘要） |

#### 流量与证据

| 表 | 关键列 | 约束/索引 | 语义 |
| --- | --- | --- | --- |
| `traffic_exchanges` | `mission_id`、`node_id`（可空）、`tool`、`phase`（intent/result）、`request_hash/size`、`response_hash/size`、`status_code`、`error`、`redaction`、`completed_at` | PK(tenant_id,id)；FK missions/tenants CASCADE；索引 `idx_traffic_exchanges_mission` | 一次工具调用一行，先 intent 后 result；崩溃留 intent |
| `evidence_snapshots` | `mission_id`、`tool`、`request_hash/size`、`response_hash/size`、`signature`、`redaction`、`created_at` | PK(tenant_id,id)；FK missions/tenants CASCADE；索引 `idx_evidence_snapshots_mission` | 冻结证据；正文在加密 blob，DB 只存引用与签名 |

#### 发现、发现证据与复测（0007）

| 表 | 关键列 | 约束/索引 | 语义 |
| --- | --- | --- | --- |
| `findings` | `mission_id`、`asset_id`（可空）、`node_id`（可空）、`title`、`severity`（info/low/medium/high/critical）、`status`（open/confirmed/fixed/retest/closed/false_positive）、`detail`、`created_at`、`updated_at` | PK(tenant_id,id)；FK missions/tenants CASCADE；索引 `idx_findings_mission`、部分索引 `idx_findings_asset`（`WHERE asset_id IS NOT NULL`）；`trg_findings_updated_at` | 发现生命周期；资产/节点**不建复合 FK**（删除时不能 SET NULL 置空 tenant_id），写入时应用层校验；B2 已暴露 API |
| `finding_evidence` | `finding_id`、`snapshot_id`、`created_at` | PK(tenant_id,finding_id,snapshot_id)；复合 FK findings/evidence_snapshots CASCADE；索引 `idx_finding_evidence_snapshot` | 发现 ↔ 证据快照多对多；重复绑定 409；快照删除级联解除绑定 |
| `retests` | `mission_id`、`finding_id`、`status`（pending/running/passed/failed/error）、`note`、`requested_by`、`started_at`、`finished_at`、`created_at` | PK(tenant_id,id)；FK missions/findings/tenants CASCADE；索引 `idx_retests_mission` | 一次复测请求一行；状态迁移维护 started_at/finished_at；B2 已暴露 API |

### 4.3 关系与语义

- **租户**：`tenants` 是唯一根；`companies`、`assets`、`missions`、`plan_steps` 等都以 `tenant_id` 关联并级联删除。
- **双图**：资产图（`companies`/`company_scope`/`assets`/`asset_identity`）跨任务共享；作战图（`operation_nodes`/`operation_edges`/`operation_anchors`）每任务私有；两图只通过 `operation_anchors.asset_id` 相连。
- **任务资产**：`mission_asset_links` 是任务与资产的关系表；`mission_scope` 是授权边界；覆盖度：分母 = links ∪ target 锚点，分子 = fact 节点 tested 锚点。
- **任务执行**：`missions` 1–N `mission_events`/`plan_steps`/`approvals`/`audit_events`/`traffic_exchanges`/`evidence_snapshots`/`findings`/`retests`；`operation_nodes` 1–N `operation_edges`（src/dst）与 `operation_anchors`。
- **证据链**：`traffic_exchanges`（采集）→ 冻结 → `evidence_snapshots`（签名 + 引用）→ `finding_evidence`（发现绑定证据）→ `findings`。
- **发现/复测**：`findings` 可选关联 `assets`/`operation_nodes`（应用层校验，无 FK）；`retests` 必须属于同一任务内的某条发现；`finding_evidence` 只存快照引用，正文仍在加密 blob。

### 4.4 状态/枚举字段与 CHECK

| 字段 | 取值 | 说明 |
| --- | --- | --- |
| `tenants.status` | active / suspended | 租户状态 |
| `missions.status` | queued / running / paused / finalizing / done / failed / cancelled | 任务状态机（§6） |
| `plan_steps.status` | pending / ready / running / done / skipped / failed | 计划步骤状态 |
| `operation_nodes.kind` | goal / hypothesis / action / fact / finding / hint / digest | 节点类型 |
| `operation_nodes.state` | open / running / done / exhausted / blocked | 节点状态（跨类型共享） |
| `operation_edges.rel` | spawns / derived_from / yields / proves / covers | 边语义 |
| `operation_anchors.role` | target / touched / tested / evidence | 锚点角色 |
| `policy_rules.action` | allow / ask / deny | 裁决 |
| `approvals.status` | pending / approved / rejected / timeout / cancelled | cancelled 目前无写入路径 |
| `traffic_exchanges.phase` | intent / result | 两阶段采集 |
| `mission_asset_links.source` | scope / discovered / manual | 任务资产来源 |
| `assets.type` / `asset_identity.kind` | root_domain / subdomain / ip / service / app / endpoint | 资产类型（注意没有 cidr） |
| `company_scope.kind` | root_domain / subdomain / ip / cidr / url / keyword / icp | 公司范围（有 icp） |
| `mission_scope.kind` | root_domain / subdomain / ip / cidr / url / keyword | 任务范围（无 icp） |
| `findings.severity` | info / low / medium / high / critical | 发现严重级别 |
| `findings.status` | open / confirmed / fixed / retest / closed / false_positive | 发现状态机（§6.10） |
| `retests.status` | pending / running / passed / failed / error | 复测状态机（§6.10） |

### 4.5 触发器与索引汇总

- `set_updated_at()` 触发器挂在 `tenants`、`companies`、`assets`、`missions`、`operation_nodes`、`findings` 六张表（0002/0007 创建）。
- 索引共 24 个（见各表行）：B1 的 20 个 + 0007 的 `idx_findings_mission`/`idx_findings_asset`/`idx_finding_evidence_snapshot`/`idx_retests_mission`。
- 查询路径约定：所有查询带 `tenant_id`；任务/审批/流量/审计/发现/复测按 `id` 或 `created_at DESC`；B1 企业/资产/范围/任务资产与 B2 任务范围/计划/图节点按 `id` 升序；任务准入按 `queued_at NULLS FIRST, id`；动作领取按 `priority DESC, id`（`FOR UPDATE SKIP LOCKED`）。

## 5. API 全表（当前 70 条）

### 5.1 通用约定

| 项目 | 约定 |
| --- | --- |
| 基址 | `http://<host>:6969`；API 前缀 `/api/v1`；前端与 API 同源 |
| 鉴权 | 除 `/healthz`、`/readyz`、`/api/v1/system/info`、`/api/v1/system/tenant` 外都需要令牌；`Authorization: Bearer <token>`；SSE 也支持 `?token=`（常量时间比较） |
| 失败关闭 | 未装配认证器时受保护路由返回 503 `auth_unavailable`；令牌错误 401 `unauthorized` |
| 错误信封 | `{"error":{"code":"...","message":"...","request_id":"..."}}`；401/409/503 等均使用该结构 |
| 请求体 | 服务端全局 `server.max_body_bytes`（默认 8MiB）；创建任务/审批决策使用 1MiB `MaxBytesReader` |
| 超时 | 全局 `server.request_timeout`（默认 30s）；超时且未写响应头 → 504 `timeout`；SSE 长连接同样受该超时约束（到期服务端结束流，EventSource 自动重连） |
| 租户 | 单租户 v1；`StaticTenant(DefaultID)` 注入上下文，所有查询带 `tenant_id` |
| 请求 ID | `X-Request-ID` 透传或生成，响应头回传并写入错误信封 |
| 分页 | 无 cursor/offset；列表通过 `limit`（部分接口支持 `status`/`severity`/`q`/`type`/`phase`/`outcome`/`role`/`model`/`mission_id` 过滤）；任务/审批/流量/命令/LLM 记录按 `id DESC`，发现/复测按 `created_at DESC, id DESC`，B1 企业/资产/范围/任务资产与 B2 任务范围/计划/图节点按 `id` 升序；B1 limit 非法静默回默认，**B3 的 traffic/commands/llm-records limit 非法（非数字/≤0）返回 400**，超上限钳制（traffic/commands 200、llm-records 500）；`/missions/{id}/usage` 的 records 不分页（单任务量级可控） |
| 内容类型 | 请求/响应 `application/json`（SSE 为 `text/event-stream`） |
| 幂等 | 创建任务非幂等；审批决策用 CAS（仅 pending）；删除任务 204 且重复删除 404；B1 资产按身份键 upsert、任务资产重复登记 409 |
| 方法/路径不匹配 | 路径存在但方法不支持 → 405 `method_not_allowed` + `Allow` 头；路径完全不存在 → JSON 404 `not_found` |

### 5.2 路由总表（当前 70 条）

| # | 方法 | 路径 | 鉴权 | 处理函数（文件） | 请求 | 成功响应 | 主要错误 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | GET | `/healthz` | 公开 | `handleHealthz`（`handler.go`） | — | `{"status":"ok"}` | — |
| 2 | GET | `/readyz` | 公开 | `handleReadyz` | — | `{"ready":bool,"checks":[{name,ok,error?}]}` | 503（未就绪） |
| 3 | GET | `/api/v1/system/info` | 公开 | `handleInfo` | — | `system.Info`（版本/commit/构建时间/启动时间/uptime/schema_version/latest_migration） | 503 `not_ready` |
| 4 | GET | `/api/v1/system/tenant` | 公开 | `handleTenant` | — | `{"tenant_id":1}` | — |
| 5 | POST | `/api/v1/missions` | Bearer | `handleCreateMission`（`missions.go`） | `{name*,goal,description,max_tokens,max_steps}` | 201 + `Mission` | 400/401/500/503 |
| 6 | GET | `/api/v1/missions` | Bearer | `handleListMissions` | `?status=&limit=`（默认 50；>200 回退 50） | `{"missions":[Mission]}` | 401/500 |
| 7 | GET | `/api/v1/missions/{id}` | Bearer | `handleGetMission` | — | `Mission` | 400/401/404 |
| 8 | POST | `/api/v1/missions/{id}/pause` | Bearer | `handlePauseMission` | — | 200 + `Mission` | 400/401/404/409 |
| 9 | POST | `/api/v1/missions/{id}/resume` | Bearer | `handleResumeMission` | — | 200 + `Mission` | 400/401/404/409 |
| 10 | POST | `/api/v1/missions/{id}/cancel` | Bearer | `handleCancelMission` | — | 200 + `Mission` | 400/401/404/409 |
| 11 | DELETE | `/api/v1/missions/{id}` | Bearer | `handleDeleteMission` | — | 204（无体） | 400/401/404 |
| 12 | GET | `/api/v1/missions/{id}/events` | Bearer 或 `?token=` | `handleMissionEvents`（`sse.go`） | `Last-Event-ID` 头或 `?since=` | `text/event-stream` | 400/401/404/500/503 |
| 13 | GET | `/api/v1/missions/{id}/audit` | Bearer | `handleMissionAudit`（`approvals.go`） | — | `{"audit":[AuditEvent]}`（最多 200） | 400/401/404/503 |
| 14 | GET | `/api/v1/approvals` | Bearer | `handleListApprovals` | `?status=&limit=`（默认 50；>200 回退 50） | `{"approvals":[Approval]}` | 401/500/503 |
| 15 | POST | `/api/v1/approvals/{id}/decide` | Bearer | `handleDecideApproval` | `{approve*,note}` | 200 + `Approval` | 400/401/409/503 |
| 16 | GET | `/api/v1/missions/{id}/traffic` | Bearer | `handleListTraffic`（`evidence_api.go`） | — | `{"traffic":[TrafficExchange]}`（最多 200） | 400/401/404/503 |
| 17 | POST | `/api/v1/traffic/{id}/evidence` | Bearer | `handleCreateEvidence` | 空 body | 201 + `EvidenceSnapshot` | 400/401/404/409/503 |
| 18 | GET | `/api/v1/evidence/{id}/verify` | Bearer | `handleVerifyEvidence` | — | `{"verified":true,"evidence_id":N,"signature":"…"}` | 400/401/404/409/503 |
| 19 | GET | `/api/v1/companies` | Bearer | `handleListCompanies`（`companies.go`） | `?q=&limit=` | `{"companies":[Company]}`（id 升序） | 400/401/503/500 |
| 20 | POST | `/api/v1/companies` | Bearer | `handleCreateCompany` | `{name*,slug*}` | 201 + `Company` | 400/401/409/503 |
| 21 | GET | `/api/v1/companies/{id}` | Bearer | `handleGetCompany` | — | `Company` | 400/401/404 |
| 22 | PATCH | `/api/v1/companies/{id}` | Bearer | `handleUpdateCompany` | `{name?,slug?}`（空串=不变） | `Company` | 400/401/404/409 |
| 23 | DELETE | `/api/v1/companies/{id}` | Bearer | `handleDeleteCompany` | — | 204 | 400/401/404 |
| 24 | GET | `/api/v1/companies/{id}/scope` | Bearer | `handleListCompanyScope` | — | `{"scope":[CompanyScope]}`（id 升序） | 400/401/404 |
| 25 | POST | `/api/v1/companies/{id}/scope` | Bearer | `handleAddCompanyScope` | `{kind*,value*,source?}`（source manual/import） | 201 + `CompanyScope` | 400/401/404/409 |
| 26 | DELETE | `/api/v1/companies/{id}/scope/{scope_id}` | Bearer | `handleDeleteCompanyScope` | — | 204 | 400/401/404 |
| 27 | GET | `/api/v1/assets` | Bearer | `handleListAssets`（`assets.go`） | `?type=&q=&limit=` | `{"assets":[Asset]}`（id 升序） | 400/401/503 |
| 28 | POST | `/api/v1/assets` | Bearer | `handleCreateAsset` | `{type*,label*,parent_id?,attrs?,identities?}` | 201 + `Asset`（带 identities 时 upsert） | 400/401/409/503 |
| 29 | GET | `/api/v1/assets/{id}` | Bearer | `handleGetAsset` | — | `Asset`（含身份键） | 400/401/404 |
| 30 | PATCH | `/api/v1/assets/{id}` | Bearer | `handleUpdateAsset` | `{label?,parent_id?,attrs?}`（`null` 清空父级） | `Asset` | 400/401/404 |
| 31 | DELETE | `/api/v1/assets/{id}` | Bearer | `handleDeleteAsset` | — | 204（子资产 parent_id 置空） | 400/401/404 |
| 32 | GET | `/api/v1/assets/{id}/identities` | Bearer | `handleListAssetIdentities` | — | `{"identities":[AssetIdentity]}` | 400/401/404 |
| 33 | GET | `/api/v1/missions/{id}/assets` | Bearer | `handleListMissionAssets` | — | `{"assets":[Asset]}`（id 升序） | 400/401/404 |
| 34 | POST | `/api/v1/missions/{id}/assets` | Bearer | `handleAddMissionAsset` | `{asset_id*,source?}`（manual/discovered/scope） | 201 + `{mission_id,asset_id,source}` | 400/401/404/409 |
| 35 | DELETE | `/api/v1/missions/{id}/assets/{asset_id}` | Bearer | `handleRemoveMissionAsset` | — | 204 | 400/401/404 |
| 36 | GET | `/api/v1/missions/{id}/coverage` | Bearer | `handleMissionCoverage` | — | `Coverage`（空任务全零、`by_type:[]`） | 400/401/404 |
| 37 | GET | `/api/v1/missions/{id}/scope` | Bearer | `handleListMissionScope`（`graph_api.go`） | — | `{"scope":[MissionScopeEntry]}`（id 升序） | 400/401/404/503 |
| 38 | POST | `/api/v1/missions/{id}/scope` | Bearer | `handleAddMissionScope` | `{kind*,value*,note?,allowed_actions?}` | 201 + `MissionScopeEntry` | 400/401/404/409/503 |
| 39 | DELETE | `/api/v1/missions/{id}/scope/{scope_id}` | Bearer | `handleDeleteMissionScope` | — | 204 | 400/401/404/503 |
| 40 | GET | `/api/v1/missions/{id}/plan` | Bearer | `handleMissionPlan` | — | `{"steps":[PlanStep]}`（空 `[]`） | 400/401/404/503 |
| 41 | GET | `/api/v1/missions/{id}/graph` | Bearer | `handleMissionGraph` | `?kind=&state=&limit=`（默认 500/上限 2000） | `{"nodes":[OperationNode],"edges":[OperationEdge],"anchors":[OperationAnchor]}` | 400/401/404/503 |
| 42 | GET | `/api/v1/nodes/{id}` | Bearer | `handleNodeGet` | — | `OperationNode` | 400/401/404/503 |
| 43 | PATCH | `/api/v1/nodes/{id}/state` | Bearer | `handleNodePatchState` | `{state*}` | 200 + `OperationNode` | 400/401/404/503 |
| 44 | GET | `/api/v1/missions/{id}/findings` | Bearer | `handleListFindings`（`findings_api.go`） | `?severity=&status=&limit=` | `{"findings":[Finding]}`（创建时间倒序） | 400/401/404/503 |
| 45 | POST | `/api/v1/missions/{id}/findings` | Bearer | `handleCreateFinding` | `{title*,severity?,detail?,asset_id?,node_id?}` | 201 + `Finding`（初始 open） | 400/401/404/503 |
| 46 | GET | `/api/v1/findings/{id}` | Bearer | `handleGetFinding` | — | `FindingDetail`（Finding 平铺 + `evidence:[]`） | 400/401/404/503 |
| 47 | PATCH | `/api/v1/findings/{id}` | Bearer | `handleUpdateFinding` | `{title?,severity?,detail?,asset_id?}`（asset_id 三态） | 200 + `Finding` | 400/401/404/503 |
| 48 | POST | `/api/v1/findings/{id}/status` | Bearer | `handleTransitionFinding` | `{status*}` | 200 + `Finding` | 400/401/404/409/503 |
| 49 | GET | `/api/v1/findings/{id}/evidence` | Bearer | `handleListFindingEvidence` | — | `{"evidence":[FindingEvidence]}` | 400/401/404/503 |
| 50 | POST | `/api/v1/findings/{id}/evidence` | Bearer | `handleAddFindingEvidence` | `{snapshot_id*}` | 201 + `{finding_id,snapshot_id}` | 400/401/404/409/503 |
| 51 | DELETE | `/api/v1/findings/{id}/evidence/{snapshot_id}` | Bearer | `handleRemoveFindingEvidence` | — | 204 | 400/401/404/503 |
| 52 | GET | `/api/v1/missions/{id}/retests` | Bearer | `handleListRetests` | `?limit=` | `{"retests":[Retest]}`（创建时间倒序） | 400/401/404/503 |
| 53 | POST | `/api/v1/missions/{id}/retests` | Bearer | `handleCreateRetest` | `{finding_id*,note?,requested_by?}` | 201 + `Retest`（初始 pending） | 400/401/404/503 |
| 54 | GET | `/api/v1/retests/{id}` | Bearer | `handleGetRetest` | — | `Retest` | 400/401/404/503 |
| 55 | PATCH | `/api/v1/retests/{id}` | Bearer | `handlePatchRetest` | `{status*,note?}` | 200 + `Retest` | 400/401/404/409/503 |
| 56 | GET | `/api/v1/missions/{id}/report` | Bearer | `handleMissionReport`（`report_api.go`） | — | `text/markdown; charset=utf-8`（Markdown 文本） | 400/401/404/503 |
| 57 | GET | `/api/v1/system/update/check` | Bearer | `handleUpdateCheck`（`update.go`） | — | `{"has_update":bool,"release":ReleaseInfo}` | 401/502/503 |
| 58 | POST | `/api/v1/system/update/apply` | Bearer | `handleUpdateApply` | 空 body | `{"updated":bool,"version":"…","message":"…"}` | 401/409/502/503 |
| 59 | POST | `/api/v1/system/update/rollback` | Bearer | `handleUpdateRollback` | 空 body | `{"rolled_back":true,"message":"…"}` | 401/409/503 |
| 60 | GET | `/api/v1/traffic` | Bearer | `handleTrafficList`（`insights_api.go`） | `?mission_id=&tool=&phase=&limit=` | `{"traffic":[TrafficExchange]}`（id DESC） | 400/401/503 |
| 61 | GET | `/api/v1/traffic/{id}` | Bearer | `handleTrafficGet` | — | `TrafficExchange`（不含正文） | 400/401/404/503 |
| 62 | GET | `/api/v1/traffic/{id}/request` | Bearer | `handleTrafficRequest` | — | `text/plain; charset=utf-8`（解密正文） | 400/401/404/409/503 |
| 63 | GET | `/api/v1/traffic/{id}/response` | Bearer | `handleTrafficResponse` | — | `text/plain; charset=utf-8`（解密正文） | 400/401/404/409/503 |
| 64 | DELETE | `/api/v1/traffic/{id}` | Bearer | `handleTrafficDelete` | — | 204（仅删采集行） | 400/401/404/503 |
| 65 | GET | `/api/v1/commands` | Bearer | `handleCommandList` | `?mission_id=&tool=&outcome=&limit=` | `{"commands":[AuditEvent]}`（id DESC） | 400/401/503 |
| 66 | GET | `/api/v1/commands/{id}` | Bearer | `handleCommandGet` | — | `AuditEvent` | 400/401/404/503 |
| 67 | GET | `/api/v1/llm-records` | Bearer | `handleLLMRecordList` | `?mission_id=&role=&model=&limit=` | `{"records":[LLMUsageRecord]}`（id DESC） | 400/401/503 |
| 68 | GET | `/api/v1/missions/{id}/usage` | Bearer | `handleMissionUsage` | — | `MissionUsage`（预算+records+by_role/by_model） | 400/401/404/503 |
| 69 | GET | `/api/v1/llm-usage/summary` | Bearer | `handleLLMUsageSummary` | `?group_by=model|role|day&mission_id=&days=` | `{"group_by":"…","groups":[LLMUsageGroup]}` | 400/401/503 |
| 70 | GET | `/api/v1/dashboard` | Bearer | `handleDashboard` | — | `DashboardStats` | 401/503 |

> 前端页面 `/` 与 `/_next/*` 由 `internal/webui.Handler` 提供（公开，不经过 `Routes()`）；`/api/` 下未匹配路径由 WebUI 返回 JSON 404。

### 5.3 复杂接口细节

**创建任务**（#5）

- 请求：`{"name":"必填","goal":"","description":"","max_tokens":0,"max_steps":0}`；`name` 只做 TrimSpace 后非空校验；`max_tokens/max_steps` 任一 >0 时调用 `SetMissionBudget`。
- 响应：`201` + `Mission`（初始 `status=queued`、`queued=true`、`queued_at=now`）。
- 写入：`CreateMission` →（可选预算）→ `AppendMissionEvent(created, actor=user)` → 发布 SSE。
- 错误：非法 JSON/空 name → 400 `bad_request`；Store/Engine 未装配 → 503 `runtime_unavailable`；DB 错误 → 500。

**任务列表**（#6）

- `status` 原样传给 SQL（无枚举校验）；`limit` 默认 50，`>200` 或 `<=0` 回退 50；按 `id DESC`。
- 返回 `{"missions":[…]}`；空列表是 `[]` 而非 null。

**暂停/恢复/取消**（#8–#10）调用 `runner.Engine.Pause/Resume/Cancel`：先做状态机 CAS 迁移（非法状态 → `ErrNotRunning` → 409 `conflict`），成功后再读取任务返回 200。语义见 §6/§7。

**删除任务**（#11）：先尽力取消，停止执行协程，`DELETE FROM missions`，级联删除节点/边/锚点/事件/范围/资产链接/计划/审批/审计/流量/证据行；返回 204。

**SSE 事件流**（#12）

- 处理顺序：解析任务 → 校验任务存在（404/500）→ **先订阅 Broker** → `Replay(afterID, limit=500)`（按 id 升序、跳过 `id<=afterID`）→ 写 SSE 头（`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`）→ 回放 → 实时事件按 `lastSentID` 去重。
- 帧格式：`id: <mission_event.id>` / `event: <mission_event.type>` / `data: <payload JSON>`，空行结束；心跳注释行 `: ping`（默认 20s）；每帧 Flush。
- 游标：`Last-Event-ID` 优先，其次 `?since=`；非法/负数按 0。
- 回放失败且未写帧 → 500 `replay_failed`；Broker/Store 未装配 → 503。
- **限制**：回放最多 500 条；连接受全局请求超时（30s）约束；慢消费者被 Broker 丢弃（缓冲 512）后连接结束，客户端需重连补洞。

**审批列表/决策**（#14–#15）

- 列表：`status` 可空；`limit` 默认 50、`>200` 回退 50；按 `id DESC`；`args` 为 JSONB 对象。
- 决策：body `{"approve":true|false,"note":"…"}`；`DecideApproval` 用 `WHERE status='pending'` CAS；已处理/不存在 → `applied=false` → 409 `conflict`；成功返回审批详情，`decided_by="user"`、`decided_at=now`。
- 审批超时由 executor 轮询触发 `ExpireApproval`（仅当 `expires_at < now()`）。

**流量/证据**（#16–#18）

- 流量列表：按 `id DESC`，最多 200；`phase` 可能为 `intent`（未完成）。
- 冻结证据：读取 `traffic_exchanges`；`phase != result` → 409 `conflict`；`evidence.Seal` 计算快照签名后写 `evidence_snapshots`；201 返回 DB 行（`evidence_id` 即行 id）。
- 校验：读快照 → 用 DB 字段重新 `Seal` 比对签名（不匹配 409 `tampered`）→ `GetBlob` 校验请求/响应正文哈希（失败 409 `tampered`）；成功 200。证据存储未装配 → 503 `evidence_unavailable`。

**企业/资产/范围/覆盖率**（#19–#36，M6.2-B1；实现见 `companies.go`、`assets.go`、`store_asset_api.go`）

- 列表：`?limit=` 缺省 50、上限 200（非数字/非正数按缺省）；企业 `?q=` 按 `name/slug` ILIKE 模糊，资产 `?q=` 按 `label` 模糊、`?type=` 精确；`%`/`_` 会被转义。列表按 `id` **升序**。
- 企业：创建 `{name,slug}` 均必填（≤200 字节），slug 唯一（冲突 409）；PATCH 空串表示保持原值，PATCH `{}` 幂等；删除企业级联删除其范围条目。
- 企业范围：`kind` 白名单 `root_domain/subdomain/ip/cidr/url/keyword/icp`，`value` 必填，`source` 只允许 `manual`（默认）/`import`；同一企业下 `(kind,value)` 重复 409；列表按 id 升序。
- 资产：`type` 白名单 `root_domain/subdomain/ip/service/app/endpoint`，`label` 必填；`parent_id` 必须存在、同租户且不能是自身（400）。`identities` 先经 `domain/asset.Normalize` 归一化（域名小写/去尾点、IP/CIDR 规范化等），非法 400。
- 创建资产：带 `identities` 走 **upsert**（同一身份键命中已有资产时复用并更新 label/attrs，返回 201）；不带 identities 创建“裸资产”（`CreateAsset`，不允许携带身份键）。身份键冲突/其它唯一键冲突 409。
- PATCH 资产：`label` 非空（≤1000 字节）；`parent_id` 缺省=不变、`null`=清空、数字=设置（自身/父不存在 400）；`attrs` 为对象；无可更新字段时幂等返回当前资产。删除资产：204，身份键与任务关联级联删除，子资产 `parent_id` 置空。
- 身份键：`GET /assets/{id}/identities` 只读（当前没有独立新增/删除/修改身份键的 API；创建资产时可写入）。
- 任务资产：`GET /missions/{id}/assets` 先校验任务存在（404）；POST body `{asset_id,source}`，`source` 缺省 manual、白名单 `scope/discovered/manual`，任务不存在 404、资产不存在 400、重复登记 409；DELETE 未登记 404；`GET /missions/{id}/coverage` 任务不存在 404，空任务返回 `{total:0,tested:0,percent:0,by_type:[]}`。
- 方法/路径兜底：路径存在但方法不支持 → 405 `method_not_allowed` + `Allow`；路径不存在 → JSON 404 `not_found`；Store 未装配 → 503 `store_unavailable`。

**任务范围/计划/作战图/节点**（#37–#43，M6.2-B2a；实现见 `graph_api.go`、`store_scope_api.go`）

- 任务范围：列表按 id 升序；POST body `{kind,value,note?,allowed_actions?}`，kind 白名单 6 种（root_domain/subdomain/ip/cidr/url/keyword），value 必填；重复 `(kind,value)` 409——实现是「先查后写 + 唯一键兜底」，并发窗口内仍可能由唯一键拒绝；成功 201 返回**条目对象**（含 id）；DELETE 未找到 404。
- 计划：`{"steps":[PlanStep]}`，空任务返回 `[]`；字段 key/title/detail/status/priority/depends_on。
- 作战图：一次返回 `{nodes,edges,anchors}`，空数组不是 null；`?kind=`/`?state=` 非法取值 400（不静默忽略）；`?limit=` 缺省 **500**、上限 **2000**，但 `pg.Store.ListNodes` 内部最大 **1000**——有过滤或 limit>1000 时实际先取 1000 条再在内存过滤/截断，因此超大数据集需要分页时结果可能不完整（已知限制）；边最多取 5000 条、锚点全量。
- 节点：`GET /nodes/{id}` 含 payload；`PATCH /nodes/{id}/state` 的 state 必须是 5 种之一，节点类型规则由 `graph.ValidateNode`（经 `SetNodeState`）校验，非法 400；节点不存在 404。
- 以上接口任务不存在均 404（`requireMission` 先确认），Store 未装配 503 `store_unavailable`。

**发现/发现证据/复测/报告**（#44–#56，M6.2-B2b；实现见 `findings_api.go`、`report_api.go`、`store_finding.go`、`domain/finding`）

- 发现列表：`?severity=`/`?status=` 非法取值 400；`?limit=` 缺省 50、上限 200；按 `created_at DESC, id DESC`；空 `[]`。
- 创建发现：title 必填（去空白），severity 缺省 `info`；asset_id/node_id 可选，资产需存在且同租户、节点需存在且属于同一任务，否则 400；初始 status=`open`，201。
- 详情：Finding 字段平铺 + `evidence:[]`；PATCH 支持 title/severity/detail/asset_id，asset_id 三态（缺省不变 / null 清空 / 数字设置），资产不存在 400。
- 状态流转：`POST /findings/{id}/status` body `{status}`；status 非法 400，非法迁移（含同状态、终态）409；成功返回更新后的 Finding。
- 发现证据：列表按绑定时间、快照 id 升序；绑定 body `{snapshot_id}`，快照必须存在且属于发现所在任务（否则 400），重复绑定 409，201 返回 `{finding_id,snapshot_id}`；解绑未绑定时 404，成功 204。
- 复测：列表按 `created_at DESC, id DESC`，limit 50/200；创建 body `{finding_id,note?,requested_by?}`，finding 必须存在且属于路径任务（否则 404/400），初始 `pending`，requested_by 缺省 `user`，201；详情 200；PATCH `{status,note?}`，status 非法 400、非法迁移 409，进入 running 时写 started_at、进入终态写 finished_at，note 非空才更新。
- 报告：`text/markdown; charset=utf-8`，只读投影，不调模型；包含任务信息、发现（最多 500）、计划步骤、fact 节点（最多展示 100 条并注明总数）、证据统计；空段落显式输出“无：…”。任务不存在 404。

**流量/命令/LLM 记录/用量/Dashboard**（#60–#70，M6.2-B3；实现见 `insights_api.go`、`store_insights.go`，无新迁移）

- 流量列表：`?mission_id=`（可选正整数；0/缺省=不筛选；非法 400）、`?tool=`（精确）、`?phase=`（仅 intent/result，否则 400）、`?limit=`（缺省 50、超上限 200 钳制；**非数字/≤0 → 400**，与 B1 静默回默认不同）；按 `id DESC`；过滤的 `mission_id` 不存在时返回空数组而不是 404（不校验任务存在）。
- 流量详情：按 id 返回 `TrafficExchange`（不含正文）；不存在 404。
- 请求/响应正文：`GET /traffic/{id}/request|response` 用 `evidence.GetBlob` 解密，`Content-Type: text/plain; charset=utf-8`；Evidence 未装配 503 `evidence_unavailable`；记录不存在 404；该阶段 hash 为空（如未完成 intent 的 response）→ 404；blob 缺失、密文被改、密钥不匹配统一 409 `tampered`（不再区分缺失与篡改，避免探测）。正文是脱敏后的明文，可能为空字符串（合法正文）。
- 删除流量：`DELETE /traffic/{id}` 204；只删 `traffic_exchanges` 行，正文 blob 与已冻结证据快照保留；不存在 404。
- 命令历史：`GET /commands` 是 `audit_events` 的只读视图，支持 `mission_id`/`tool`/`outcome` 过滤（不校验枚举值）、`limit` 缺省 50/上限 200、`id DESC`、空 `[]`；`GET /commands/{id}` 不存在 404。只读，无重放/取消。
- LLM 记录：`GET /llm-records` 读 `llm_usage`，支持 `mission_id`/`role`/`model` 过滤（不校验枚举）、`limit` 缺省 50/上限 500、`id DESC`；`mission_id` 可为 null（脱离任务的调用）。
- 任务用量：`GET /missions/{id}/usage` 先校验任务存在（404）；返回 `mission_id/max_tokens/spent_tokens/max_steps/steps_used/records/by_role/by_model`；records 为**该任务全部记录**（不分页，单任务量级可控），by_role/by_model 按名称升序，空 `[]`。
- 用量聚合：`GET /llm-usage/summary` 的 `group_by` 缺省 `model`，仅 model/role/day，否则 400；`days` 缺省 30、非数字/<1 → 400、>365 钳制到 365；时间窗为 `now().UTC() - days`；`day` 键为 `date_trunc('day', created_at)` 的 `YYYY-MM-DD`（**DB 会话时区**，容器通常 UTC）；`groups` 按 key 升序、空 `[]`；每项含 `calls/prompt_tokens/completion_tokens/total_tokens`。
- Dashboard：单次返回 `missions{total,by_status 固定 7 键}`、`tokens{spent_total,calls,today}`、`approvals{pending}`、`findings{total,by_severity 固定 5 键}`、`assets{total}`、`traffic{total}`、`recent_missions(≤5，id DESC)`。口径：`spent_total=SUM(missions.spent_tokens)`（任务累计预算消耗），`calls=COUNT(llm_usage)`，`today=SUM(llm_usage.total_tokens) WHERE created_at >= date_trunc('day', now())`（DB 时区）；分布中未知取值不进入 map，固定键补 0。所有计数均限定当前租户。

**自更新**（#57–#59）

- `check`：调用 GitHub latest release API，比较版本，返回本平台资产与 SHA256SUMS 信息；未装配 Updater → 503 `update_unavailable`；禁用/网络失败 → 502 `update_check_failed`。
- `apply`：检查→下载→校验和→`--version` 冒烟→备份 `.old`→替换；成功/无更新均 200；失败 409 `update_failed`；替换后需重启。
- `rollback`：用 `<binary>.old` 还原；无备份/失败 409 `rollback_failed`；不受 `enabled=false` 限制。

### 5.4 错误码字典

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `bad_request` | 请求体非法、任务/审批/证据/采集/发现/复测 id 非法、名称为空；资产类型/身份键/父资产非法、范围 kind/source 非法、任务资产引用预检失败；图的 kind/state 非法、节点状态非法、发现 severity/status 或资产/节点引用非法；流量 phase 非法、limit 非数字/≤0、group_by 非法、days 非数字/<1 |
| 401 | `unauthorized` | 缺少/错误令牌 |
| 404 | `not_found` | 任务/审批/采集/证据/企业/资产/企业范围条目/任务资产关联/任务范围条目/节点/发现/复测/证据绑定/流量记录/命令记录不存在；流量该阶段无正文（hash 空）；WebUI 下未知 `/api/` 路径 |
| 405 | `method_not_allowed` | 路径存在但请求方法不在路由表（响应带 `Allow` 头） |
| 409 | `conflict` | 当前状态不允许该操作（暂停/恢复/取消）；采集未完成冻结证据；审批已处理；企业 slug/范围条目/资产身份键/任务资产/任务范围重复；发现/复测非法状态迁移、证据重复绑定 |
| 409 | `tampered` | 证据签名或正文哈希校验失败；流量正文 blob 缺失/被篡改/密钥不匹配统一返回该错误 |
| 409 | `update_failed` / `rollback_failed` | 自更新/回滚失败 |
| 500 | `internal_error` | 未分类服务端错误 |
| 500 | `replay_failed` | SSE 回放失败（尚未开始流） |
| 500 | `sse_not_configured` | SSE 未配置任务解析器（正常不会出现） |
| 502 | `update_check_failed` | GitHub API/Release 检查失败、自更新禁用 |
| 503 | `auth_unavailable` | 未装配认证器（fail closed） |
| 503 | `runtime_unavailable` | Store/Engine 未装配（任务接口） |
| 503 | `store_unavailable` | Store 未装配（B1/B2 企业与资产域接口） |
| 503 | `sse_unavailable` | Broker/Store 未装配 |
| 503 | `evidence_unavailable` | 证据存储未装配 |
| 503 | `update_unavailable` | Updater 未装配 |
| 504 | `timeout` | 请求超时且未写响应头 |

### 5.5 未暴露但已存在的后端能力（前端暂时不能用）

以下 `pg.Store` 方法或数据已实现，但**没有 HTTP API**（详见 §9/§10）：

- 任务范围/计划/作战图：已随 B2a 暴露（§5 #37–#43）；仍无写入节点/边/锚点的 API（`AddNode/AddEdge/AddAnchor` 仅内部使用），图数据由 AI/执行路径写入。
- 资产细节：身份键只有 `GET /assets/{id}/identities`（只读）；没有独立新增/删除/修改身份键 API；企业 `attrs` 无 PATCH 入口。
- 任务事件：除 SSE 外没有分页查询历史事件的 API（`ListMissionEvents` 仅内部使用）。
- 流量/命令/LLM：B3 已暴露列表/详情/正文/聚合（§5 #60–#70）；仍缺流量导出、命令重放、LLM 请求/响应正文（仅 token 用量）、趋势按小时聚合。
- 用量/预算：`MissionBudget`、`RecordLLMUsage`（写入侧）、`ReadyPlanSteps`（内部调度）；查询侧已由 B3 暴露。
- 策略：`ListPolicyRules`（只读；没有 CRUD API）。
- 内部维护：`ListQueuedMissions/TransitionMission/SetMissionStatus/ListMissionScope` 等引擎/内部方法。

## 6. 领域状态机与业务规则

### 6.1 任务状态（7）

| 状态 | 中文 | 含义 | 终态 |
| --- | --- | --- | --- |
| `queued` | 排队中 | 已创建、等待准入；未占执行资源 | 否 |
| `running` | 运行中 | 已准入，runner 存活并领取动作 | 否 |
| `paused` | 已暂停 | 人为或预算暂停；runner 停止，可恢复 | 否 |
| `finalizing` | 收尾中 | 不再领新动作，等待收尾与总结 | 否 |
| `done` | 已完成 | 正常结束 | 是 |
| `failed` | 已失败 | 失败终止 | 是 |
| `cancelled` | 已取消 | 取消或删除（原因在事件 reason） | 是 |

实现：`internal/domain/task/task.go` 的 `Status.Valid/Terminal/String`。

### 6.2 任务事件（11）

| 事件 | 含义 | 备注 |
| --- | --- | --- |
| `created` | 任务创建 | 只从空状态接受 |
| `admitted` | 准入 | 启动 runner（EffectStartRunners） |
| `started` | runner 实际启动 | **保留值，当前状态机不消费**（发送会 `ErrIllegalTransition`） |
| `paused` | 暂停 | stop_runners；用户或预算暂停均用此事件 |
| `resumed` | 恢复 | 回到 queued |
| `finalizing` | 进入收尾 | stop_runners |
| `completed` | 完成 | finalizing→done |
| `failed` | 失败 | →failed |
| `cancelled` | 取消 | 与 deleted 同落 cancelled，reason 区分 |
| `deleted` | 删除 | 与 cancelled 同落 cancelled（实际删除接口还会删行） |
| `recovered` | 重启对账 | running/finalizing→queued |

### 6.3 合法迁移表（`task.Reduce`）

| 当前状态 | 事件 | 下一状态 | 副作用 |
| --- | --- | --- | --- |
| （空） | `created` | `queued` | — |
| `queued` | `admitted` | `running` | `start_runners` |
| `queued` | `cancelled` / `deleted` | `cancelled` | — |
| `queued` | `recovered` | `queued` | — |
| `running` | `paused` | `paused` | `stop_runners` |
| `running` | `finalizing` | `finalizing` | `stop_runners` |
| `running` | `failed` | `failed` | `stop_runners` |
| `running` | `cancelled` / `deleted` | `cancelled` | `stop_runners` |
| `running` | `recovered` | `queued` | `stop_runners` |
| `paused` | `resumed` | `queued` | — |
| `paused` | `cancelled` / `deleted` | `cancelled` | — |
| `finalizing` | `completed` | `done` | — |
| `finalizing` | `failed` | `failed` | — |
| `finalizing` | `cancelled` / `deleted` | `cancelled` | — |

其余组合返回 `ErrIllegalTransition`；未知状态/事件分别 `ErrInvalidStatus`/`ErrInvalidEvent`；终态任何事件 `ErrTerminal`。`Reduce` 是纯函数（不读时钟/DB），由 `pg.Store.TransitionMission` 以 CAS + 同事务事件落库（`internal/infra/pg/mission_runtime.go`）。

### 6.4 计划步骤状态（6）

`pending`（唯一可派发）→ `ready`（预留语义）→ `running` → `done` / `skipped` / `failed`。只有 `done` 满足依赖；`skipped`/`failed` 不满足。`ReadySteps(steps,status)` 在 `internal/domain/plan/ready.go`；决策校验 `Decision.Validate/ValidateWithKnown`（最多 20 步/10 动作、key 正则、依赖/环、动作 kind）在 `validate.go`；解析 `ParseDecision/ParseDecisionWithKnown` 在 `parse.go`。

### 6.5 审批状态机（5）

`pending` →（人工决策）`approved` / `rejected`；超时由 executor 调 `ExpireApproval` → `timeout`；`cancelled` 在 CHECK 中预留但**当前没有写入路径**（无取消审批 API）。`DecideApproval` 只允许 pending，CAS 更新。

### 6.6 工具风险与策略

| 维度 | 取值 | 位置 |
| --- | --- | --- |
| 策略裁决 | `allow` / `ask` / `deny` | `domain/policy/action.go` |
| 风险等级 | `low` / `medium` / `high` | 同上；工具声明（http_probe=medium，shell_exec=high） |
| 规则匹配 | 优先级降序（稳定排序）→ tool 精确或 `*` → `arg_pattern` 正则匹配**原始 JSON** → 第一条命中；无命中用 `default_action` | `domain/policy/evaluate.go` |
| 失败关闭 | 正则非法、命中规则动作非法、默认动作非法 → 返回错误，不降级放行 | 同上 |

内置五条规则（`SeedDefaults`，启动时 `EnsurePolicyRules` 幂等写入，不覆盖同名已有规则）：

| 规则名 | tool | arg_pattern（原文） | action | priority | timeout | immutable |
| --- | --- | --- | --- | --- | --- | --- |
| 允许 GET/HEAD 请求 | `http_probe` | `\"method\"\s*:\s*\"(GET\|HEAD)\"` | allow | 100 | 0 | 是 |
| 禁止下载并执行 | `shell_exec` | `(curl\|wget)[^"]*\|[^"]*(sh\|bash)` | deny | 200 | 0 | 是 |
| 禁止删除根目录 | `shell_exec` | `rm\s+-rf?\s+/(\s\|"\|\$)` | deny | 200 | 0 | 是 |
| shell 需人工审批 | `shell_exec` | （空，匹配全部参数） | ask | 50 | 60 | 否 |
| HTTP 其他方法需审批 | `http_probe` | （空） | ask | 50 | 60 | 否 |

> 注：上表的正则为源码字符串的近似展示（源码中引号写作 `\"`）。领域层 `Evaluate` 不读 `enabled`，由 Store 只查询启用规则；`immutable` 只是标记，当前没有规则 CRUD，API 不会删除/改内置规则。

### 6.7 预算语义

| 字段 | 默认 | 语义 |
| --- | --- | --- |
| `mission.max_tokens` | 0（不限） | token 预算；创建任务时可传 |
| `mission.spent_tokens` | 0 | 每次模型调用累加 `total_tokens`（同事务写 `llm_usage`） |
| `mission.max_steps` | 0（不限） | 步骤预算；`steps_used` 只统计实际新增步骤 |
| `mission.steps_used` | 0 | 已新增计划步骤数 |

- 预算为**软上限**：正在进行的模型调用不会中断，可能略微超限。
- token 超限：**引擎**在每轮循环把 `running → paused`（事件 `paused`，reason「模型 token 预算超限暂停」）；`resume` 后重新排队。
- step 超限：规划器写 `step_budget_exhausted` 并返回 done，按正常完成收尾；规划器内部仍保留 `budget_exhausted` 分支（正常会被引擎暂停先触发）。

### 6.8 证据规则（摘要）

- 两阶段：`traffic_exchanges.phase=intent` → 执行 → `phase=result`；崩溃留 intent。
- 正文加密 blob：`nonce(12)||AES-256-GCM`，AAD=明文 sha256；路径 `blobs/<hash[:2]>/<hash>.enc`；原子写入，已存在损坏不覆盖。
- 快照：规范化元数据（mission/tool/request/response/redaction/created_at UTC）→ ID=sha256；Signature=HMAC-SHA256（含 ID、不含 Signature）；DB 只存签名与引用。
- 校验：改 DB 字段 → 重算签名不一致；改/缺 blob → 哈希失败；API 均返回 409 `tampered`。
- 脱敏：头名单 + JSON 键名片段（password/token/secret/apikey 等）；纯文本输出解析失败时原文落盘（已知缺口）。

详细规则见 `docs/tools-policy.md`、`docs/evidence.md`。

### 6.9 资产、企业与范围规则（M6.2-B1）

- **资产类型/身份类型**（同一套 6 种）：`root_domain`、`subdomain`、`ip`、`service`、`app`、`endpoint`；先由 `domain/asset.Normalize` 归一化（域名小写/去尾点、IP 与 CIDR 规范化、URL/服务 key 规范化），再参与唯一键判定。
- **身份唯一性**：`UNIQUE(tenant_id,kind,key)`；创建资产带 `identities` 时走 `UpsertAsset`：身份已存在则复用该资产并更新 label/attrs（幂等）；身份指向不同资产/类型冲突时返回 409。带 identities 时不能创建“重复身份资产”；不带 identities 的 `CreateAsset` 只创建裸资产。
- **父子关系**：`assets.parent_id` 无数据库外键，由 Store 校验：父资产必须存在、同租户、不能是自身；删除父资产会把直接子资产的 `parent_id` 置空（不级联删除）。
- **企业范围**：`company_scope.kind` 7 种（比资产多 `cidr`、`keyword`、`icp`，没有 `service/app/endpoint`）；`source` 仅 `manual`（默认）/`import`；`(company_id,kind,value)` 唯一。
- **任务资产/来源**：`mission_asset_links.source` 仅 `scope`/`discovered`/`manual`；重复登记 409；登记时任务不存在 404、资产不存在 400。
- **覆盖度**：分母 = `mission_asset_links` ∪ 角色为 `target` 的锚点；分子 = `kind='fact'` 节点上角色为 `tested` 的锚点；`percent` 为 0–100（分母 0 时为 0），按资产类型汇总后按类型名排序。

### 6.10 发现与复测状态机（M6.2-B2b）

实现：`internal/domain/finding/finding.go`、`retest.go`；Store 在事务内 `SELECT ... FOR UPDATE` 后二次校验，非法迁移包装为 `ErrConflict` → HTTP **409**；status 取值非法 → 400。

**发现状态（6）**：

| 当前 | 允许迁移到 | 说明 |
| --- | --- | --- |
| `open` | `confirmed` / `false_positive` / `closed` | 新建发现初始状态 |
| `confirmed` | `fixed` / `retest` / `closed` | 已确认存在 |
| `retest` | `confirmed` / `fixed` / `closed` | 等待/正在复测 |
| `fixed` | `retest` / `closed` | 已修复（可再复测） |
| `closed` | —（终态） | 已关闭 |
| `false_positive` | —（终态） | 误报 |

规则：`open → open` 等**同状态不算迁移**（409）；终态任何迁移返回 `ErrTerminalStatus`（409）；未列出的边一律 409。

**复测状态（5）**：

| 当前 | 允许迁移到 | 说明 |
| --- | --- | --- |
| `pending` | `running` | 发起复测初始状态 |
| `running` | `passed` / `failed` / `error` | 进入 running 写 `started_at`，进入终态写 `finished_at` |
| `passed` / `failed` / `error` | —（终态） | 终态不可再迁移 |

### 6.11 任务范围、计划与作战图规则（M6.2-B2a）

- **任务范围**：kind 六种（`root_domain/subdomain/ip/cidr/url/keyword`，与 `mission_scope.kind` CHECK 一致），`(mission_id,kind,value)` 唯一；HTTP 创建时先查重返回 409，写入用 `AddMissionScope`（幂等 upsert）并由唯一键兜底并发。范围条目包含 `allowed_actions T[]` 与 `note`；当前没有「把范围编译成策略规则」的逻辑。
- **计划步骤**：只读 API 返回 `key/title/detail/status/priority/depends_on`；状态语义见 §6.4；API 不写步骤（写入由 AI Planner 完成）。
- **作战图/节点**：`kind` 7 种、`state` 5 种共享；`graph.ValidateNode` 按类型收敛状态：goal 不允许 running/exhausted，fact/finding/hint/digest 不允许 running（知识类节点写入即终态），action/hypothesis 允许全部 5 种；`PATCH /nodes/{id}/state` 非法状态 400，kind 规则不满足 400。
- **图查询**：只读；`edges` 两端同任务由数据库复合外键保证；节点/边/锚点由 AI/执行路径写入，当前没有写图 API。

### 6.12 洞察口径：流量正文 / 命令 / LLM 记录 / Dashboard（M6.2-B3）

- **流量正文**：`traffic_exchanges` 只存 hash/size；正文经 `evidence.GetBlob` 解密后以 `text/plain` 返回。`request_hash` 为空表示没有请求正文（404）；`response_hash` 为空通常表示采集未完成。blob 缺失/密文被改/密钥不匹配统一 409 `tampered`（不区分缺失与篡改，避免探测）；不装配 Evidence 时 503。
- **命令历史**：直接投影 `audit_events`（每次工具调用一行），`outcome` 取值 `executed/tool_error/denied_by_rule/rejected_by_user/approval_unavailable`；只读，不改审计、无重放能力。
- **LLM 记录**：直接投影 `llm_usage`（逐次调用台账，`mission_id` 可空）；与 `missions.spent_tokens` 的区别：记录表逐次，任务字段是累计值。
- **Dashboard 口径**：`missions.by_status` 固定 7 键（queued/running/paused/finalizing/done/failed/cancelled）且缺省补 0；`findings.by_severity` 固定 5 键（info/low/medium/high/critical）补 0；`tokens.spent_total=SUM(missions.spent_tokens)`、`tokens.calls=COUNT(llm_usage)`、`tokens.today=SUM(llm_usage.total_tokens) WHERE created_at >= date_trunc('day', now())`（DB 会话时区）；`recent_missions` 按 id 倒序最多 5 条。所有统计限定当前租户。
- **聚合时间窗**：`/llm-usage/summary` 的 `days` 使用 UTC 截止时间；`group_by=day` 的分组键由 DB 的 `date_trunc('day', created_at)` 生成（DB 时区），与 UTC 截止时间可能跨日不一致——前端展示时以返回的 key 为准。

## 7. 运行时机制

### 7.1 引擎调度与任务执行

- **准入调度**（`internal/app/runner/engine.go` 的 `scheduleLoop`）：每 **1s** 触发；可用槽位 = `max_concurrent - 运行中任务数`；`ListQueuedMissions` 按 `queued_at NULLS FIRST, id`（FIFO）取最多槽位数个任务；逐个 `TransitionMission(queued→running)` 并写 `admitted` 事件；成功后启动每任务协程。
- **每任务循环**（`runMission`）：规划 tick（`planner_interval`，默认 2s）与执行 tick（`worker_interval`，默认 200ms）交替；规划时先 `pauseIfOverBudget`；`Planner.Plan` 返回 done 且 `CountOpenIntents==0` 时执行 `running→finalizing→done`（事件 `finalizing`/`completed`）。
- **动作领取**：`ClaimNextIntent` 用 `FOR UPDATE SKIP LOCKED` 选中 `state='open'` 或「running 且租约过期/缺失」的动作，按 `priority DESC, id`；置 `running`、写 `lease_until=now+ttl`、`leased_by=owner`、`attempts+1`。
- **心跳/完成/释放**：执行期间心跳每 `heartbeat_interval`（10s）续约；`FinishIntent` 只接受 `done`/`exhausted`/`blocked` 且持有者匹配；`ReleaseMissionLeases` 在暂停/取消/删除时把 running 动作放回 open 并 `attempts+1`。
- **崩溃恢复**（`Engine.Start`）：`ResetExpiredLeases`（过期/缺失租约的 running→open）→ `ResetRunningMissions`（running/finalizing→queued，`queued=true`、`reason=进程重启恢复`，写 `recovered` 事件）。v1 假设单进程，无多副本协调。
- **预算暂停**：每轮循环开始检查 token 预算；超限把 running→paused（事件 `paused`，reason=模型 token 预算超限暂停），不硬失败；`resume` 回到 queued 重新准入。

### 7.2 暂停/恢复/取消/删除语义

| 操作 | 允许的状态 | 行为 | 失败 |
| --- | --- | --- | --- |
| pause | running | CAS running→paused + `paused` 事件（actor=user）；停协程（≤10s）；释放租约 | 409 `conflict` |
| resume | paused | paused→queued + `resumed` 事件；重新排队 | 409 |
| cancel | queued/running/paused/finalizing | →cancelled + `cancelled` 事件；停协程；释放租约 | 409 |
| delete | 任意 | 尽力取消 → 停协程 → `DELETE missions` 级联删除 | 404 |

终态（done/failed/cancelled）不接受任何事件；不存在接口。

### 7.3 AI Planner（`internal/app/ai/planner.go`）

| 步骤 | 行为 |
| --- | --- |
| 1 预算 | `MissionBudget`；`max_tokens>0 && spent>=max_tokens` → 事件 `budget_exhausted` 并返回 done（正常先被引擎暂停拦截） |
| 2 态势 | `ListNodes(500)` + `ListPlanSteps`；动作结果回写步骤：action done→step done，blocked/exhausted→step failed |
| 3 跳过空转 | 有 open/running 动作时直接返回（省 token） |
| 4 步骤预算 | `max_steps>0 && steps_used>=max_steps` → `step_budget_exhausted` 并返回 done |
| 5 快照 | 任务名/目标/步骤（key/标题/状态/依赖）/最近 10 条 fact（正文截断 300）/进行中动作数 |
| 6 决策 | 首次用拆解提示词、之后用下一步提示词；`CompleteJSON`（role=`planner`）；解析失败带错误重试一次（role=`planner-retry`） |
| 7 校验 | `ParseDecisionWithKnown`（已知步骤集合）：最多 20 步/10 动作、key 正则、依赖/环、动作引用 |
| 8 落库 | 单次≤8 个新步骤；`UpsertPlanSteps` 幂等（已有步骤保留标题/详情）；`steps_used += 实际新增数`；`ReadyPlanSteps` 选依赖已满足的 pending 步骤，每轮最多建 3 个 action 节点（`kind=action,state=open`，payload: step_key/kind/target/detail） |
| 9 完成 | 模型 done 但仍有未完成步骤 → 追问一次（role=`planner-followup`）；仍 done 则接受并把剩余非终态步骤标 `skipped`；只有 `CountOpenIntents==0` 才收尾 |

补充：规划/落库失败写事件 `planner_error` / `planner_persist_error` 并等下一轮；模型过早完成写 `planner_done_with_pending`。`ai.Config` 默认 `MaxStepsPerPlan=8`、`MaxActionsPerRound=3`。

### 7.4 AI Worker 与工具循环（`internal/app/ai/worker.go`）

- 装配工具（`SetTools`，仅 LLM 模式）后使用工具系统提示词：最多 **6 轮** `CompleteWithTools`（`tool_choice=auto`）；每轮把 `tool_calls` 逐个交给 `executor.ExecuteText`（missionID、action 节点 id、工具名、原始参数），工具结果以 `role=tool` 回灌（截断 4000 字符），错误也作为结果回灌；无 tool_calls 时得到最终文本；轮次用尽无结论写“工具调用轮次用尽，未给出最终结论”。
- 每次模型调用写 `llm_usage`（role=`worker-tool`）；未装配工具时走 `CompleteJSON`（role=`worker`）。
- 最终文本按 `{"fact":"…","detail":"…","done":true}` 解析为观察；解析失败退化为原文；写入 fact 节点（`kind=fact,state=done`，payload: action_id/step_key/kind/target），动作返回 done；模型调用失败动作置 blocked 并返回错误。

### 7.5 执行器（`internal/app/executor/executor.go`）

`Executor.Execute` 是工具调用唯一入口，顺序固定：

1. `Registry.Get(tool)`（未知工具 error）。
2. `ListPolicyRules`（只取 enabled，按 priority 倒序）。
3. `policy.Evaluate`；`deny` → 审计 `denied_by_rule` + error，不执行。
4. `allow` → 直接进入执行；`ask` → 用规则/配置超时创建 approval，写任务事件 `approval_required`（payload: approval_id/tool），每 300ms 轮询直到 approved/rejected/cancelled/timeout；未批准写审计 `rejected_by_user` 并返回错误；等待异常/ctx 取消写 `approval_unavailable`。
5. **执行前采集**：`BeginTraffic` 写入 intent（请求=工具参数 JSON，`RedactJSON` 后加密 PutBlob）。
6. `tool.Execute`。
7. **执行后采集**：`FinishTraffic` 写 result（响应=工具输出文本，redact 失败则原文；`status_code` 取 `Meta["status"]`，仅 http_probe；error 文本）。
8. 写审计：成功 `executed`、工具失败 `tool_error`（detail 截断 500）。

采集失败只记 WARN，不影响工具执行；**deny/审批拒绝的调用不会产生 traffic 行**（审计仍写）。

### 7.6 事件总线与 SSE 推送

- `events.Broker`：按 missionID 分发；订阅者独立缓冲（启动时 `NewBroker(512)`）；`Publish` 非阻塞，缓冲满即丢弃并关闭该订阅者；`Close` 关闭全部。
- SSE 处理器（`internal/httpapi/sse.go`）：先订阅再 `Replay`；游标 `Last-Event-ID` > `?since=`；回放按 id 升序、跳过 ≤游标；回放上限 500；实时按 `lastSentID` 去重；心跳 20s；每帧 flush；写失败/ctx 结束/通道关闭即退出并取消订阅。
- 任务事件写入 `mission_events` 后通过 `EventSink` 发布；因此 SSE 事件类型 = mission_events.type（可能包含 `planner_error` 等前端未登记的类型，见 §8.2）。

### 7.7 LLM 客户端与路由

- `llm.Client`：OpenAI 兼容 `/chat/completions`；`max_tokens`/`timeout`/`max_retries` 默认 2048/60s/2；仅 429/5xx/网络错误重试（指数退避 100ms 起）；JSON 模式被 HTTP 400 拒绝时回退普通请求；API Key 脱敏、请求/响应体 8MiB 限制。
- `llm.Router`：按 primary → `llm.fallbacks` 顺序；可切换类别 `auth`/`quota`/`rate_limit`/`transient`；不可切换 `context_overflow`/`fatal`（立即失败）；每次切换写一条 WARN「模型切换」；全部失败返回 `RouteError`（Attempts/Kind/Last）。
- `ClassifyError` 统一分类（`internal/infra/llm/errors.go`）；`ai.Planner/Worker` 只依赖 `LLM` 接口，可在 Client 与 Router 间替换。

### 7.8 自更新

`internal/infra/selfupdate`：检查 GitHub latest release → 版本比较（`dev` 视为旧）→ 下载本平台 zip（200MiB）与 SHA256SUMS（4MiB）→ sha256 校验 → 解压出 basename 为 `beaconx`/`beaconx.exe` 的条目 → 状态目录写 `beaconx.new` → `--version` 冒烟（10s）→ 备份当前二进制为 `<binary>.old` → rename 替换 → 写 `selfupdate/update.json`（version/installed_at，0600）。失败不改当前文件；`Rollback` 用 `.old` 还原；替换/回滚均需重启进程。状态目录为 `state.dir/selfupdate`；容器内通常无写权限，建议关闭自更新。

### 7.9 发现、复测与报告写入路径（M6.2-B2）

- **人工 API 唯一写入**：发现由 `POST /missions/{id}/findings` 创建、`PATCH /findings/{id}` 编辑、`POST /findings/{id}/status` 迁移；证据绑定由 `POST /findings/{id}/evidence`（校验快照同任务）；复测由 `POST /missions/{id}/retests` 创建、`PATCH /retests/{id}` 推进。AI 自动写 finding、复测执行器（自动跑验证并回写 passed/failed）**均未实现**。
- **状态机校验位置**：`internal/domain/finding` 纯函数校验，Store 事务内 `FOR UPDATE` 二次校验后写入；API 把非法迁移映射为 409，不修改其它字段。
- **报告生成器**：`internal/httpapi/report_api.go` 的 `renderMissionReport` 是只读投影，读取任务、发现（≤500）、计划步骤、fact 节点（≤100 展示，标注总数）与证据统计后拼 Markdown；不调用模型、不写库，结论为“同一数据同一报告（除生成时间）”。导出 PDF/Word 未实现。

### 7.10 B3 只读洞察投影（M6.2-B3）

- B3 不新增写入路径：`insights_api.go` 只读 `traffic_exchanges`/`audit_events`/`llm_usage`/`missions`/`approvals`/`findings`/`assets`，客户端读正文时调用 `evidence.GetBlob` 解密。
- 流量正文读取不缓存、不记录新的审计/采集行；`DELETE /traffic/{id}` 只删采集行，保留 blob 与已冻结快照（证据完整性优先）。
- Dashboard 每次请求实时聚合（多条 COUNT/SUM/分组 SQL，无物化视图/缓存）；数据量大时是已知性能风险，后续可加缓存或按日汇总。
- 命令历史与 LLM 记录是审计/台账视图，不触发任务运行时、不改变任务状态。

## 8. 前端对接指南

### 8.1 令牌与请求封装

| 项目 | 约定 |
| --- | --- |
| 获取令牌 | 启动日志中的临时令牌，或部署方配置的 `BEACONX_ADMIN_TOKEN` |
| 存储 | 浏览器 `localStorage`，键名 `beaconx.admin_token`（当前前端实现） |
| 普通请求 | `Authorization: Bearer <token>`；同源基址 `/api/v1`（推荐留空 `NEXT_PUBLIC_API_BASE`） |
| SSE | `EventSource` 不能带 header，用 `?token=<token>` |
| 错误解析 | 统一解析 `{"error":{"code","message","request_id"}}`；展示 message，附 code/request_id |
| 401 | 提示重新输入令牌；其他错误原样展示后端 message |
| 204 | 删除任务无响应体；不要尝试 JSON 解析 |
| CORS | 后端当前不提供 CORS，必须同源部署（Go 托管静态文件）；跨源需自行补后端 |

### 8.2 SSE 使用

- 地址：`GET /api/v1/missions/{id}/events?token=<token>&since=<cursor>`；断线重连由 `EventSource` 自动携带 `Last-Event-ID`。
- 帧：`id: <事件 id>`、`event: <类型>`、`data: <payload JSON>`；心跳为 `: ping` 注释行。
- **去重**：以 SSE `id` 为键（重连回放会重复下发）；页面建议只保留最近 N 条（当前前端 200 条）。
- **历史回放**：首次连接建议 `since=0`（或省略），服务端最多回放 500 条；更早历史未提供 API（缺口）。
- **连接时长**：受全局请求超时（30s）约束，服务端会主动结束；依赖 EventSource 自动重连 + Last-Event-ID 补洞，前端需容忍周期重连。
- **事件类型**（后端可写入 `mission_events` 的类型）：

| 类型 | 含义 | 前端是否已登记 |
| --- | --- | --- |
| `created` / `admitted` / `paused` / `resumed` / `finalizing` / `completed` / `failed` / `cancelled` / `deleted` / `recovered` | 任务状态迁移 | 是 |
| `started` | 保留未消费 | 是（但后端当前不发送） |
| `approval_required` | 需要人工审批 | 是 |
| `budget_exhausted` / `step_budget_exhausted` | 预算事件 | 是 |
| `planner_error` / `planner_persist_error` / `planner_done_with_pending` | 规划诊断 | **否（缺口：EventSource 只派发已注册命名事件，前端收不到）** |

### 8.3 推荐轮询周期（与当前前端一致）

| 数据 | 周期 | 说明 |
| --- | --- | --- |
| 任务列表 / 任务详情 | 5s | 与 SSE 状态变化互补 |
| 审批 pending 列表 | 5s | 审批无 SSE 推送（只有 approval_required 事件） |
| 流量列表 | 5s | 证据列表与任务详情同屏 |
| 系统探针（healthz/readyz/info） | 10s | 公开接口，无需令牌 |
| 企业/资产/范围/任务资产/覆盖度 | 10s | B1 新接口；编辑后立即重拉 |
| 任务范围/计划/作战图/节点 | 10s | B2a 新接口；图较大时降低频率或按需拉取 |
| 发现/复测 | 5s | B2b 新接口；状态迁移后立即重拉 |
| 报告 | 按需 | `GET /missions/{id}/report` 返回 Markdown，点击生成/下载，不轮询 |
| Dashboard | 15s | B3 聚合接口；实时 Count/Sum，无缓存，避免高频 |
| 流量列表 / 命令历史 / LLM 记录 | 10s 或按需 | B3 只读列表；分页为 `limit`，详情/正文按点击加载 |
| 流量请求/响应正文 | 按需 | 点击“查看正文”时请求，不预取（可能含敏感明文） |

### 8.4 字段字典

| 实体 | JSON 字段 | 说明 |
| --- | --- | --- |
| `Mission` | `tenant_id,id,name,goal,description,status,queued,queued_at,started_at,finished_at,deadline_at,reason,max_tokens,spent_tokens,max_steps,steps_used,created_at,updated_at` | 时间字段 RFC3339；`*_at` 可空（omitempty）；预算 0=不限；`status` 见 §6；`reason` 是最近迁移原因 |
| `Approval` | `tenant_id,id,mission_id,node_id,tool,args,status,note,decided_by,decided_at,expires_at,created_at` | `args` 为 JSON 对象；`status` pending/approved/rejected/timeout/cancelled；`decided_at` 可空 |
| `TrafficExchange` | `tenant_id,id,mission_id,node_id,tool,phase,request_hash,request_size,response_hash,response_size,status_code,error,redaction,created_at,completed_at` | `phase` intent/result；`status_code` 可空；`node_id` 可空；正文不在 JSON 中 |
| `EvidenceSnapshot` | `tenant_id,id,mission_id,tool,request_hash,request_size,response_hash,response_size,signature,redaction,created_at` | DB 行；正文在加密 blob；`signature` HMAC-SHA256 |
| `AuditEvent` | `tenant_id,id,mission_id,node_id,tool,action,rule_name,args_digest,outcome,detail,actor,created_at` | `action` allow/ask/deny；`outcome` executed/tool_error/denied_by_rule/rejected_by_user/approval_unavailable；只有参数摘要 |
| `PlanStep`（B2 已暴露） | `tenant_id,id,mission_id,key,title,detail,status,priority,depends_on,created_at,updated_at` | `GET /missions/{id}/plan`；`depends_on` 为 key 数组；`status` 见 §6.4 |
| `Node`（B2 已暴露） | `tenant_id,id,mission_id,kind,state,title,body,payload,created_at,updated_at` | `OperationNode`；`payload` 为对象；kind/state 见 §6.11 |
| `Edge`（B2 已暴露） | `tenant_id,id,mission_id,src_id,dst_id,rel,created_at` | `OperationEdge`；边两端同任务（DB 复合外键保证） |
| `Anchor`（B2 已暴露） | `tenant_id,node_id,asset_id,role,created_at` | `OperationAnchor`；role target/touched/tested/evidence |
| `MissionScopeEntry`（B2 已暴露） | `tenant_id,id,mission_id,kind,value,allowed_actions,note,created_at` | 任务范围；kind 六种；空 `allowed_actions` 返回 `[]` |
| `GraphView`（B2 已暴露） | `nodes:[OperationNode], edges:[OperationEdge], anchors:[OperationAnchor]` | `GET /missions/{id}/graph`；空数组不是 null |
| `Finding`（B2 已暴露） | `tenant_id,id,mission_id,asset_id,node_id,title,severity,status,detail,evidence_count,created_at,updated_at` | 详情额外带 `evidence:[]`；asset_id/node_id 可空；severity 见 §6.10 |
| `FindingEvidence`（B2 已暴露） | `tenant_id,finding_id,snapshot_id,created_at` | 发现 ↔ 证据快照绑定；正文在加密 blob |
| `Retest`（B2 已暴露） | `tenant_id,id,mission_id,finding_id,status,note,requested_by,created_at,started_at,finished_at` | 状态见 §6.10；started_at/finished_at 可空 |
| `MissionEvent`（SSE data/回放） | `tenant_id,id,mission_id,type,actor,reason,payload,created_at` | SSE 的 data 为 `events.Event`（id/mission_id/type/payload/created_at），不含 actor/reason |
| `Company` / `Asset`（B1 已暴露） | Company: `tenant_id,id,name,slug,attrs,created_at,updated_at`；Asset: `tenant_id,id,type,label,parent_id,attrs,identities,created_at,updated_at` | 列表 `?q=&limit=`（id 升序）；Asset 的 `identities` 为 `{kind,key}` 数组；创建带 identities 时 upsert；资产类型见 §4.4 |
| `CompanyScope` / `AssetIdentity`（B1 已暴露） | CompanyScope: `tenant_id,id,company_id,kind,value,source,created_at`；AssetIdentity: `tenant_id,asset_id,kind,key,created_at` | 企业范围 source manual/import；身份键只读列表，写入只能随创建资产 |
| `Coverage` / `TypeCoverage`（B1 已暴露） | Coverage: `total,tested,percent,by_type`；TypeCoverage: `type,total,tested,percent` | 覆盖度语义见 §4.3/§6.9；percent 为 0–100 浮点 |
| `TrafficExchange`（列表/详情） | 同上方字段；列表 `{traffic:[]}`（id DESC），详情平铺 | 正文不在 JSON；`GET .../request|response` 返回 `text/plain` |
| `AuditEvent`（命令历史） | 同上方字段；`GET /commands` 返回 `{commands:[]}`（id DESC），详情平铺 | outcome 见 §6.12；只读审计视图 |
| `LLMUsageRecord`（B3 已暴露） | `tenant_id,id,mission_id,role,model,prompt_tokens,completion_tokens,total_tokens,created_at` | `mission_id` 可空；列表 `{records:[]}`（id DESC） |
| `MissionUsage`（B3 已暴露） | `mission_id,max_tokens,spent_tokens,max_steps,steps_used,records,by_role:[{role,calls,total_tokens}],by_model:[{model,calls,total_tokens}]` | records 为该任务全部调用（不分页）；空 `[]` |
| `LLMUsageGroup` / `LLMUsageSummary`（B3 已暴露） | 单项 `key,calls,prompt_tokens,completion_tokens,total_tokens`；外层 `group_by,groups` | key 由 group_by 决定（model/role/YYYY-MM-DD）；day 用 DB 时区 |
| `DashboardStats`（B3 已暴露） | `missions{total,by_status}`,`tokens{spent_total,calls,today}`,`approvals{pending}`,`findings{total,by_severity}`,`assets{total}`,`traffic{total}`,`recent_missions:[{id,name,status,spent_tokens,created_at}]` | by_status 固定 7 键、by_severity 固定 5 键；口径见 §6.12 |
| `SystemInfo` | `service,version,commit,build_time,started_at,uptime,schema_version,latest_migration` | `/api/v1/system/info` |
| `Readiness` | `ready,checks[{name,ok,error?}]` | `/readyz` |
| `ReleaseInfo` | `version,tag,url,notes,published_at,asset_url,asset_name,checksum_url` | update/check 的 `release` |
| `UpdateCheckResult` / `UpdateApplyResult` / `RollbackResult` | check: `has_update,release`；apply: `updated,version,message`；rollback: `rolled_back,message` | 自更新接口 |

> 字段字典中标注「B1/B2/B3 已暴露」的类型可直接从 HTTP 读取；B3 已把 `LLMUsageRecord`/`MissionUsage`/`LLMUsageGroup`/`DashboardStats` 与流量/命令列表/详情暴露为 API。图写入、AI 自动 finding、复测执行器、流量导出/命令重放仍未实现（见 §10）。

### 8.5 任务可用操作矩阵

| 任务状态 | pause | resume | cancel | delete | 说明 |
| --- | --- | --- | --- | --- | --- |
| queued | ✗（409） | ✗ | ✓（→cancelled） | ✓ | 等待准入 |
| running | ✓（→paused） | ✗ | ✓（→cancelled） | ✓ | 运行中 |
| paused | ✗（409） | ✓（→queued） | ✓（→cancelled） | ✓ | 人为/预算暂停 |
| finalizing | ✗ | ✗ | ✓（→cancelled） | ✓ | 收尾中 |
| done / failed / cancelled | ✗（409） | ✗ | ✗（终态不可迁移） | ✓ | 终态仍可删除 |

- 按钮建议：仅在状态允许时启用，否则禁用并提示；删除始终二次确认。
- 暂停/恢复/取消成功返回 200 + 最新 Mission；状态不允许返回 409 `conflict`；不存在 404；id 非法 400。

**发现/复测操作矩阵（M6.2-B2b）**：

| 操作 | 允许的前置状态 | 失败 |
| --- | --- | --- |
| 发现状态流转 | 仅 §6.10 表中列出的边（open/confirmed/retest/fixed 的内部边） | 终态、同状态、未列出边 → 409；status 非法 → 400 |
| 发现编辑/绑定证据 | 任意状态（编辑不改状态；重复绑定 409；快照跨任务 → 400） | 发现/快照不存在 → 404/400 |
| 发起复测 | 任意发现（新复测初始 pending） | finding 不属于任务 → 400；发现不存在 → 404 |
| 推进复测 | pending→running；running→passed/failed/error | 终态/同状态/跳级 → 409；status 非法 → 400 |

### 8.6 展示约定

- **状态颜色建议**：queued 灰、running 蓝、paused 琥珀、finalizing 紫、done 绿、failed 红、cancelled 深灰；不要用颜色单独承载信息，配文字标签。
- **时间**：后端返回 RFC3339（多为 UTC），前端转本地时区显示；相对时间（“3 分钟前”）只用于辅助。
- **预算**：`spent_tokens/max_tokens`、`steps_used/max_steps` 用进度条；0 显示“不限”。
- **空状态**：列表空返回 `[]`（不是 null）；页面展示空状态，不用假数据。
- **错误**：展示后端 `error.message`，附 `code`/`request_id`；401 引导重新输入令牌。
- **分页**：当前接口只支持 `limit`（默认 50，超上限回退），没有 cursor/total；任务/审批/流量按 id 倒序，发现/复测按创建时间倒序，任务范围/计划按 id 升序；页面可提示“最多显示 N 条”，不要伪造总数。
- **长文本**：任务 goal/description、审计 detail、证据摘要需限高/折叠。

### 8.7 B1 资产/企业对接要点

- 企业列表：`GET /api/v1/companies?q=&limit=`；`q` 对 `name`/`slug` 做 ILIKE 模糊（`%`/`_` 已转义）；创建 `POST /api/v1/companies` body `{name,slug}`，slug 冲突 409。
- 资产列表：`GET /api/v1/assets?type=&q=&limit=`；`type` 必须是 6 种之一（非法 400），`q` 对 `label` 模糊；返回按 `id` 升序。
- 创建资产：`POST /api/v1/assets` 带 `identities` 时是 **upsert**——同一身份键命中已有资产会复用并更新 label/attrs，因此响应 `id` 可能不是新 id；前端必须以响应为准，并在“新建成功”文案上区分“已存在并更新”。
- `PATCH /api/v1/assets/{id}` 的 `parent_id`：**字段缺省**=不变，**显式 `null`**=清空父级，数字=设置；两种语义不能混淆。自身为父/父不存在 400。
- 任务资产：先 `POST /assets`（或复用已有资产）拿到 `asset_id`，再 `POST /missions/{id}/assets {asset_id,source}`；任务不存在 404、资产不存在 400、重复登记 409；`GET /missions/{id}/coverage` 空任务返回全零而非 404。
- 错误处理：企业/资产接口的 store 未就绪是 503 `store_unavailable`（与任务接口的 `runtime_unavailable` 不同）；路径存在但方法错配为 405 + `Allow`，路径不存在为 404 JSON。

### 8.8 B2 范围/计划/图/发现/复测/报告对接要点

- **任务范围**：列表 `GET /missions/{id}/scope` 按 id 升序；新增 body `{kind,value,note?,allowed_actions?}`，重复 409，201 返回带 id 的条目；删除 204。
- **计划/图**：`GET /missions/{id}/plan` 返回 `{steps:[]}`；`GET /missions/{id}/graph?kind=&state=&limit=` 返回 `{nodes,edges,anchors}`。图节点 limit 缺省 500/上限 2000，但 ListNodes 内部上限 1000——需要完整图时建议按 kind 分页拉取；kind/state 传非法值会 400（不要依赖后端忽略）。
- **发现**：列表支持 severity/status 过滤（非法值 400）；创建时 title 必填、severity 缺省 info；PATCH 的 asset_id 三态与资产 parent_id 一致；状态迁移只允许 §6.10 表内边，其它一律 409。
- **发现证据**：绑定 body `{snapshot_id}`；快照必须属于发现所在任务（否则 400），重复绑定 409；解绑未绑定 404。前端应把「绑定证据」做成从流量/证据列表选择快照，而不是手填 id。
- **复测**：创建 body `{finding_id,note?,requested_by?}`，finding 必须属于该任务；PATCH 只推进状态（pending→running→终态），note 非空才覆盖；终态 409。
- **报告**：`GET /missions/{id}/report` 返回 `text/markdown; charset=utf-8`，前端按 Markdown 渲染（或直接下载）；不是 JSON，不要 `response.json()`；报告只读、不落库。
- **一致性**：发现/复测的 400 是「参数/引用非法」，409 是「状态机拒绝或重复」，遇到 409 应刷新详情后提示，而不是自动重试。

### 8.9 B3 流量/命令/LLM/用量/Dashboard 对接要点

- **流量正文按需加载**：列表/详情只返回 hash/size；点击“查看请求/响应正文”再请求 `/traffic/{id}/request|response`，不要预取全量正文（明文可能敏感）。
- **limit 严格校验**：B3 的 traffic/commands/llm-records 对 `limit` 非数字或 ≤0 返回 400（不是回退默认）；traffic/commands 上限 200、llm-records 上限 500，超出会被钳制。
- **正文错误语义**：404 = 记录不存在或该阶段无正文（hash 为空）；409 tampered = blob 缺失/被篡改/密钥不匹配（不区分，避免探测）；503 = 证据存储未装配。前端应把 409 展示为“证据校验失败”，不要重试。
- **命令历史/LLM 记录**：都是只读视图，没有“取消/重放”操作；分页只支持 `limit`，需要更多历史时调大 limit（≤200/500）。
- **任务用量**：`records` 不分页，直接取该任务全部调用；页面数据量大时前端自行折叠/虚拟滚动。
- **用量聚合**：`group_by` 缺省 model；`days` 默认 30、上限 365、非法 400；day 分组键用 DB 时区，跨时区展示以返回 key 为准。
- **Dashboard**：建议 15s 轮询或进入首页时拉一次；`spent_total` 是任务累计消耗（不是 LLM 记录求和），`today` 是当天记录求和，两者口径不同，不要混用。

## 9. 原版 27 页映射与后端缺口

参考产品的页面清单来自 `research/` 下研究资料（gitignored，不随仓库发布）；下表的「原版页面」只是页面职责对照，不代表本仓库包含其代码。

**状况分级**：`A` 已有可用 API（可能部分字段/操作缺失，见缺口列）；`B` 数据/Store 已有，缺 HTTP API；`C` 完全未实现（无数据/无 API）；`D` 需新增领域能力（新表/新模块/新流程）。B1 已把企业/资产/任务资产/覆盖度从 B 提升为 A；B2 已把任务范围/计划/作战图/节点/发现/复测/报告提升为 A（基础版）。

| # | 原版页面 | 原版职责 | 状况 | 现有 API/数据 | 缺口 | 建议前端页面 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | login | 密码登录、须知确认 | D | 无用户体系，仅静态管理令牌 | 用户/会话/密码存储 | 暂不实现（用管理令牌） |
| 2 | setup | 首次初始化密码 | D | 无 | 初始化流程 | 暂不实现 |
| 3 | external redirect | 落地跳转 | A | 单页 `/` 已可用 | 无 | 不需要 |
| 4 | chat | 会话列表 + transcript + @提及 | D | 无会话/消息表 | 会话模型、LLM 对话 API、SSE | 后续 |
| 5 | dashboard | token 用量、趋势、活动 | A | B3：`GET /dashboard`（任务/用量/审批/发现/资产/流量/最近任务） | 趋势图按小时聚合、活动流（缺口） | 首页仪表盘（B3 基础版） |
| 6 | tasks | 任务列表/分类/归档/批量/创建 | A/B | 任务 CRUD + 列表 API | 分类/归档/批量（无表） | 任务列表（已实现基础版） |
| 7 | tasks/detail | 任务头 + 10 Tab 容器 | A | 任务详情/事件/审计/流量/证据；B1 任务资产/覆盖度；B2 范围/计划/图/发现/复测/报告 | 会话 transcript；策略只读/编辑 | 任务详情（B2 已实现基础版） |
| 8 | tasks/detail/sessions | transcript + 任务 SSE + worker 操作 | C | 仅 `mission_events` | Agent 会话/消息/侧问模型与 API | 后续（需新领域） |
| 9 | tasks/detail/overview | 目标/约束/范围/拦截/LLM 链 | A/B | B2：任务范围/计划 API；B1：资产/覆盖度；policy 数据已有 | 策略只读+编辑 API（B4） | 任务概览（B2 基础版） |
| 10 | tasks/detail/graph | 探索图可视化 | A | B2：`GET /missions/{id}/graph` + 节点详情/状态 | 图布局/大图分页（ListNodes 1000） | 作战图（B2 已实现） |
| 11 | tasks/detail/broadcast | 分页播报板 | B/C | 图节点数据可查 | 专门查询/语义 API | 可并入作战图 |
| 12 | tasks/detail/findings | 任务发现 | A | B2：任务内发现 CRUD/状态/证据绑定 | 跨任务发现列表（独立页） | 发现列表（B2 基础版） |
| 13 | tasks/detail/retests | 复测 | A | B2：复测 CRUD/状态推进 | 自动复测执行器（未实现） | 复测（B2 基础版） |
| 14 | tasks/detail/assets | 任务资产 | A | B1：`GET/POST /missions/{id}/assets`、`DELETE .../{asset_id}` | 无（权限/来源筛选可后续补） | 任务资产（B1 已实现） |
| 15 | tasks/detail/coverage-graph | 覆盖度图 | A | B1：覆盖度 API；B2：作战图 API | 图布局 | 覆盖度（B1/B2 已实现） |
| 16 | tasks/detail/intercept | 任务内审批 | A | `GET /approvals` + decide | 任务级过滤参数（可前端过滤） | 审批（已有） |
| 17 | tasks/detail/report | Markdown 报告 | A | B2：`GET /missions/{id}/report`（text/markdown） | PDF/Word 导出（未实现） | 报告（B2 已实现基础版） |
| 18 | findings | 发现三视图/筛选/导出 | A/B | B2：任务内发现列表/详情/编辑；无跨任务列表 | 跨任务发现列表/导出 API | 发现页（B2 基础版） |
| 19 | findings/detail | 发现详情/攻击链/流量 | A | B2：发现详情 + 证据绑定；图节点可查 | 攻击链聚合视图（前端组合） | 发现详情（B2 基础版） |
| 20 | traffic | 抓包列表/详情/删除/清理 | A | B3：全局流量列表/详情/正文/删除；M5 任务流量列表 + 冻结/校验 | 按 host 清理、导出、正文下载（缺口） | 流量页（B3 基础版） |
| 21 | commands | 工具执行历史 + 统计 | A | B3：`GET /commands`（audit_events 视图）+详情 | 按工具聚合统计、命令重放（缺口） | 工具历史（B3 基础版） |
| 22 | llm-records | LLM 原始记录 + token 聚合 | A | B3：`GET /llm-records`、`/missions/{id}/usage`、`/llm-usage/summary` | LLM 请求/响应正文记录、按小时趋势（缺口） | LLM 用量（B3 基础版） |
| 23 | assets | 企业范围 + 资产 CRUD + 搜索 | A | B1：企业/企业范围/资产 CRUD、`?q=`/`?type=` 搜索、身份键只读 | 身份键独立增删改、企业 attrs 更新、游标翻页 | 资产页（B1 已实现基础版） |
| 24 | sync | 外部数据源同步 | D | 无 | 数据源/项目/任务/同步 API | 后续（B5） |
| 25 | workspace | workDir 文件管理 | D | 无 | 文件系统 API + 沙箱 | 后续（B5） |
| 26 | system/llm | LLM profile CRUD/状态/重试 | D | 仅配置文件（启动时固定） | profile 表 + CRUD/测试 API | 后续（B5） |
| 27 | system/agents | Agent 定义与编辑器 | D | 无 Agent 表 | Agent 模型/CRUD/绑定工具 | 后续（B5） |
| 28 | system/agents/detail | 深链复用编辑器 | D | 无 | 同上 | 后续（B5） |
| 29 | system/mcp | MCP server CRUD/工具可见性 | D | 无 | MCP 客户端+表+API | 后续（B5） |
| 30 | system/skills | Skill 文件树/上传/缺失统计 | D | 无 | 技能存储+API | 后续（B5） |
| 31 | system/tools | 内置工具参数编辑/自定义工具 | C | 工具静态注册（代码内） | 工具配置表/自定义工具执行 | 后续（B4/B5） |
| 32 | system/notify | 通知渠道/投递/测试 | D | 无 | 通知表+发送器+API | 后续（B4） |
| 33 | system/intercept | 拦截规则/LLM judge/工具范围 | B | `policy_rules` + 内置规则 | 规则 CRUD/模拟/作用域 API | 策略页（B4） |
| 34 | system/intercept/assets | 全局资产黑名单 | D | 无 | 黑名单表/API | 后续（B4） |
| 35 | system/intercept/approvals | 审批记录壳页 | A | `GET /approvals` + decide | 历史筛选/分页已有 limit | 审批记录（已有） |
| 36 | system/logs | SSE 日志流 | D | 只有进程日志（slog 输出） | 日志缓冲/查询/SSE API | 后续（B4） |
| 37 | system/settings | 更新/GC/Web 搜索/环境检测 | A/D | `update/check|apply|rollback` API 已有 | GC、搜索自检、环境检测 | 设置页（先做更新） |

> 表中 #1–#7 与 #18–#37 对应原版 27 个顶层页面；#8–#17 是任务详情页的 10 个 Tab（对照用，不单独计页面）。

**B1 已实现（18 条，§5 #19–#36）**：企业 5 条、企业范围 3 条、资产 6 条、任务资产/覆盖度 4 条。**B2 已实现（20 条，§5 #37–#56）**：B2a 任务范围/计划/作战图/节点 7 条；B2b 发现/证据/复测/报告 13 条。**B3 已实现（11 条，§5 #60–#70）**：流量列表/详情/正文/删除 5 条、命令历史 2 条、LLM 记录/任务用量/用量聚合 3 条、Dashboard 1 条。前端可先做「资产/企业/任务资产/覆盖度」+「任务详情范围/计划/图/发现/复测/报告」+「流量/命令/LLM/用量/Dashboard」页面。

**仍未暴露的已有数据（B 类）**：跨任务发现列表/导出、策略规则（`ListPolicyRules` 只读，无 CRUD）、任务事件历史（除 SSE 外无分页查询 API）、图节点/边/锚点写入、身份键独立增删改。

**完全未实现/需新领域能力（C/D 类）**：用户与登录、Agent 会话/transcript/侧问、AI 自动写 finding、复测执行器、报告 PDF/Word 导出、任务分类/归档/批量、同步、Workspace、LLM profile 管理、Agent/MCP/Skills 管理、自定义工具、通知、全局资产黑名单、日志流、设置项、GC/搜索自检。

**B 方案前端优先级建议**：任务列表/详情/审批/流量/证据（现成）→ 企业/资产/任务资产/覆盖度（**B1 已完成**）→ 任务范围/计划/作战图/发现/复测/报告（**B2 已完成**）→ 流量/命令/LLM 记录/用量/Dashboard（**B3 已完成**）→ 策略/日志/通知/设置（B4）→ MCP/Skills/Agents/Workspace/Sync（B5）。

## 10. M6.2 后端补齐路线（B1/B2/B3 已实现；B4–B5 草案）

> B1（18 条）、B2（20 条）与 B3（11 条）已实现（见 §5 #19–#70）；B4–B5 仍是**未实现的 API 草案**，实际实现时以 OpenAPI 与本文档更新为准。统一约定：`/api/v1`、Bearer 鉴权、错误信封、`limit`（B1 默认 50/上限 200 且非法静默回退，B3 非法 400、超上限钳制）、写操作返回资源或 204；所有查询带租户。B1 列表按 `id` 升序，发现/复测按 `created_at DESC`，任务/审批/流量/命令/LLM 记录按 `id DESC`。

### B1：资产 / 企业 / 范围 / 覆盖率 / 任务资产（已实现 2026-10-21）

已实现 18 条（无新迁移，只补 handler 与 Store 方法）：

| 分类 | 已实现 API | 实现位置 |
| --- | --- | --- |
| 企业（5） | `GET/POST /api/v1/companies`；`GET/PATCH/DELETE /api/v1/companies/{id}` | `internal/httpapi/companies.go` |
| 企业范围（3） | `GET/POST /api/v1/companies/{id}/scope`；`DELETE /api/v1/companies/{id}/scope/{scope_id}` | `internal/httpapi/companies.go` |
| 资产（6） | `GET/POST /api/v1/assets`；`GET/PATCH/DELETE /api/v1/assets/{id}`；`GET /api/v1/assets/{id}/identities` | `internal/httpapi/assets.go` |
| 任务资产/覆盖度（4） | `GET/POST /api/v1/missions/{id}/assets`；`DELETE /api/v1/missions/{id}/assets/{asset_id}`；`GET /api/v1/missions/{id}/coverage` | `internal/httpapi/assets.go` |

- Store 扩展：`internal/infra/pg/store_asset_api.go`；数据表沿用 0002，无需迁移。
- 错误语义：400 `bad_request`、404 `not_found`、409 `conflict`、503 `store_unavailable`；路径存在但方法不支持 405 `method_not_allowed` + `Allow`。
- 测试：`internal/httpapi/assets_integration_test.go`（企业/资产/范围/任务资产/覆盖度集成）。

**B1 未包含（继续列入后续批次）**：

- 身份键独立新增/删除/修改（当前只读 + 随创建写入）；企业 `attrs` 更新（PATCH 仅 name/slug）。
- 资产列表游标翻页（当前仅 `limit`，无 `after_id`）。
- 验收（已完成）：18 条路由与 §5 一致、`make docs-check` 覆盖全部路由、企业/资产/覆盖度集成测试通过。

### B2：任务范围 / 计划 / 作战图 / 发现 / 复测 / 报告（已实现 2026-10-21，B2a+B2b）

已实现 20 条（新增迁移 0007 与 `domain/finding`）：

| 分类 | 已实现 API | 实现位置 |
| --- | --- | --- |
| 任务范围（3） | `GET/POST /api/v1/missions/{id}/scope`；`DELETE /api/v1/missions/{id}/scope/{scope_id}` | `internal/httpapi/graph_api.go`、`internal/infra/pg/store_scope_api.go` |
| 计划/图/节点（4） | `GET /missions/{id}/plan`；`GET /missions/{id}/graph`；`GET /nodes/{id}`；`PATCH /nodes/{id}/state` | `internal/httpapi/graph_api.go` |
| 发现（5） | `GET/POST /missions/{id}/findings`；`GET/PATCH /findings/{id}`；`POST /findings/{id}/status` | `internal/httpapi/findings_api.go`、`internal/infra/pg/store_finding.go` |
| 发现证据（3） | `GET/POST /findings/{id}/evidence`；`DELETE /findings/{id}/evidence/{snapshot_id}` | `internal/httpapi/findings_api.go` |
| 复测（4） | `GET/POST /missions/{id}/retests`；`GET/PATCH /retests/{id}` | `internal/httpapi/findings_api.go` |
| 报告（1） | `GET /missions/{id}/report`（text/markdown） | `internal/httpapi/report_api.go` |

- 迁移 0007：`findings` / `finding_evidence` / `retests` 三表（见 §4）；领域包 `internal/domain/finding`（Severity、发现/复测状态机）。
- 状态机：发现 6 状态、复测 5 状态，非法迁移 409（见 §6.10）；报告只读投影（见 §7.9）。
- 测试：`internal/httpapi/graph_api_test.go`、`findings_api_test.go`、`internal/domain/finding/*_test.go`、`internal/httpapi/assets_integration_test.go` 等。

**B2 未包含（后续批次/前端组合实现）**：

- AI 自动写 finding（当前只有人工 API）；复测执行器（自动复跑验证并回写 passed/failed）未实现。
- 图可视化数据分页：`ListNodes` 内部上限 1000，大图需要游标/分段 API。
- 任务范围编译为策略规则、报告导出 PDF/Word、跨任务发现列表/导出。
- 验收（已完成）：20 条路由与 §5 一致、`make docs-check` 覆盖全部路由、0007 迁移与发现/复测状态机测试通过。

### B3：流量 / 命令 / LLM 记录 / 用量 / Dashboard（已实现 2026-10-21）

已实现 11 条（无新迁移，新增 `insights_api.go`、`store_insights.go`）：

| 分类 | 已实现 API | 实现位置 |
| --- | --- | --- |
| 流量（5） | `GET /api/v1/traffic`；`GET/DELETE /api/v1/traffic/{id}`；`GET /api/v1/traffic/{id}/request`、`/response` | `internal/httpapi/insights_api.go`、`internal/infra/pg/store_insights.go` |
| 命令（2） | `GET /api/v1/commands`；`GET /api/v1/commands/{id}` | 同上 |
| LLM 记录/用量（3） | `GET /api/v1/llm-records`；`GET /api/v1/missions/{id}/usage`；`GET /api/v1/llm-usage/summary` | 同上 |
| Dashboard（1） | `GET /api/v1/dashboard` | 同上 |

- 正文读取：`/traffic/{id}/request|response` 用 `evidence.GetBlob` 解密，`text/plain; charset=utf-8`；blob 缺失/篡改/密钥不匹配统一 409 `tampered`。
- 口径：`spent_total=SUM(missions.spent_tokens)`、`calls=COUNT(llm_usage)`、`today=SUM(llm_usage.total_tokens)`（DB 时区当天）；`by_status` 固定 7 键、`by_severity` 固定 5 键。
- 测试：`internal/httpapi/insights_api_test.go`（列表/详情/正文/用量/Dashboard）。

**B3 未包含（后续批次/前端组合实现）**：

- 流量导出、按 host 清理、正文下载文件；命令重放；LLM 请求/响应正文记录（当前只有 token 用量）。
- 趋势按小时/自定义粒度、Dashboard 缓存/物化视图；`/missions/{id}/usage` 的 records 不分页。
- 验收（已完成）：11 条路由与 §5 一致、`make docs-check` 覆盖全部路由、insights 接口测试通过。

### B4：策略 CRUD / 日志 / 通知 / 设置

| 方法/路径 | 用途 | 依赖 |
| --- | --- | --- |
| `GET/POST /api/v1/policy/rules`；`GET/PATCH/DELETE /api/v1/policy/rules/{id}` | 规则 CRUD（immutable 不可删改动作） | `policy_rules` 表 + Store CRUD |
| `POST /api/v1/policy/simulate` | 用给定 tool/args 试算裁决 | `policy.Evaluate`（纯函数，易实现） |
| `GET /api/v1/logs?level=&limit=`；`GET /api/v1/logs/stream`（SSE） | 日志查询/实时流 | 需要环形缓冲/日志 sink（新增） |
| `GET/POST/PUT/DELETE /api/v1/notifications/channels`；`POST .../test`；`GET /api/v1/notifications/deliveries` | 通知渠道与投递记录 | 新表（channels/deliveries）+ 发送器 |
| `GET/PATCH /api/v1/settings` | 运行时可调设置（并发、预算默认等） | 新 `settings` 表或复用配置（需决定） |
| `POST /api/v1/approvals/{id}/cancel` | 取消 pending 审批 | 已有 `cancelled` 状态，补 Store 写入路径 |

### B5：MCP / Skills / Agents / Workspace / Sync

| 方法/路径 | 用途 | 依赖 |
| --- | --- | --- |
| `GET/POST/PUT/DELETE /api/v1/mcp/servers`；`POST .../refresh`；`GET /api/v1/mcp/tools` | MCP 端点与工具发现 | MCP 客户端 + 新表 |
| `GET/POST/PUT/DELETE /api/v1/skills`；`POST /api/v1/skills/upload`；`GET /api/v1/skills/{name}/files` | 技能包管理 | 状态目录文件存储 + 新表/索引 |
| `GET/POST/PUT/DELETE /api/v1/agents` | Agent 定义 CRUD（角色/提示词/工具集） | 新 `agents` 表 + 引擎装配改造 |
| `GET /api/v1/workspace/files`；`GET/PUT /api/v1/workspace/files/{path}`；`POST .../upload`；`DELETE .../{path}` | 任务工作目录文件管理 | 沙箱目录 + 路径穿越/大小限制 |
| `GET/POST /api/v1/sync/sources`；`GET/POST /api/v1/sync/projects`；`POST /api/v1/sync/run` | 外部数据源同步 | 数据源适配器 + 新表 |

依赖与风险：B5 需要新增较多表与安全边界（沙箱、路径校验、外部网络访问），建议在 B1–B4 稳定后再启动。

## 11. 配置与环境变量

### 11.1 加载与优先级

优先级：**环境变量 > 配置文件 > 内置默认值**（`internal/config/config.go` 的 `LoadFrom`）。配置文件路径由 `BEACONX_CONFIG` 或 `--config` 指定，默认 `config.json`；文件不存在时使用默认值（不报错）。环境变量值为空视为未设置。`Duration` 字段支持 Go duration 字符串（`"30s"`）或纳秒整数。启动时调用 `Validate()`，非法值直接退出。

### 11.2 配置段与默认值

| section | 字段（默认值） |
| --- | --- |
| `server` | `addr=":6969"`、`read_header_timeout=10s`、`idle_timeout=60s`、`request_timeout=30s`、`shutdown_timeout=15s`、`max_body_bytes=8388608` |
| `database` | `dsn=""`、`max_conns=10`、`min_conns=1`、`connect_timeout=8s` |
| `tenant` | `default_id=1` |
| `state` | `dir="data"` |
| `log` | `level="info"`、`format="json"` |
| `engine` | `max_concurrent=2`、`planner_interval=2s`、`worker_interval=200ms`、`lease_ttl=30s`、`heartbeat_interval=10s` |
| `auth` | `admin_token=""`（空则启动生成临时令牌并写日志） |
| `llm` | `base_url=""`、`api_key=""`、`model=""`、`max_tokens=2048`、`temperature=null`、`timeout=60s`、`max_retries=2`、`fallbacks=[]`（`[{base_url,api_key,model}]`） |
| `policy` | `default_action="ask"`、`approval_timeout=60s` |
| `evidence` | `dir=""`（默认 `<state.dir>/tenants/<tenant_id>/evidence`）、`key_hex=""`（默认 `<state.dir>/evidence.key`） |
| `selfupdate` | `enabled=true`、`repo="orzl1/BeaconX"` |

### 11.3 全部环境变量（29 个）

| 变量 | 对应字段 | 默认值 | 备注 |
| --- | --- | --- | --- |
| `BEACONX_CONFIG` | — | `config.json` | 配置文件路径 |
| `BEACONX_ADDR` | `server.addr` | `:6969` | 监听地址 |
| `BEACONX_REQUEST_TIMEOUT` | `server.request_timeout` | `30s` | 也约束 SSE 连接时长 |
| `BEACONX_SHUTDOWN_TIMEOUT` | `server.shutdown_timeout` | `15s` | 优雅关闭 |
| `BEACONX_DB_DSN` | `database.dsn` | 空（必填） | PostgreSQL DSN |
| `BEACONX_DB_MAX_CONNS` | `database.max_conns` | `10` | 连接池上限 |
| `BEACONX_DB_MIN_CONNS` | `database.min_conns` | `1` | 连接池保底 |
| `BEACONX_TENANT_DEFAULT_ID` | `tenant.default_id` | `1` | 单租户默认 ID |
| `BEACONX_HOME` | `state.dir` | `data` | 状态目录 |
| `BEACONX_LOG_LEVEL` | `log.level` | `info` | debug/info/warn/error |
| `BEACONX_LOG_FORMAT` | `log.format` | `json` | json/text |
| `BEACONX_MAX_CONCURRENT` | `engine.max_concurrent` | `2` | 并发任务上限 |
| `BEACONX_PLANNER_INTERVAL` | `engine.planner_interval` | `2s` | 规划 tick |
| `BEACONX_WORKER_INTERVAL` | `engine.worker_interval` | `200ms` | 执行 tick |
| `BEACONX_LEASE_TTL` | `engine.lease_ttl` | `30s` | 动作租约 |
| `BEACONX_HEARTBEAT_INTERVAL` | `engine.heartbeat_interval` | `10s` | 续约 |
| `BEACONX_ADMIN_TOKEN` | `auth.admin_token` | 空 | 空则生成临时令牌（仅当次进程） |
| `BEACONX_LLM_BASE_URL` | `llm.base_url` | 空 | OpenAI 兼容端点 |
| `BEACONX_LLM_API_KEY` | `llm.api_key` | 空 | 只放环境变量/本地 `.env` |
| `BEACONX_LLM_MODEL` | `llm.model` | 空 | 模型名 |
| `BEACONX_LLM_MAX_TOKENS` | `llm.max_tokens` | `2048` | 输出上限 |
| `BEACONX_LLM_TIMEOUT` | `llm.timeout` | `60s` | 单次请求超时 |
| `BEACONX_LLM_MAX_RETRIES` | `llm.max_retries` | `2` | 仅 429/5xx/网络错误 |
| `BEACONX_POLICY_DEFAULT_ACTION` | `policy.default_action` | `ask` | allow/ask/deny |
| `BEACONX_APPROVAL_TIMEOUT` | `policy.approval_timeout` | `60s` | ask 默认等待 |
| `BEACONX_EVIDENCE_DIR` | `evidence.dir` | 空 | 证据根目录 |
| `BEACONX_EVIDENCE_KEY` | `evidence.key_hex` | 空 | 64 位 hex 主密钥 |
| `BEACONX_UPDATE_ENABLED` | `selfupdate.enabled` | `true` | 布尔；非法值启动失败 |
| `BEACONX_UPDATE_REPO` | `selfupdate.repo` | `orzl1/BeaconX` | `owner/repo` |

### 11.4 校验规则（`Config.Validate`）

- `server.addr` 非空；`tenant.default_id>0`；`state.dir` 非空；`log.level ∈ {debug,info,warn,error}`；`log.format ∈ {json,text}`。
- `database.max_conns>=1`、`0<=min_conns<=max_conns`；`server.request_timeout/shutdown_timeout/max_body_bytes>0`。
- `engine.max_concurrent>=1`；四个 engine duration 均 >0。
- `policy.default_action ∈ {allow,ask,deny}`；`policy.approval_timeout>0`。
- `selfupdate.enabled=true` 时 `repo` 非空。
- `evidence.key_hex` 为空或 64 位十六进制（32 字节）。
- `llm.base_url` 与 `llm.model` 必须同时设置或同时为空；启用时 `max_tokens>0`、`timeout>0`、`max_retries>=0`；每个 fallback 必须同时有 `base_url` 与 `model`。

### 11.5 密钥与目录约定

- 管理令牌：`BEACONX_ADMIN_TOKEN`（空则生成 24 字节随机数并打印 WARN，仅当次进程）。
- LLM 密钥：只放环境变量/本地 `.env`；日志与错误体脱敏；fallback 密钥同理。
- 证据密钥：`BEACONX_EVIDENCE_KEY` 或 `<state.dir>/evidence.key`（0600 自动生成）；不打印；不支持轮换（M5.1）。
- `.env` 已 gitignore；`docker compose` 只透传 compose 文件里引用的变量（当前未透传 update/证据/LLM/policy 等，容器部署需自行补 environment 或挂载 config.json）。
- 状态目录布局：`$BEACONX_HOME/{jwt.key?} tenants/<id>/{sessions,evidence,traffic} skills/ cache/ selfupdate/`（部分目录尚未使用）。

## 12. 测试 / CI / 发布

### 12.1 测试分层（20 个测试包）

| 层 | 包 | 内容 | 需要 PostgreSQL |
| --- | --- | --- | --- |
| 领域（纯函数） | `domain/asset`、`domain/graph`、`domain/task`、`domain/plan`、`domain/policy`、`domain/finding` | 归一化、枚举/校验、状态机穷举、依赖/环、策略匹配、发现/复测迁移矩阵 | 否 |
| 配置/平台 | `config`、`platform`、`tenant`（无测试）、`logging`（无测试） | 默认值/环境变量/校验、目录权限 | 否 |
| HTTP | `httpapi` | 路由/鉴权/健康、任务 API 集成、审批、证据、SSE、B1 企业/资产集成、B2 范围/计划/图/发现/复测/报告、B3 流量/命令/LLM/用量/Dashboard、docs-check | 部分 |
| 存储 | `infra/pg` | 资产 upsert/隔离/覆盖度、性能（`BEACONX_PERF=1`） | 是 |
| AI/执行 | `app/ai`、`app/executor`、`app/tools`、`app/evidence` | 假 LLM 端到端、工具循环、策略/审批/证据采集、加密/签名/篡改 | 部分是 |
| 运行时 | `app/runner` | 完成/暂停恢复/恢复对账/删除/并发上限/预算暂停 | 是 |
| LLM 客户端 | `infra/llm` | 请求/响应、重试、JSON 回退、路由/错误分类、工具协议 | 否 |
| 迁移 | `infra/migrate` | 文件解析/校验和/排序 | 否 |
| 交付 | `infra/selfupdate`、`webui` | 检查/换装/回滚/校验和/冒烟、静态回退 | 否 |

### 12.2 运行方式

```bash
# 单元 + 集成（推荐）
BEACONX_TEST_PG_DSN='postgres://beaconx:beaconx@127.0.0.1:55432/beaconx?sslmode=disable' go test ./...
# 竞态
go test -race ./...
# 性能（10 万资产 P95）
BEACONX_TEST_PG_DSN=... BEACONX_PERF=1 go test -run TestAssetQueryPerf100k ./internal/infra/pg
# 文档路由覆盖
make docs-check
```

### 12.3 CI（`.github/workflows/ci.yml`）

| job | 内容 |
| --- | --- |
| `lint` | gofmt、go vet、`scripts/check-brand.sh`、`make docs-check` |
| `test` | `go test -race -coverprofile`（无 DB 时集成测试跳过） |
| `integration` | `postgres:16-alpine` service + `go test -count=1 ./...` |
| `web` | node 22 + `npm ci` + `npm run typecheck` + `NEXT_EXPORT=1 npm run build` + 校验 `out/index.html` |
| `build` | 5 平台交叉编译（linux/darwin amd64+arm64、windows amd64）+ Windows vet |
| `docker` | buildx 构建镜像（不推送） |

### 12.4 发布流水线（`.github/workflows/release.yml`）

1. tag `v*`（或手动 dispatch）触发。
2. `web` job：node 22 + `npm ci` + `NEXT_EXPORT=1 npm run build` → 上传 `web-dist`。
3. `build` job（5 平台）：下载 web-dist 到 `internal/webui/dist` → 交叉编译（注入 Version/Commit/BuildTime）→ 打包 `beaconx-<goos>-<goarch>.zip`。
4. `release` job：汇总 zip → `sha256sum *.zip > SHA256SUMS` → `gh release create <tag> ... --verify-tag`。

**已知问题**：非 Windows 包内文件名是 `beaconx-<goos>-<goarch>`，而 selfupdate 只认 `beaconx`/`beaconx.exe`，导致非 Windows 自更新在解压阶段失败；另外发布包只有 SHA256 无签名，Windows 自更新未验证。

### 12.5 镜像与自更新

- `Dockerfile` 三阶段：`node:22-alpine` 构建前端 → `golang:1.24-alpine` 嵌入前端并构建 → `alpine:3.21` 运行（非 root uid 10001、6969、`/readyz` HEALTHCHECK）。单镜像同时提供 API 与前端。
- 自更新 API 与流程见 §5.3/§7.8；容器内建议 `BEACONX_UPDATE_ENABLED=false`。
- 前端构建：`make web`（npm ci + static build + 复制）；CI 含独立 `web` job（typecheck + 静态构建 + 产物校验）；未构建时显示占位页。

### 12.6 已知缺口汇总

| 缺口 | 影响 | 关联章节 |
| --- | --- | --- |
| 无浏览器级 e2e、无 URL 路由、无 CORS | 前端交付/多源部署 | §8、M6.1 |
| 发布包文件名与 selfupdate 解压不匹配；无签名；Windows 自更新未验证 | 自更新可用性/供应链 | §7.8、§12.4 |
| `beaconx help` 未列新变量；compose 未透传新变量 | 部署一致性 | §11.5 |
| 无规则 CRUD、跨任务发现列表、流量导出/命令重放、LLM 请求/响应正文记录、趋势按小时 | 前端剩余页面（B4/B5）不可做 | §9、§10 |
| AI 自动写 finding、复测执行器、图大集合分页、报告导出 PDF/Word 未实现 | B2 只到“人工 API 基础版” | §7.9、§10 |
| 纯文本输出未脱敏、MITM/真实流量未实现、证据密钥不支持轮换 | 证据/安全边界 | §6.8、M5.1 |
| LLM 规划诊断事件（planner_*）前端未登记 | 前端看不到诊断事件 | §8.2 |

## 13. 一致性检查清单

### 13.1 改动类型 → 必须同步的文件

| 改动 | 必须同步 |
| --- | --- |
| HTTP API（新增/修改/删除） | 本文档 §5/§8 + `api/openapi.yaml` + README 接口表 |
| 数据表/迁移 | 本文档 §4/§6 + `docs/data-model.md` |
| 状态机/事件 | 本文档 §6/§7/§8 操作矩阵 + 前端 `STREAM_EVENT_TYPES` |
| 发现/复测领域 | 本文档 §4/§6/§7/§8/§10 + `docs/data-model.md` + `internal/domain/finding` 测试 |
| 洞察/聚合口径 | 本文档 §5/§6.12/§7.10/§8.9 + OpenAPI schemas + `insights_api_test.go` |
| 配置项 | 本文档 §11 + `docs/configuration.md` + `config.example.json` + `.env.example` |
| 运行时机制 | 本文档 §7 + `docs/runtime.md`/`docs/ai.md`/`docs/release.md` |
| 前端字段/接口 | 本文档 §8 + `docs/web.md` |
| 策略/证据 | 本文档 §6/§7 + `docs/tools-policy.md`/`docs/evidence.md` |
| 发布/镜像 | 本文档 §12 + `docs/release.md` |
| 纯重构 | 仅本文档 §14 变更记录（注明无对外行为变化） |

### 13.2 提交前检查

```bash
gofmt -l $(go list -f '{{.Dir}}' ./...)
go vet ./...
make lint
make docs-check
BEACONX_TEST_PG_DSN='postgres://beaconx:beaconx@127.0.0.1:55432/beaconx?sslmode=disable' go test ./...
```

- 路由有新增/修改？`Routes()`、handler 绑定、OpenAPI、README、本文档 §5 是否一致。
- 迁移只追加？校验和、data-model、本文档 §4 是否同步。
- 新配置项？默认值、校验、env、三份示例、本文档 §11 是否同步。
- 新事件类型？后端写入、SSE 帧、前端登记、本文档 §8 是否同步。
- 新工具/规则？executor 唯一入口、seed、策略文档、本文档 §6/§7 是否同步。

## 14. 变更记录

| 日期 | commit | 变更点 | 影响章节 |
| --- | --- | --- | --- |
| 2026-10-21 | `bfb2540` | 创建文档，记录 M0–M6 现状（初始 21 条路由；后端 + 前端 + 交付） | 全部 |
| 2026-10-21 | 提交 38aaeb4（M6.2-B1） | 实现 M6.2-B1：企业/资产/范围/覆盖率/任务资产 API（18 条，总路由 39 条） | §1、§3、§5、§6、§8、§9、§10、§14 |
| 2026-10-21 | 本次提交 | 实现 M6.2-B2：B2a 任务范围/计划/作战图/节点 7 条 + B2b 发现/证据/复测/报告 13 条（共 20 条，总路由 39→59）；新增迁移 0007 三表与 `internal/domain/finding` 状态机 | §1、§3、§4、§5、§6、§7、§8、§9、§10、§12、§13、§14 |
| 2026-10-21 | 本次提交 | 实现 M6.2-B3：流量列表/详情/正文/删除 5 条 + 命令历史 2 条 + LLM 记录/任务用量/用量聚合 3 条 + Dashboard 1 条（共 11 条，总路由 59→70）；无新迁移；`spent_total`/`today` 口径与 DB 时区说明 | §1、§3、§5、§6、§7、§8、§9、§10、§12、§13、§14 |

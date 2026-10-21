# 路线图

每个里程碑都以「可验收」为准，不以「代码写完」为准。M0–M6 已完成（M6 于 2026-10-21 收官），后续增强项列入 [M4.1](#m41-延期项)、[M5.1](#m51-延期项) 与 [M6.1](#m61-延期项) 延期项，长期方向见文末「后续方向（v2）」。

| 阶段 | 目标 | 验收标准 |
| --- | --- | --- |
| **M0 骨架**（已完成） | 可运行进程、声明式 HTTP + 中间件、版本化迁移、租户预留、健康检查、CI（含 Windows 交叉编译）、品牌门禁 | `docker compose up` 后 5 分钟内 `/readyz` 返回 200；CI 全绿；三平台可编译 |
| **M1 数据与图**（已完成 2026-10-21） | 租户、资产与身份、任务范围、资产图 + 作战图、锚点与覆盖度、迁移 v1。实际交付：迁移 0002（资产图 + 作战图核心表）；`internal/domain/asset`（资产身份归一化）与 `internal/domain/graph`（节点/状态/边/锚点枚举及校验）；`internal/infra/pg` Store（资产 upsert、范围解析、节点/边/锚点、覆盖度）；集成测试（双租户隔离、覆盖度语义、10 万资产性能） | 双租户夹具不串数据；覆盖度语义有单测；10 万资产查询 P95 < 200ms（实测列表 ≈ 4.1ms、覆盖度 ≈ 83ms） |
| **M2 运行时与引擎**（已完成 2026-10-21） | 任务状态 reducer + 事件、准入调度、规划/执行循环、动作租约、SSE 回放。实际交付：迁移 0003（任务时间字段、动作租约、mission_events）；`internal/domain/task`（状态机 + Reduce）；`internal/app/runner`（准入调度、每任务规划/执行循环、暂停/恢复/取消/删除、崩溃恢复、租约续约）；`internal/app/events` + SSE 处理器（先订阅后回放、Last-Event-ID、心跳、慢消费者丢弃）；静态令牌鉴权（Bearer 或 `?token=`） | kill -9 后无永久 running；暂停/恢复/删除可用；断线补洞无丢失（实测：10 个测试包全绿；引擎集成测试 5 项通过；HTTP 冒烟创建 201、约 1 秒内 done、4 条事件可回放、重复暂停 409、删除 204→404、无令牌 401） |
| **M3 规划与执行**（已完成 2026-10-21） | 目标分解、态势快照、结构化规划决策、计划依赖图、过程检索、上下文压缩、预算与收尾。实际交付：迁移 0004（plan_steps / step_dependencies / llm_usage / 任务预算列）；`internal/infra/llm`（OpenAI 兼容客户端：超时、重试、JSON 模式回退、错误体脱敏）；`internal/domain/plan`（Decision / Step / 依赖校验 / 就绪判定）；`internal/app/ai`（LLM Planner 与 Worker、追问、完成时 skipped、用量记录）；预算与超限事件 | 同一组靶场任务的完成率达标；规划输出 100% 通过 schema 校验；依赖派发用例通过（实测：`go test ./...` 13 个测试包全绿；假 LLM 端到端通过；DeepSeek `deepseek-flash` 真实冒烟约 50s 完成、3 动作 / 3 fact / 5 次调用约 3.8k tokens） |
| **M4 工具与策略**（已完成 2026-10-21） | 工具注册表（schema 生成）、MCP 连接池、技能版本化；策略规则 + 审批 + 模型兜底 + 审计；统一拦截入口与启动自检。实际交付：迁移 0005（policy_rules / approvals / audit_events）；`internal/app/tools`（Registry + `http_probe` / `shell_exec`，JSON Schema 与风险级别）；`internal/domain/policy`（Rule / Evaluate / SeedDefaults：五条内置规则、优先级匹配、fail closed）；`internal/app/executor`（取规则 → allow/ask/deny → 审批等待 → 执行 → 审计，工具调用唯一入口）；`internal/app/ai` Worker 工具循环（最多 6 轮）；审批与审计 HTTP 接口；MCP/技能/模型兜底/规则 CRUD 延期到 M4.1 | 工具面按角色收敛；审批/超时/审计用例通过；所有执行型 Agent 均受策略检查（实测：`go test ./...` 16 个测试包全绿；执行器集成测试 4 项通过；DeepSeek `deepseek-flash` 真实冒烟 14 次工具调用全部经策略判定与审批，GET 走 allow、shell 走 ask 并批准，目标不可达时如实记录 TCP 失败，审计可见 allow/ask 与 executed/tool_error） |
| **M5 证据与模型路由**（已完成 2026-10-21） | 采集两阶段、脱敏与加密、证据版本令牌、复测；单层模型路由 + 结构化错误 + 预算。实际交付：迁移 0006（traffic_exchanges / evidence_snapshots）；`internal/app/evidence`（AES-256-GCM 内容寻址 blob、HMAC-SHA256 快照签名、ErrTampered、RedactHeaders/RedactJSON）；`internal/app/executor` 两阶段采集（intent→result，工具调用前置捕获）；证据三接口（traffic 列表 / 冻结快照 / 校验）；`internal/infra/llm` 的 Router + ErrorKind/ClassifyError（auth/quota/rate_limit/transient 切换，context_overflow/fatal 立即失败）；引擎 token 预算超限暂停；MITM/复测/密钥轮换/纯文本脱敏列入 M5.1 | 篡改证据必失败；预算超限可暂停；模型故障切换有单一来源日志（实测：`go test ./...` 17 个测试包全绿；evidence 包 36 个测试含 13 类篡改场景；执行器采集+篡改集成测试、预算暂停测试、Router/错误分类测试通过；POST 快照→verify 正常，改数据库字段或改 blob 后 409 tampered） |
| **M6 前端与交付**（已完成 2026-10-21） | 由 OpenAPI 生成客户端、页面按功能域拆分、SSE/查询 hooks、图可视化、e2e；自更新与发布链（macOS/Linux）。实际交付：`web/`（Next.js 16 静态导出 + TypeScript + 原生 CSS 的单页控制台：任务/审批/系统三区、SSE、流量证据、自更新）；`internal/webui` 的 go:embed 同源托管、静态回退与占位页；`internal/infra/selfupdate`（检查/下载/SHA256SUMS 校验/冒烟/备份 `.old`/回滚/`update.json`）与 update 三接口；`release.yml` 前端构建 + 5 平台交叉编译 + SHA256SUMS + gh release；`Dockerfile` 多阶段内嵌前端；浏览器 e2e/URL 路由/CORS/发布签名/Windows 自更新列入 M6.1 | 发布包带校验和（release.yml 生成 SHA256SUMS）；自更新检查/校验/冒烟/回滚有单测（selfupdate 4 项 + update API 1 项）；`go test ./...` 19 个测试包全绿；前端构建产物与 Go 静态回退（`/`、`/missions/123`、`/_next/*`、`/api/v1/nope`）验证通过；**浏览器级 5 条 e2e 未实现（延后 M6.1）**，发布流水线未在 GitHub 实际执行（静态审阅） |

## M1 交付明细

- **迁移 0002**：资产图（companies / company_scope / assets / asset_identity）、任务与范围（missions / mission_scope / mission_asset_links）、作战图（operation_nodes / operation_edges / operation_anchors）及索引、`updated_at` 触发器。
- **领域模型**：`internal/domain/asset` 提供域名 / IP / CIDR / 服务 / 应用 / URL 的归一化身份；`internal/domain/graph` 提供节点类型、节点状态、边语义、锚点角色与合法性校验。
- **存储层**：`internal/infra/pg` 的 Store 实现资产 upsert（多身份合并与冲突检测）、任务范围解析与资产登记、节点 / 边 / 锚点写入、覆盖度计算；所有方法显式携带 `tenant_id`。
- **集成测试**：`BEACONX_TEST_PG_DSN` 指向真实 PostgreSQL 时执行，覆盖双租户隔离、资产 upsert 幂等与身份合并、范围解析、覆盖度语义、同任务连边约束、10 万资产性能（`BEACONX_PERF=1` 时运行）。
- **CI**：新增「集成测试」作业，以 `postgres:16-alpine` service 启动数据库并运行 `go test -count=1 ./...`。

## M2 交付明细

- **迁移 0003**：任务排队/时间字段与状态词表收敛（`draft→queued`、`active→running`）、动作租约列（`priority` / `lease_until` / `leased_by` / `attempts`）、只追加事件表 `mission_events` 及索引。
- **状态机**：`internal/domain/task` 定义 7 个状态、11 种事件与合法迁移表，`Reduce` 是纯函数并返回 `start_runners` / `stop_runners` 副作用；迁移由 `TransitionMission` 以 CAS + 同事务事件落库。
- **引擎**：`internal/app/runner` 实现准入调度（`MaxConcurrent` + FIFO）、每任务规划/执行循环、动作租约（领取 / 心跳续约 / 完成 / 释放 / 过期回收）、暂停/恢复/取消/删除、崩溃恢复（`ResetExpiredLeases` + `ResetRunningMissions`）。
- **事件与鉴权**：`internal/app/events` 提供非阻塞事件总线；`internal/httpapi` 的 SSE 处理器实现先订阅后回放、`Last-Event-ID` 优先 `?since=`、心跳与慢消费者丢弃；`TokenAuthenticator` 支持 Bearer 与 `?token=` 常量时间比较。
- **验收**：`go test ./...` 10 个包全绿；引擎集成测试 5 项通过（完成、暂停恢复取消、崩溃恢复、删除、并发上限）；HTTP 冒烟结果见 [运行时与引擎](runtime.md) 第 9 节。

## M3 交付明细

- **迁移 0004**：`plan_steps`（步骤与状态）、`step_dependencies`（依赖，含自依赖 CHECK）、`llm_usage`（逐次调用台账）；`missions` 增加 `max_tokens` / `spent_tokens` / `max_steps` / `steps_used` 预算列。
- **LLM 客户端**：`internal/infra/llm` 提供最小 OpenAI 兼容 Chat Completions 客户端：超时、指数退避重试（仅 429/5xx/网络错误）、JSON 模式（HTTP 400 时回退普通请求）、请求/响应大小限制、错误体密钥脱敏。
- **规划领域**：`internal/domain/plan` 定义 Decision / Step / Action、步骤状态、依赖校验与环检测、`ParseDecision` 容错解析、`ReadySteps` 就绪判定；规划输出必须在领域层通过 schema 校验。
- **AI 规划与执行**：`internal/app/ai` 的 Planner 基于态势快照产出结构化决策并幂等落库，只给依赖已满足的步骤创建动作（单次新增 ≤8 步、每轮 ≤3 动作），模型过早完成时追问一次、仍完成则把剩余步骤标 `skipped`；Worker 把观察写成 fact 节点。
- **预算与用量**：每次调用写 `llm_usage` 并累加 `spent_tokens`；token/步骤预算超限写 `budget_exhausted` / `step_budget_exhausted` 事件并按正常完成收尾。
- **验收**：`go test ./...` 13 个测试包全绿；假 LLM 端到端覆盖规划→执行→事实→完成；真实 DeepSeek 冒烟约 50 秒完成（3 动作、3 fact、5 次调用约 3.8k tokens），详见 [AI 规划与执行](ai.md)。

## M4 交付明细

- **迁移 0005**：`policy_rules`（规则，`UNIQUE (tenant_id, name)`）、`approvals`（审批状态机、`expires_at`、任务/节点外键）、`audit_events`（工具调用审计、`args_digest`）及索引。
- **工具层**：`internal/app/tools` 提供 Registry 与两个内置工具——`http_probe`（medium、worker、32KB 输出上限）与 `shell_exec`（high、worker、超时/输出上限）；工具定义带 JSON Schema 与风险级别，注册表按角色过滤可见性。
- **策略域**：`internal/domain/policy` 定义 Rule / Action / Risk / Evaluate / SeedDefaults；Evaluate 是纯函数（优先级倒序、稳定排序、tool 通配、参数原始 JSON 正则、无命中用默认动作），配置错误一律 fail closed；五条内置规则覆盖 GET/HEAD 放行、下载并执行与删根拒绝、shell 与非 GET/HEAD HTTP 审批。
- **执行与审批**：`internal/app/executor` 是工具调用唯一入口（取规则 → 判定 → ask 建审批并写 `approval_required` 事件 → 轮询等待 → 执行 → 写审计）；`internal/httpapi` 新增审批列表/决策与任务审计接口；`internal/app/ai` Worker 进入「模型 → 工具 → 模型」循环（最多 6 轮）。
- **验收**：`go test ./...` 16 个测试包全绿；执行器集成测试 4 项（allow / 批准后执行 / deny / 人工拒绝）通过；真实 DeepSeek 冒烟 14 次工具调用全部经策略判定与审批，GET 走 allow、shell 走 ask 并经 API 批准，目标不可达时模型如实记录 TCP 连接失败并产出事实，审计表可见 allow/ask 与 executed/tool_error。

## M5 交付明细

- **迁移 0006**：`traffic_exchanges`（两阶段 intent→result，请求/响应哈希与大小、状态码、错误、redaction、完成时间）与 `evidence_snapshots`（签名、正文引用、大小、redaction、创建时间）及索引。
- **证据存储**：`internal/app/evidence` 实现 AES-256-GCM 加密的内容寻址 blob（`nonce||ciphertext`、AAD 绑定明文 sha256、原子写入、已存在损坏不覆盖）、HMAC-SHA256 快照签名与校验、`ErrTampered`、`RedactHeaders` / `RedactJSON`；主密钥由 `BEACONX_EVIDENCE_KEY` 或状态目录 `evidence.key` 提供。
- **两阶段采集**：`internal/app/executor` 在策略门通过后、执行工具前登记 intent（参数脱敏加密落盘），执行后写 result（输出脱敏落盘、状态码、错误）；采集失败只记日志，不阻塞工具执行；崩溃会留下未完成的 intent 行。
- **证据接口**：`GET /api/v1/missions/{id}/traffic`、`POST /api/v1/traffic/{id}/evidence`、`GET /api/v1/evidence/{id}/verify`；篡改（数据库字段或 blob）返回 409 `tampered`，未完成的采集返回 409 `conflict`。
- **模型路由**：`internal/infra/llm` 新增 `Router` / `ErrorKind` / `ClassifyError` / `RouteError`；`llm.fallbacks` 按序故障转移，仅 auth/quota/rate_limit/transient 可切换，context_overflow/fatal 立即失败，每次切换写一条「模型切换」日志。
- **预算暂停**：引擎在每轮循环检查 token 预算，超限把任务置为 `paused`（事件 reason=「模型 token 预算超限暂停」），不硬失败，可用 `resume` 继续。
- **验收**：`go test ./...` 17 个测试包全绿；evidence 36 个测试（含 13 类篡改场景）、执行器采集+篡改集成测试、预算暂停测试、Router/错误分类测试通过；快照创建→verify 正常，改库/改文件后返回 409。

## M6 交付明细

- **前端**：`web/` 使用 Next.js 16 App Router + React + TypeScript + 原生 CSS，单页控制台含令牌栏、任务（列表/创建/暂停/恢复/取消/删除、5s 轮询）、任务详情（SSE 14 类事件、按 id 去重、200 条上限、断线重连、流量→冻结证据→校验）、审批（pending 列表与决策）、系统（探针与自更新）；`NEXT_EXPORT=1` 输出纯静态 `web/out`。
- **托管**：`internal/webui` 通过 `go:embed all:dist` 把前端嵌入二进制；静态文件直出、非 API 路径回退 `index.html`、`/api/` 未知路径返回 JSON 404；未构建前端时显示占位页。`make web` 完成 `npm ci`、静态构建与复制。
- **自更新**：`internal/infra/selfupdate` 实现检查 GitHub latest release → 下载平台 zip → SHA256SUMS 校验 → `--version` 冒烟 → 备份 `.old` → 原子替换 → 写 `update.json`；失败不改当前文件，`rollback` 用 `.old` 还原；三个 API 端点（check/apply/rollback）需要管理令牌。
- **发布与镜像**：`.github/workflows/release.yml` 在 `v*` tag 上先构建前端，再交叉编译 5 个平台、打包 zip、生成 `SHA256SUMS` 并创建 Release；`Dockerfile` 多阶段构建（Node 构建前端 → Go 嵌入 → alpine 运行），单镜像同时提供 API 与前端（6969）。
- **验收**：`go test ./...` 19 个测试包全绿（新增 selfupdate 与 webui 测试；update API 测试通过）；`make web` + `make build` 可产出内嵌前端二进制；Go 端 `/`、`/missions/123` 回退、`/_next/*` 200、`/api/v1/nope` JSON 404 验证通过。

## M4.1 延期项

以下 M4 增强项未在本里程碑实现，已明确延后：

- MCP 连接池与外部工具接入。
- 技能（skills）版本化与加载。
- 规则未命中时的模型兜底审批。
- 策略规则的 CRUD/API 与任务级/资产级作用域。
- 更完备的参数级策略（命令/URL/目录白名单、结构化参数匹配）与更强的沙箱/网络出口控制。

## M5.1 延期项

以下 M5 增强项未在本里程碑实现，已明确延后：

- MITM 代理与真实网络报文采集（当前采集的是工具参数与输出文本）。
- 漏洞复测（retester）与证据版本令牌的自动流转。
- 证据密钥轮换与多密钥并存。
- 纯文本输出脱敏，以及把 `RedactHeaders` 接入真实抓包链路。
- 证据保留策略/GC 与批量导出。

## M6.1 延期项

以下 M6 增强项未在本里程碑实现，已明确延后：

- 浏览器级 e2e 测试（当前没有浏览器自动化用例）。
- URL 级路由与深链接（单页刷新会丢失选中任务）。
- 跨源部署所需的 CORS 支持。
- 发布包签名（当前只有 SHA256 校验和，没有 GPG/cosign）。
- Windows 自更新的实际验证（运行中可执行文件替换、备份与回滚）。
- 非 Windows 发布包内文件名（`beaconx-<goos>-<goarch>`）与 selfupdate 解压匹配规则（只认 `beaconx`/`beaconx.exe`）不一致的问题修复。
- 常规 CI（ci.yml）补前端构建 job（目前只有发布流水线构建前端）。

## 后续方向（v2）

- **多租户**：从字段预留升级为行级隔离（RLS + 租户级认证），不需要数据搬迁。
- **Windows 正式支持**：服务化、安装器、证书存储集成与自更新验证。
- **MCP 与外部工具**：MCP 连接池、技能版本化与外部工具接入（M4.1）。
- **证据复测**：MITM 真实流量、retester、证据版本令牌与密钥轮换（M5.1）。
- **策略与审计增强**：规则 CRUD、模型兜底审批、审批推送与审计导出。
- **前端与交付完善**：浏览器 e2e、URL 路由、CORS、发布签名与自更新完善（M6.1）。

## 非目标（v1）

- 多租户隔离、多用户角色体系
- Windows 官方发布包
- 执行期授权策略引擎
- 扫描器编排（能力仍由 Agent 通过工具完成）

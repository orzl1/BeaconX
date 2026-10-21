# 路线图

每个里程碑都以「可验收」为准，不以「代码写完」为准。M0、M1、M2 与 M3 已完成（M3 于 2026-10-21 收官），下一阶段为 **M4**（未开始）。

| 阶段 | 目标 | 验收标准 |
| --- | --- | --- |
| **M0 骨架**（已完成） | 可运行进程、声明式 HTTP + 中间件、版本化迁移、租户预留、健康检查、CI（含 Windows 交叉编译）、品牌门禁 | `docker compose up` 后 5 分钟内 `/readyz` 返回 200；CI 全绿；三平台可编译 |
| **M1 数据与图**（已完成 2026-10-21） | 租户、资产与身份、任务范围、资产图 + 作战图、锚点与覆盖度、迁移 v1。实际交付：迁移 0002（资产图 + 作战图核心表）；`internal/domain/asset`（资产身份归一化）与 `internal/domain/graph`（节点/状态/边/锚点枚举及校验）；`internal/infra/pg` Store（资产 upsert、范围解析、节点/边/锚点、覆盖度）；集成测试（双租户隔离、覆盖度语义、10 万资产性能） | 双租户夹具不串数据；覆盖度语义有单测；10 万资产查询 P95 < 200ms（实测列表 ≈ 4.1ms、覆盖度 ≈ 83ms） |
| **M2 运行时与引擎**（已完成 2026-10-21） | 任务状态 reducer + 事件、准入调度、规划/执行循环、动作租约、SSE 回放。实际交付：迁移 0003（任务时间字段、动作租约、mission_events）；`internal/domain/task`（状态机 + Reduce）；`internal/app/runner`（准入调度、每任务规划/执行循环、暂停/恢复/取消/删除、崩溃恢复、租约续约）；`internal/app/events` + SSE 处理器（先订阅后回放、Last-Event-ID、心跳、慢消费者丢弃）；静态令牌鉴权（Bearer 或 `?token=`） | kill -9 后无永久 running；暂停/恢复/删除可用；断线补洞无丢失（实测：10 个测试包全绿；引擎集成测试 5 项通过；HTTP 冒烟创建 201、约 1 秒内 done、4 条事件可回放、重复暂停 409、删除 204→404、无令牌 401） |
| **M3 规划与执行**（已完成 2026-10-21） | 目标分解、态势快照、结构化规划决策、计划依赖图、过程检索、上下文压缩、预算与收尾。实际交付：迁移 0004（plan_steps / step_dependencies / llm_usage / 任务预算列）；`internal/infra/llm`（OpenAI 兼容客户端：超时、重试、JSON 模式回退、错误体脱敏）；`internal/domain/plan`（Decision / Step / 依赖校验 / 就绪判定）；`internal/app/ai`（LLM Planner 与 Worker、追问、完成时 skipped、用量记录）；预算与超限事件 | 同一组靶场任务的完成率达标；规划输出 100% 通过 schema 校验；依赖派发用例通过（实测：`go test ./...` 13 个测试包全绿；假 LLM 端到端通过；DeepSeek `deepseek-flash` 真实冒烟约 50s 完成、3 动作 / 3 fact / 5 次调用约 3.8k tokens） |
| **M4 工具与策略**（下一阶段，未开始） | 工具注册表（schema 生成）、MCP 连接池、技能版本化；策略规则 + 审批 + 模型兜底 + 审计；统一拦截入口与启动自检 | 工具面按角色收敛；审批/超时/审计用例通过；所有执行型 Agent 均受策略检查 |
| **M5 证据与模型路由** | 采集两阶段、脱敏与加密、证据版本令牌、复测；单层模型路由 + 结构化错误 + 预算 | 篡改证据必失败；预算超限可暂停；模型故障切换有单一来源日志 |
| **M6 前端与交付** | 由 OpenAPI 生成客户端、页面按功能域拆分、SSE/查询 hooks、图可视化、e2e；自更新与发布链（macOS/Linux） | 5 条 e2e 全绿；发布包带校验和；升级回滚演练通过 |

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

## 后续方向

- **v1.1**：审批推送（IM/Webhook）、审计导出、审批工作台增强。
- **v2**：执行期授权策略（动作 × 目标编译）、fail-closed 默认、不可删除的安全底线、策略快照 API。
- **多租户**：从字段预留升级为行级隔离（RLS + 租户级认证），不需要数据搬迁。
- **Windows 正式支持**：服务化、安装器、证书存储集成；v1 仅保证可编译。

## 非目标（v1）

- 多租户隔离、多用户角色体系
- Windows 官方发布包
- 执行期授权策略引擎
- 扫描器编排（能力仍由 Agent 通过工具完成）

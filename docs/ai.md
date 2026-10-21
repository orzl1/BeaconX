# AI 规划与执行（M3）

本文描述 BeaconX M3：用 OpenAI 兼容的 LLM 实现 M2 定义的两个运行时接口——规划器（Planner）与执行器（Worker），以及计划模型、预算与用量、追问语义。

权威来源是 `internal/app/ai`、`internal/domain/plan`、`internal/infra/llm`、`internal/infra/pg/plan_store.go`、`migrations/0004_planning.sql` 与 `cmd/beaconx/main.go`；本文只做解释，若与代码不一致，以代码为准并回来修正本文。运行时调度与状态机见 [运行时与引擎](runtime.md)。

## 1. 能力边界

M3 **没有接入任何真实工具**（M4 才接入）。因此 Worker 的“观察”不是工具输出，而是模型基于任务目标、动作要求与上下文给出的**规划性结论**：

- 提示词明确要求：不编造工具输出；缺少执行条件时在 detail 中写明“未实际执行”及原因；
- 观察会写入 `operation_nodes` 的 fact 节点，供后续规划轮次引用；它只是可复核的文字结论，不是扫描结果或证据；
- M4 接入工具后，只需替换 `ai.Worker.Execute` 的实现（调用工具、记录证据），计划模型、引擎、预算与 SSE 均不变。
- Planner 同样不执行动作：它只输出结构化决策（新步骤 + 待执行动作 + 是否完成）。

## 2. 启用方式与模式

`main` 根据 LLM 配置决定运行时实现：

- **llm 模式**：`llm.base_url` 与 `llm.model` **同时非空**时启用。启动日志为 `规划模式 mode=llm model=<模型名>`，规划器/执行器分别由 `ai.NewPlanner` / `ai.NewWorker` 提供。
- **idle 模式**：任一为空时使用 M2 的空实现 `IdlePlanner` / `NoopWorker`，日志为 `规划模式 mode=idle model=`；任务在无待办动作时一个规划周期内走完生命周期。
- 启用时会用 `llm.NewClient` 校验配置：`base_url` 必须是可解析的 http/https 地址且有主机名，`model` 不能为空；API Key 允许为空（本地假服务或免鉴权网关）。校验失败会打印“初始化模型客户端失败”并终止启动。
- 只配置其中一项（例如只有 base_url）会在启动配置校验阶段报错——两项必须同时设置。

## 3. 配置项

| 配置项 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `llm.base_url` | `BEACONX_LLM_BASE_URL` | 空 | OpenAI 兼容端点根地址，例如 `https://api.deepseek.com` 或 `https://api.openai.com/v1`；客户端会拼接 `/chat/completions` |
| `llm.api_key` | `BEACONX_LLM_API_KEY` | 空 | 以 `Authorization: Bearer <key>` 发送；为空时不发送该头 |
| `llm.model` | `BEACONX_LLM_MODEL` | 空 | 模型名，例如 `deepseek-flash` |
| `llm.max_tokens` | `BEACONX_LLM_MAX_TOKENS` | `2048` | 单次回复的输出上限；`<=0` 时使用 2048 |
| `llm.temperature` | —（仅配置文件） | 未设置 | 可选；未设置时不发送，使用服务端默认值 |
| `llm.timeout` | `BEACONX_LLM_TIMEOUT` | `60s` | 单次 HTTP 请求超时；`<=0` 时使用 60s |
| `llm.max_retries` | `BEACONX_LLM_MAX_RETRIES` | `2` | 最多重试次数（不含首次）；`<=0` 时使用 2。仅 429、5xx 与网络错误会重试，因此无法用 0 关闭重试 |

环境变量值为空时视为未设置；其余配置规则见 [配置说明](configuration.md)。

请求失败时的处理：只有 HTTP 429、5xx 与网络错误会按 `max_retries` 做指数退避重试（100ms 起翻倍）；`CompleteJSON` 在服务端以 HTTP 400 拒绝 JSON 模式时，会自动回退为普通请求再试一次；其余 4xx 不重试。调用方取消或超时会立即终止重试。

## 4. 规划流程

规划器每个任务按 `engine.planner_interval`（默认 2s）被调用一次，单轮流程如下：

1. **预算检查**：读取任务预算；若 `max_tokens > 0` 且 `spent_tokens >= max_tokens`，写入事件 `budget_exhausted`（原因“模型 token 预算已用尽，结束任务”），返回完成，不再调用模型。
2. **加载态势**：读取最多 500 个节点与全部计划步骤；把动作结果回写到步骤：动作 `done → 步骤 done`，动作 `blocked` / `exhausted → 步骤 failed`。
3. **跳过空转**：若仍有 `open` / `running` 的动作，本轮直接跳过规划（等动作结束再规划），避免每轮重复消耗 token。
4. **步骤预算检查**：若 `max_steps > 0` 且 `steps_used >= max_steps`，写入事件 `step_budget_exhausted`（原因“步骤预算已用尽，结束任务”），返回完成。
5. **态势快照**：渲染任务名、目标、当前步骤（key / 标题 / 状态 / 依赖 / 详情）、最近 10 条 fact（正文截断到 300 个字符）与进行中动作数，作为用户消息。
6. **模型决策**：首次（还没有步骤）用“目标拆解”提示词，之后用“下一步规划”提示词；调用 `CompleteJSON` 要求服务端返回 JSON 对象，记录用量（role=`planner`）。
7. **解析与校验**：`plan.ParseDecision` 从输出中提取 JSON（容忍 Markdown 围栏与前后杂质、自动去重依赖），再执行结构校验（上限、key 格式、依赖与动作引用、环）；解析失败会带着错误信息重试一次（role=`planner-retry`）。
8. **落地决策**：单次最多写入 8 个新增步骤（`ai.Config.MaxStepsPerPlan`），`UpsertPlanSteps` 幂等写入（已存在步骤保留首次的标题与详情）；`steps_used` 累加实际新增数；用 `ReadyPlanSteps`（pending 且依赖全部 done）筛选可执行步骤，只给这些步骤创建动作，每轮最多 3 个（`MaxActionsPerRound`）。动作节点为 `kind=action`、`state=open`，payload 记录 `step_key` / `kind` / `target` / `detail`。
9. **完成判断**：若模型返回 `done=true` 但计划里仍有未完成步骤，则追问一次（role=`planner-followup`）；追问仍返回完成才接受，并把剩余非终态步骤标为 `skipped`，避免计划里留下永远 pending 的悬空步骤。只有引擎确认没有待办动作后，任务才会进入收尾并完成。

规划失败不会让任务失败：解析/落库错误会写事件（`planner_error` / `planner_persist_error`）并等下一轮重试。模型过早完成会写事件 `planner_done_with_pending`，便于审计。

### 提示词要点

- 规划器系统提示词要求“只输出一个 JSON 对象”，不使用 Markdown 围栏；首轮拆解要求 3–8 个可验证步骤，后续轮次要求动作只引用已有且依赖已满足的步骤，每轮最多 3 个动作，无法推进时返回 done 并说明原因。
- 执行器系统提示词要求输出 `{"fact":"…","detail":"…","done":true}`，并明确“不编造工具输出；缺少执行条件时写明未实际执行及原因”。
- 上下文只带当前步骤与最近 10 条 fact（正文截断到 300 字符），不塞历史动作全文：既控制 token，也让模型围绕最新事实决策。
- 解析容错但校验严格：`ParseDecision` 容忍围栏与前后杂质，结构错误会以中文错误回灌模型并重试一次；重试仍失败则本轮放弃、等下一轮。

### 一轮交互示意

```text
第 1 轮规划：拆解目标 → 写入步骤 recon，创建 1 个 probe 动作
执行：Worker 产出 fact（观察，非工具输出）→ 步骤 recon = done
第 2 轮规划：读取最新 fact → 生成下一步动作或 done=true
完成：引擎确认无待办动作 → finalizing → done
```

### 可观测性

- 规划/执行相关事件写入 `mission_events`，可通过 SSE（`GET /api/v1/missions/{id}/events`）回放：`planner_error`、`planner_persist_error`、`planner_done_with_pending`、`budget_exhausted`、`step_budget_exhausted`；状态迁移事件仍为 `admitted` / `finalizing` / `completed` 等。
- 日志组件名为 `ai-planner` / `ai-worker`，记录模型调用失败、解析失败与落库失败；密钥与错误体已脱敏。

## 5. 执行与结果回写

- 执行器收到动作节点后，用动作的 `step_key` / `kind` / `target` / `detail` 与任务目标组装提示词，要求模型输出 `{"fact":"一句话结论","detail":"依据、推理与限制条件","done":true}`。
- 成功时把观察写入 fact 节点（标题截断到 120 字符，正文为 detail，payload 记录 `action_id` / `step_key` / `kind` / `target`），并把动作置为 `done`。
- 模型输出不是合法 JSON 时，退化为把原始文本整体存为观察（标题取截断后的一行），保证信息不丢。
- 模型调用失败时动作置为 `blocked` 并返回错误（引擎记录告警），下一轮规划会把对应步骤标为 `failed`。
- 动作的终态只允许 `done` / `exhausted` / `blocked`（由存储层 `FinishIntent` 约束）。

### 工具调用循环（M4）

启用工具后（`Worker.SetTools` 装配了执行器与工具定义），Worker 不再直接要求模型输出最终观察，而是进入「模型 → 工具 → 模型」循环：

1. 用工具系统提示词调用 `CompleteWithTools`（`tool_choice=auto`），最多 6 轮；
2. 模型返回 `tool_calls` 时，逐个经 `executor.ExecuteText` 调用工具（每次调用都会过策略门与审批），把结果以 `role=tool` 消息回灌；工具错误也作为结果回灌；
3. 模型不再调用工具时，其输出即最终文本；轮次用尽仍无结论时写入“工具调用轮次用尽，未给出最终结论”；
4. 最终文本仍按 `{"fact":"…","detail":"…","done":true}` 解析为观察并写成 fact 节点；解析失败退化为原文。

工具调用与审批的规则、审计与已知盲区见 [工具与策略](tools-policy.md)。未装配工具（M3 行为或 idle 模式）时退化为纯文本观察。

## 6. 计划模型（domain/plan）

- `Decision`：`summary`、`steps`、`actions`、`done`、`reason`。
- `Step`：`key`（任务内唯一，仅小写字母 / 数字 / `_` / `-`，长度 1..64）、`title`、`detail`、`depends_on`。
- `Action`：`step_key`、`kind`（`probe` / `verify` / `report` / `other`）、`target`、`detail`。
- 单次决策上限：20 个步骤、10 个动作；依赖不允许自依赖、重复、悬空与环；动作必须指向本次或已知步骤。
- 步骤状态：`pending`（唯一可派发）、`ready`、`running`、`done`、`skipped`、`failed`；只有 `done` 才满足依赖，`skipped` / `failed` 都不会让下游步骤就绪。
- `ParseDecision` 只做依赖去重这一项规范化；步骤 key 不做大小写/空白修正，格式问题会作为校验错误暴露，而不是被静默改写。

## 7. 预算与用量

- 任务预算字段：`max_tokens`（BIGINT，默认 0=不限）、`spent_tokens`（已用 token，默认 0）、`max_steps`（integer，默认 0=不限）、`steps_used`（已新增步骤数，默认 0）。
- 创建任务时可通过请求体传入 `max_tokens` / `max_steps`；任一大于 0 才会写入预算，省略或 0 表示不限制。
- 每次模型调用（`planner` / `planner-retry` / `planner-followup` / `worker`）都会在 `llm_usage` 写一条记录（role、model、prompt_tokens、completion_tokens、total_tokens），并在**同一事务**中累加 `missions.spent_tokens`。
- 预算是**软上限**：只在规划轮次开始时判定，正在进行的模型调用不会被中断，因此 `spent_tokens` 可能略微超过 `max_tokens`；超限后任务按正常完成流程收尾（状态 `done`，不是 `failed`）。
- `steps_used` 只统计实际新增的步骤；重复声明的已有步骤不重复计数。

## 8. 存储（0004 迁移）

- `plan_steps`：`tenant_id`、`id`、`mission_id`、`key`、`title`、`detail`、`status`（CHECK 六状态）、`priority`、`created_at`、`updated_at`；`PRIMARY KEY (tenant_id, id)`、`UNIQUE (tenant_id, mission_id, key)`；索引 `idx_plan_steps_mission (tenant_id, mission_id, status, priority DESC, id)`。
- `step_dependencies`：`step_id`、`depends_on_step_id`；`PRIMARY KEY (tenant_id, step_id, depends_on_step_id)`，`CHECK (step_id <> depends_on_step_id)`，两个外键级联到 `plan_steps`。
- `llm_usage`：`mission_id`（可空）、`role`、`model`、三个 token 计数、`created_at`；索引 `idx_llm_usage_mission (tenant_id, mission_id, id)`。
- `missions` 增加 `max_tokens`、`spent_tokens`、`max_steps`、`steps_used` 四列（默认 0）。

## 9. 安全

- API Key 只放在环境变量或本地 `.env` 中（`.env` 已被 `.gitignore` 忽略），**不得提交到仓库**；配置示例只使用空占位符。
- 日志不打印密钥；模型接口的错误响应体会先抹掉 API Key（替换为 `[已隐藏]`）、截断到 2048 字节并保证 UTF-8 合法，再写入错误信息。
- `api_key` 为空时不发送 `Authorization` 头，便于本地假服务与免鉴权网关。

## 10. 测试与实测

- **单元测试**：`internal/domain/plan` 覆盖解析（围栏 / 杂质 / 数组拒绝）、校验（上限、key、依赖、环）、就绪判定与状态；`internal/infra/llm` 覆盖请求编码、响应解析、重试分类（429/5xx/网络）、JSON 模式 400 回退、错误体脱敏与截断。
- **假 LLM 端到端**：`internal/app/ai` 用 httptest 模拟 `/chat/completions`，按系统提示词区分规划器与执行器，验证“规划 → 执行 → fact → 完成”、计划步骤状态、`llm_usage` 记录与 `spent_tokens` 累计（需要真实 PostgreSQL）。
- **完整测试**：`go test ./...` 共 13 个测试包全部通过（数据库相关测试需要设置 `BEACONX_TEST_PG_DSN`）。
- **真实模型冒烟**：使用 DeepSeek 的 OpenAI 兼容端点、模型 `deepseek-flash`。任务在约 50 秒内走到 `done`；执行了 3 个动作、产出 3 条 fact；planner + worker 共 5 次调用、约 3.8k tokens。模型判定“未实际执行探测”并如实写入事实，说明无工具阶段的边界行为符合预期。

复现真实冒烟时，只需设置下列环境变量（示例值均为占位符，请勿提交真实密钥）：

```bash
export BEACONX_ADMIN_TOKEN='请填入管理令牌'
export BEACONX_LLM_BASE_URL='https://api.deepseek.com'
export BEACONX_LLM_MODEL='deepseek-flash'
export BEACONX_LLM_API_KEY='请填入服务商提供的密钥'
```

## 11. M4 替换点

M4 接入真实工具后：实现新的 `runner.Worker`（在 `Execute` 中调用工具、采集证据并把可验证结果写成 fact / finding），替换 `ai.Worker` 即可；`runner.Planner`、计划模型、依赖调度、预算、SSE 与状态机保持不变。若 M4 需要更强的规划提示词或工具级审批，可在 `ai.Planner` 的提示词与 `Worker` 的调用路径上扩展，而不改动引擎。

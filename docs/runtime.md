# 运行时与引擎（M2）

本文描述 BeaconX M2 的任务运行时：状态机、准入调度、规划/执行循环、动作租约、暂停恢复与崩溃恢复，以及事件流与鉴权。

权威来源是 `internal/domain/task`（状态机）、`internal/app/runner`（引擎）、`migrations/0003_runtime.sql`（数据表）、`internal/httpapi`（任务接口与 SSE）与 `internal/app/events`（内存总线）；本文只做解释，若与代码不一致，以代码为准并回来修正本文。

## 1. 任务生命周期

### 状态

| 状态 | 中文 | 含义 |
| --- | --- | --- |
| `queued` | 排队中 | 已创建、等待准入；未占用执行资源 |
| `running` | 运行中 | 已准入，任务执行器存活并领取动作 |
| `paused` | 已暂停 | 人为暂停；执行器已停止，可恢复 |
| `finalizing` | 收尾中 | 不再领取新动作，等待收尾动作与总结 |
| `done` | 已完成 | 终态：正常结束 |
| `failed` | 已失败 | 终态：失败终止 |
| `cancelled` | 已取消 | 终态：取消或删除；具体原因保留在事件的 `reason` |

只有 `done` / `failed` / `cancelled` 是终态；终态任务不再接受任何事件（`ErrTerminal`）。

### 事件

状态机只消费事件类型，`reason` 与时间戳供审计、日志与展示使用：`created`（创建）、`admitted`（准入）、`paused`（暂停）、`resumed`（恢复）、`finalizing`（进入收尾）、`completed`（完成）、`failed`（失败）、`cancelled`（取消）、`deleted`（删除，与取消同落 `cancelled`）、`recovered`（进程重启对账，回排队）。`started` 为与审计表对齐而保留，当前迁移表不消费它，发送会返回 `ErrIllegalTransition`。

### 合法迁移

| 当前状态 | 事件 | 下一状态 | 副作用 |
| --- | --- | --- | --- |
| （空） | `created` | `queued` | — |
| `queued` | `admitted` | `running` | 启动执行器 |
| `queued` | `cancelled` / `deleted` | `cancelled` | — |
| `queued` | `recovered` | `queued` | — |
| `running` | `paused` | `paused` | 停止执行器 |
| `running` | `finalizing` | `finalizing` | 停止执行器 |
| `running` | `failed` | `failed` | 停止执行器 |
| `running` | `cancelled` / `deleted` | `cancelled` | 停止执行器 |
| `running` | `recovered` | `queued` | 停止执行器 |
| `paused` | `resumed` | `queued` | — |
| `paused` | `cancelled` / `deleted` | `cancelled` | — |
| `finalizing` | `completed` | `done` | — |
| `finalizing` | `failed` | `failed` | — |
| `finalizing` | `cancelled` / `deleted` | `cancelled` | — |

其余组合一律返回 `ErrIllegalTransition`（未知状态 `ErrInvalidStatus`、未知事件 `ErrInvalidEvent`）。状态迁移由 `pg.Store.TransitionMission` 以 CAS 方式执行，并在**同一事务**内追加对应事件；写入前会用状态机校验「起始状态 + 事件 → 目标状态」是否成立，因此数据库更新与领域规则不会各写一套。

## 2. 引擎结构

引擎（`runner.Engine`）由 `main` 在启动时创建，负责恢复对账、准入调度与单个任务的执行循环。

- **准入调度**：调度协程每 1s 触发一次，可用槽位 = `MaxConcurrent - 当前运行任务数`；从 `missions` 中按 `queued_at NULLS FIRST, id`（即 FIFO）取最多「槽位数」个排队任务，逐个 CAS `queued → running` 并发出 `admitted` 事件，然后为每个任务启动一个执行协程。并发上限默认 2，可配置（`engine.max_concurrent`）。
- **每任务执行循环**：规划与执行在同一循环内交替；
  - 规划按 `engine.planner_interval`（默认 2s）到点触发：调用 `Planner.Plan`；若返回「已完成」且任务没有 `open` / `running` 的动作（`CountOpenIntents`），进入收尾：`running → finalizing → done`，并发出 `finalizing`、`completed` 事件；
  - 执行按 `engine.worker_interval`（默认 200ms）轮询：用租约领取一条动作（`ClaimNextIntent`），在工作协程中执行（心跳协程按 `engine.heartbeat_interval` 续约），结束后 `FinishIntent` 写回动作终态；执行报错则把动作置为 `blocked`。
- 引擎参数默认值：`MaxConcurrent=2`、`PlannerInterval=2s`、`WorkerInterval=200ms`、`LeaseTTL=30s`、`HeartbeatInterval=10s`；`Owner` 默认 `runner-1`，进程内默认使用 `beaconx-<pid>` 作为租约持有者。

### 启动与停止

`main` 在迁移成功后创建事件总线（缓冲 512）、Store 与引擎；引擎启动时先执行恢复对账、再启动调度循环，随后构建 HTTP 处理器并开始监听。关闭时按「HTTP 优雅关闭 → 引擎停止（取消调度与全部任务协程并等待退出）→ 事件总线关闭」的顺序收尾。进程退出不会让任务永久停在 `running`：下次启动的恢复流程会把它退回 `queued` 并重新准入。

## 3. 动作租约

动作即 `operation_nodes` 中 `kind='action'` 的节点；M2 为它增加了 `priority`、`lease_until`、`leased_by`、`attempts` 四列：

- **领取**：`ClaimNextIntent` 在一条 SQL 中把满足条件的动作置为 `running`，写入 `lease_until = now() + ttl`、`leased_by = owner`，并把 `attempts` 加一；选择条件为 `state='open'` 或「`running` 且租约已过期/缺失」，按 `priority DESC, id` 取最早一条，使用 `FOR UPDATE SKIP LOCKED` 允许多 worker 并发领取而不互相阻塞。
- **续约**：执行期间心跳调用 `HeartbeatIntent`，仅在仍持有租约（`state='running'` 且 `leased_by` 匹配）时延长 `lease_until`；续约失败说明租约已被回收，执行协程随之退出。
- **完成**：`FinishIntent` 只接受 `done` / `exhausted` / `blocked` 三个终态，且仅当持有者匹配时生效，成功后清空租约列。
- **释放**：暂停、取消、删除任务时调用 `ReleaseMissionLeases`，把该任务下所有 `running` 动作放回 `open` 并递增 `attempts`，避免恢复或重启后要等租约自然过期。
- **回收**：启动时 `ResetExpiredLeases` 把过期或缺失租约的 `running` 动作放回 `open`（同样递增 `attempts`）。

## 4. 暂停、恢复、取消与删除

- **暂停**：仅允许 `running → paused`；先迁移状态并发 `paused` 事件（`actor=user`），再取消执行协程并等待其退出（上限 10s），最后释放动作租约。
- **恢复**：`paused → queued` 并发 `resumed` 事件；任务重新回到队列，由准入调度决定何时再次开跑（不直接进入 `running`）。
- **取消**：允许从 `queued` / `running` / `paused` / `finalizing` 迁移到 `cancelled`，发 `cancelled` 事件，停止执行协程并释放租约。
- **删除**：先尽力取消（若仍可取消），停止执行协程，再删除 `missions` 行；节点、边、锚点、事件、范围与资产链接随外键级联删除，接口返回 204，之后查询返回 404。
- **预算超限暂停（M5）**：引擎在每轮执行循环开始检查 token 预算；若 `max_tokens > 0` 且 `spent_tokens >= max_tokens`，把任务从 `running` 迁移到 `paused`（事件 `paused`，`actor=system`，reason=「模型 token 预算超限暂停」）并退出该任务协程。这是**暂停而不是失败**：提高预算或确认后调用 `resume`，任务重新排队、再次准入继续。步骤预算超限仍由规划器按完成收尾。
- 状态不允许该操作时引擎返回 `ErrNotRunning`，HTTP 层映射为 409 `conflict`；任务不存在映射为 404 `not_found`。

| 场景 | HTTP 结果 |
| --- | --- |
| 暂停 / 恢复 / 取消成功 | 200 + 任务详情 |
| 任务当前状态不允许该操作 | 409 `conflict` |
| 任务不存在 | 404 `not_found` |
| 删除成功 | 204（无响应体） |
| 缺少或错误的令牌 | 401 `unauthorized` |

## 5. 崩溃恢复

引擎启动时依次执行两步对账，目标是不留下永久的 `running`：

1. `ResetExpiredLeases`：回收所有过期或缺失租约的 `running` 动作，放回 `open`。
2. `ResetRunningMissions`：把数据库中遗留的 `running` / `finalizing` 任务退回 `queued`（`queued=true`、`queued_at=now()`、`reason='进程重启恢复'`），并为每个任务追加一条 `recovered` 事件（`actor=system`）。

恢复完成后由调度器按正常流程重新准入；重复执行恢复是幂等的（`queued + recovered` 不产生额外副作用）。v1 假设单进程运行，未实现多副本之间的租约协调。

## 6. SSE 事件流

`mission_events` 是只追加的事件表（`id` 自增、`type`、`actor`、`reason`、`payload JSONB`、`created_at`），既是 SSE 回放来源，也是状态追溯与审计的事实来源。接口为 `GET /api/v1/missions/{id}/events`：

- **先订阅后回放**：处理器先订阅内存事件总线，再查询数据库回放 `id > afterID` 的事件，因此回放期间产生的新事件不会丢失；回放按 id 升序输出，实时事件按 `lastSentID` 去重。
- **游标**：优先 `Last-Event-ID` 请求头（浏览器 `EventSource` 断线重连会自动携带），其次 `?since=`；空值、非法值与负数一律按 0 处理。
- **帧格式**：`id: <id>`、`event: <type>`、`data: <payload JSON>`，空行结束；回放上限 500 条；心跳为注释行 `: ping`（默认 20s），每条事件后立即 Flush。
- **事件内容**：帧里的 `data` 是 `events.Event` 的 JSON（`id` / `mission_id` / `type` / `payload` / `created_at`）；`actor`、`reason` 等审计字段保存在 `mission_events` 中，不回放进 SSE 帧。
- **慢消费者**：内存总线按任务分发，发布非阻塞；订阅者缓冲（进程内 512）被写满时该订阅者被丢弃并关闭通道，SSE 连接随之结束，客户端重连后凭游标回放补洞。
- **错误**：回放失败且尚未写出任何帧时返回 500 JSON（`replay_failed`）；任务不存在 404；任务运行时或事件总线未装配时 503（`sse_unavailable`/`runtime_unavailable`）。

事件优先级与错误码：`400 bad_request`（任务标识/请求体非法）、`401 unauthorized`（缺少或错误令牌）、`404 not_found`、`409 conflict`（当前状态不允许该操作）、`500 internal_error` / `replay_failed`、`503 auth_unavailable` / `runtime_unavailable` / `sse_unavailable`。

## 7. 管理鉴权

任务接口默认全部需要认证（`/healthz`、`/readyz`、`/api/v1/system/*` 为公开端点）：

- 令牌来自 `auth.admin_token` 或环境变量 `BEACONX_ADMIN_TOKEN`；未配置时启动会生成 24 字节随机数（48 位十六进制字符）作为临时令牌，并以 WARN 级别打印到日志（字段名 `token`），仅本次进程有效。
- 客户端优先使用 `Authorization: Bearer <token>`；SSE 无法自定义请求头，也可用 `?token=<token>`。两种方式都使用常量时间比较；空令牌一律拒绝。
- 未安装认证组件时受保护路由返回 503（fail closed），令牌错误返回 401。

## 8. 规划/执行接口：M2 空实现 → M3 LLM 实现

`runner.Planner` 与 `runner.Worker` 是引擎的注入点。M2 不接入 LLM：`runner.IdlePlanner.Plan` 直接返回「已完成」，`runner.NoopWorker` 把动作标记为 `done`，因此没有待办动作的任务会在一个规划周期内走完 `queued → running → finalizing → done`。

M3 起由 `internal/app/ai` 提供 LLM 实现：Planner 读取态势快照、调用 OpenAI 兼容模型生成结构化计划与动作；Worker 把模型的观察写成 fact 节点。启用条件、规划流程、追问语义、预算与真实冒烟结果见 [AI 规划与执行](ai.md)。未配置 `llm.base_url` 与 `llm.model` 时仍使用上述空实现；配置后启动日志打印 `规划模式 mode=llm model=<模型名>`。

工具调用在 M4 接入：`ai.Worker` 装配工具后进入「模型 → 工具 → 模型」循环，每次工具调用都统一经 `internal/app/executor`（策略判定 → 审批 → 执行 → 审计），executor 是工具调用的唯一入口；命中 ask 时 worker 会阻塞等待人工决策或超时（默认 60s）。规则、审批与审计详见 [工具与策略](tools-policy.md)。通过 `runner.New(cfg, store, planner, worker, log, sink)` 注入即可，准入、租约、恢复、SSE 与状态机逻辑保持不变。事件通过 `EventSink` 投递到 `events.Broker`，由 SSE 处理器回放与转发。

证据采集在工具执行前发生（M5）：executor 在策略门通过、真正调用工具之前先登记 `traffic_exchanges` 的 intent 阶段（参数脱敏后加密落盘），工具返回后再写 result（输出、状态码、错误）；采集失败只记 WARN，不阻塞工具执行。证据快照、签名与篡改校验见 [证据链](evidence.md)。

发现、复测与报告（M6.2-B2）：当前写入路径只有 HTTP API（人工登记发现、绑定证据快照、发起/推进复测），没有自动发现生成器或复测执行器；AI 自动写 finding、复测执行均未实现。报告 `GET /missions/{id}/report` 是只读投影（任务元信息 + 发现 + 计划步骤 + fact 节点 + 证据统计），生成过程不调用模型，除生成时间外同一数据得到同一报告；报告与发现/复测接口的细节见 [项目地图](project-map.md) §5/§6。

洞察接口（M6.2-B3）：流量列表/详情、命令历史、LLM 记录、任务用量、用量聚合与 Dashboard 都是只读投影，不写入运行时状态；流量请求/响应正文按需调用 `evidence.GetBlob` 解密后以 `text/plain` 返回，blob 缺失/篡改/密钥不匹配统一 409 `tampered`；`DELETE /traffic/{id}` 只删采集行，保留正文 blob 与已冻结证据。Dashboard 每次请求实时聚合（Count/Sum/分组），`spent_total` 取任务累计 `spent_tokens`，`today` 按数据库时区当天求和。

## 9. 验收与本地实测

M2 验收标准：kill -9 后无永久 `running`；暂停/恢复/删除可用；断线补洞无丢失。

- **测试**：`go test ./...` 共 10 个包全绿（config、domain/asset、domain/graph、domain/task、httpapi、infra/migrate、infra/pg、platform、app/events、app/runner）。引擎集成测试 5 个通过（完成流程、暂停/恢复/取消、崩溃恢复、删除、并发上限），需要真实 PostgreSQL（`BEACONX_TEST_PG_DSN`）。
- **HTTP 冒烟**：创建任务返回 201；1 秒内 `status=done`，`created → admitted → finalizing → completed` 四条事件可回放；对已完成任务 `POST .../pause` 返回 409；`DELETE` 返回 204，再查询返回 404；未带令牌返回 401。

复现命令（需要本机 PostgreSQL）：

```bash
export GOCACHE=$PWD/.cache/gocache GOMODCACHE=$PWD/.cache/gomodcache GOTMPDIR=$PWD/.cache/tmp GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct GOSUMDB=off
BEACONX_TEST_PG_DSN='postgres://beaconx:beaconx@127.0.0.1:55432/beaconx?sslmode=disable' go test -count=1 ./...
```

## 10. B4/B5 扩展与风险（M6.2-B4/B5）

- **模块位置**：策略/审批/设置/日志/通知/资产补全在 `internal/httpapi/{policy_api,settings_api,logs_api,notifications_api,assets_extra}.go`；MCP/Skills/Agents/Workspace/Sync/报告/流量/证据增强在 `{mcp_api,skills_api,agents_api,workspace_api,sync_api,traffic_extra,evidence_extra,report_extra,retest_extra}.go`；领域/存储见 `internal/app/{mcp,report,syncdata,workspace}` 与 `internal/infra/pg/store_*`。
- **日志**：进程级环形缓冲（2000）由 `internal/logging` 提供，SSE 先订阅后写头、20s 心跳、慢订阅丢帧；无跨进程聚合/after_id。
- **设置**：`settings` 表持久化 + 可读；B7 起启动读 settings 覆盖 executor/engine，PATCH 即时热生效；运行中任务预算不回溯、调低并发不抢占（🟡）。
- **通知**：webhook 真实 POST、email/generic 记 skipped；无真实事件触发、无出站 URL 白名单、config 明文（🟡）。
- **文件沙箱**：Skills/Workspace 用状态目录 + 路径 Clean/前缀校验/符号链接拒绝 + 大小限制；非空目录删除需 recursive。
- **Sync**：异步运行 ≤60s、manual/http_json、SSRF 三层校验默认拒私网、逐条容错 skipped；无崩溃恢复/保留策略（🟡）。
- **流量重放（高风险 🟡）**：`POST /traffic/{id}/replay` 直接调用 http_probe，**绕过 executor 的政策门/审批**，仅受 Bearer+租户+http_probe 限制；接入前不应开放给普通使用。
- **证据**：`GET /evidence/{id}/audit` 覆盖 verify 与部分正文 read（best-effort）；`POST /evidence/rotate` 已实现（200，重加密全部 blob；新密钥不落盘，存量签名失效需离线重签 🟡）。
- **Agents**：仅定义 CRUD/test，**未接入 engine 装配（🟡）**；test 不调用 LLM，未知工具以 200+warnings 返回。

## 11. B6 硬化（M6.2-B6）

- **流量重放走 executor**：`POST /traffic/{id}/replay` 现在通过 `Options.Executor` 执行 `http_probe`，经策略门/审批/审计/证据采集；未接线 503 `executor_unavailable`，策略 deny/审批拒绝 403，审批超时 409，未知工具/其它 502；响应 `{policy_gated:true,output,status,traffic_id?}`，traffic_id 为 best-effort（可为 0）。不再旁路直调。
- **证据轮换**：`POST /evidence/rotate` 在 Store 写锁下逐 blob 旧解密→新加密，失败 best-effort 回滚；新密钥不落盘，需更新 `BEACONX_EVIDENCE_KEY`/evidence.key 并重启；**存量 Snapshot HMAC 签名随主密钥切换失效，Verify→ErrTampered，当前无自动重签（🟡）**，生产轮换必须备份 PG+证据目录并走离线重签。
- **证据审计**：verify 成功/失败都写 `evidence_access_log`；流量正文读取在 blob 被证据快照引用时写 read（hash 反查）；均为 best-effort，仍有路径未埋点（🟡）。
- **可观测**：新增全局活动流 `GET /activity`（mission_events 跨任务倒序，before_id 游标）；Dashboard 新增 `hourly` 最近 24 小时 UTC 桶；`GET /missions/{id}/graph` 增加 `after_id`/`next_after_id`（节点升序，取 id>after_id；edges/anchors 全量，过滤仍受 ListNodes 1000 上限 🟡）。
- **MCP/Workspace/Skills 硬化**：stdio 默认禁用、env 白名单、独立临时工作目录、stdout≤1MiB、超时 kill 进程组、命令白名单/`BEACONX_MCP_STDIO_DIR`、Windows 400；Workspace 默认 100MB 配额（`BEACONX_WORKSPACE_QUOTA_MB`），PUT/上传预检+复核，超限 413 quota_exceeded，per-mission 锁仅进程内；Skills `.versions` 保留 5 份，restore 先快照当前状态；B7 起快照含 meta.json，restore 同时回滚 files+description/version（旧快照 warning）。
- **from-node 唯一性**：0016 `uq_findings_node` 部分唯一索引兜底，并发重复→409。

## 12. B7 收口（M6.2-B7）

- **settings 热生效**：启动读 settings 覆盖 executor/engine 配置；PATCH 即时生效（executor 默认动作/审批超时、engine max_concurrent、mission 默认预算共享 RuntimeConfig）。新任务未给预算用默认（0=默认，默认 0=不限制）；运行中任务预算不回溯、调低并发不抢占（🟡）。
- **LLM 正文采集**：`internal/app/ai/llm_capture.go` 在 planner/worker 6 个调用点写 `llm_exchanges`（失败也记 error）；messages/响应脱敏（键级+密钥形态+tool_calls.arguments）、单条 64KB UTF-8 截断；`BEACONX_LLM_CAPTURE` 默认 true，写失败 Warn 不阻塞；**无保留期清理（🟡）**，任务删除级联删除。
- **证据轮换重签**：rotate 成功后遍历租户全部快照用新 key 重签，返回 `{rotated,resigned}`；中断 500 `rotate_resign_failed` + 已完成数；存在短暂 Verify 409 窗口；多租户需全租户范围（🟡）。
- **策略 immutable**：除 `enabled` 外任何字段提交 409。
- **sync 恢复/保留**：启动 running>15min→failed；终态按 `BEACONX_SYNC_RETENTION_DAYS`（默认 30，0=不清理）清理；pending 不恢复（🟡）。
- **通知 SSRF**：webhook 发送前/拨号前校验（loopback/私网/link-local/CGNAT→400 webhook_blocked）、禁重定向/环境代理；`BEACONX_NOTIFY_ALLOW_PRIVATE` 默认 false；响应 config 敏感键/url userinfo 掩码，存储明文（🟡）。
- **Skills/Workspace**：快照含 meta.json，restore 回滚 files+description/version（旧快照 warning）；workspace 配额用 pg advisory xact lock 跨进程，未注入退化进程内锁。

## 13. B8 收口（M6.2-B8）

- **通知加密**：敏感 config 用证据派生密钥 EncryptSecret/DecryptSecret 整体加密为 `{__enc,payload,url}`，DB 无明文；响应仍掩码；旧明文 best-effort 迁移；Evidence=nil 创建/更新敏感配置 503。
- **通知事件推送**：后台 dispatcher 每 1s 轮询 mission_events（跨租户），仅 approval_required/completed/failed/paused；先 INSERT ON CONFLICT 占位（UNIQUE tenant,event_id,channel_id）再发送，多副本单发送者；deliveries webhook sent/failed、email/generic skipped；detail 片段脱敏≤512B；**at-most-once，启动游标取当前最大 event id（停机不补推）**。
- **证据审计**：action 扩展 read/verify/export/create/rotate；create/rotate 均 best-effort；export 失败不记（设计限制）。
- **全租户重签**：rotate 遍历全部租户快照重签；轮换前收集全部租户加密通知配置、轮换后重加密回写，收集失败拒绝轮换；极窄窗口新建渠道可能遗漏（需重跑）。
- **启动维护**：`BEACONX_LLM_RETENTION_DAYS`（默认 7，0=不清理）清理 llm_exchanges（不动 llm_usage）；sync pending>15min→failed；running 恢复/终态保留沿用。
- **Agents 接入**：Planner/Worker 按任务解析启用 role=planner/worker 定义，应用 system_prompt/model/tools 白名单（未知工具忽略+Warn）；每任务每角色缓存，InvalidateAgents 热失效；首次记 agent_applied；无定义行为不变；`WithModel` 优先/同端点异模型/回退默认并 Warn。
- **graph 下推**：kind/state/after_id/limit 下推到 SQL（500/2000），删除 1000 静默截断；edges 仍 5000 上限。
- **Skills/Windows**：`BEACONX_SKILLS_KEEP_VERSIONS`（默认 5，1..50，非法回退）每次裁剪读取；versions 增 has_meta；CI 增 windows-test（mcp/llm），Windows stdio 400。

# 工具与策略（M4）

本文描述 BeaconX M4 的工具层与执行期策略：工具注册表、策略规则、人工审批、审计，以及已知安全边界。

权威来源是 `internal/app/tools`、`internal/domain/policy`、`internal/app/executor`、`internal/infra/pg/policy_store.go`、`migrations/0005_policy.sql`、`internal/app/ai/worker.go`（工具循环）、`internal/httpapi/approvals.go` 与 `cmd/beaconx/main.go`；本文只做解释，若与代码不一致，以代码为准并回来修正本文。计划与执行的上层逻辑见 [AI 规划与执行](ai.md)。

## 1. 工具注册表

`internal/app/tools` 定义工具模型与注册表：

- `Definition`：`Name`、`Description`、`Parameters`（JSON Schema 原文）、`Risk`（low / medium / high）、`Roles`（可用角色列表）。
- `Tool` 接口：`Definition()` 与 `Execute(ctx, args json.RawMessage) (Result, error)`；`Result` 含 `Output` 文本与 `Meta` 元数据。
- `Registry`：`Register`（拒绝空名、非法风险级别、非法 JSON Schema、重复注册）、`Get(name)`、`List(role)`（角色为空返回全部，结果按名称排序）。

内置工具（均只对 `worker` 角色可见）：

| 工具 | 风险 | 角色 | 参数 schema（required） | 关键限制 |
| --- | --- | --- | --- | --- |
| `http_probe` | medium | worker | `url`（必填）；可选 `method` / `headers` / `body` / `timeout_seconds` | 仅 http/https；默认 GET；超时 1–60s，缺省 15s；输出最多 32KB；额外 30s 客户端超时 |
| `shell_exec` | high | worker | `command`（必填）；可选 `timeout_seconds` / `work_dir` | 非 Windows 用 `/bin/sh -c`，Windows 用 `cmd /C`；超时 1–120s，缺省 30s；输出最多 32KB；失败时错误信息附带截断输出 |

工具的 JSON Schema 原样透传给模型与前端；注册表只校验它语法合法。`http_probe` 的参数结构如下（节选）：

```json
{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "完整 URL（仅支持 http/https）"},
    "method": {"type": "string", "description": "HTTP 方法，默认 GET"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数（1-60）"}
  },
  "required": ["url"]
}
```

`shell_exec` 的 schema 结构类似：required 为 `command`，可选 `timeout_seconds`（1–120）与 `work_dir`。

新增工具的步骤：实现 `Tool` 接口（声明 Definition 与 Execute）→ 在 `main` 中注册到 `Registry` → 在 Definition 中声明角色 → 如有必要补充策略规则。注册表只做静态描述校验；参数合法性由各工具自己的 `Execute` 校验（当前没有统一的 JSON Schema 执行前校验）。

## 2. 执行链（唯一入口）

任何工具调用都必须经过 `internal/app/executor`，避免某个 Agent 绕过策略检查。`Executor.Execute` 的流程是：

1. 从注册表按名称取工具（未知工具直接报错）。
2. 读取本租户**启用中**的策略规则（存储层 `WHERE enabled = true`，按优先级倒序）。
3. `policy.Evaluate(tool, argsJSON, rules, defaultAction)` 得到 allow / ask / deny。
4. `deny`：写审计（outcome=`denied_by_rule`）并返回错误，不执行工具。
5. `ask`：按规则或配置确定审批超时，创建 `approvals` 记录，写入任务事件 `approval_required`（payload 含 `approval_id` 与 `tool`），然后轮询等待人工决策；批准后继续执行，被拒绝/取消/超时则写审计并返回错误。
6. `allow`：直接执行。
7. 执行结束后写审计：成功为 `executed`，工具报错为 `tool_error`；detail 保存输出摘要（截断 500 字符）。

`Executor.ExecuteText` 是给 AI Worker 使用的文本包装：成功返回工具输出，失败返回错误文本。M4 中只有 `ai.Worker` 通过 `SetTools` 装配执行器；未配置 LLM（idle 模式）时执行器不会被调用。审批等待会**阻塞当前 worker**（默认最长 60s），直到人工决策、超时或任务上下文取消。

执行链伪代码（便于理解调用顺序）：

```text
tool     = Registry.Get(name)
rules    = Store.ListPolicyRules(tenant, enabled=true)
decision = policy.Evaluate(tool, args, rules, default_action)
if deny:  Audit(denied_by_rule); return error
if ask:   approval = CreateApproval(...); emit approval_required
          wait until approved / rejected / cancelled / timeout
          if not approved: Audit(rejected_by_user 或 approval_timeout); return error
result   = tool.Execute(args)
Audit(executed | tool_error)
return result
```

## 3. 策略规则

`policy.Rule` 字段：

| 字段 | 含义 |
| --- | --- |
| `name` | 规则名，必填，租户内唯一；审计与审批界面用它标识命中规则 |
| `tool` | 工具名（精确匹配）；`*` 表示所有工具；空字符串在匹配时也按 `*` 处理，但 `Validate` 会拒绝空 tool |
| `arg_pattern` | 可选正则，非空时对**参数原始 JSON 文本**做 `regexp.MatchString` |
| `action` | 命中后的裁决：`allow` / `ask` / `deny` |
| `priority` | 越大越先匹配；同优先级保持输入顺序（稳定排序） |
| `timeout_seconds` | ask 的审批超时；`<=0` 用执行器默认值（60s） |
| `enabled` | 配置层开关；存储层只返回启用规则 |
| `immutable` | 内置底线规则标记；播种不会覆盖用户改动，当前也没有规则 CRUD API |

匹配语义：按 `priority` 降序稳定排序，规则 `tool` 命中且（若有）`arg_pattern` 命中原始 JSON 的第一个规则生效；没有命中时使用 `policy.default_action`。`Evaluate` 是纯函数，不读 `Enabled`；配置写错（正则非法、命中规则动作非法、默认动作非法）一律返回错误（fail closed），而不是跳过规则继续匹配。

内置五条默认规则由 `policy.SeedDefaults()` 在启动时幂等写入（`ON CONFLICT (tenant_id, name) DO NOTHING`）：

| 规则名 | 工具 | 参数正则 | 动作 | 优先级 | 超时 | immutable |
| --- | --- | --- | --- | --- | --- | --- |
| 允许 GET/HEAD 请求 | `http_probe` | `"method"\s*:\s*"(GET\|HEAD)"` | allow | 100 | — | 是 |
| 禁止下载并执行 | `shell_exec` | `(curl\|wget)[^"]*\|[^"]*(sh\|bash)` | deny | 200 | — | 是 |
| 禁止删除根目录 | `shell_exec` | `rm\s+-rf?\s+/(\s\|"\|\$)` | deny | 200 | — | 是 |
| shell 需人工审批 | `shell_exec` | —（不限制参数） | ask | 50 | 60s | 否 |
| HTTP 其他方法需审批 | `http_probe` | —（不限制参数） | ask | 50 | 60s | 否 |

注：`arg_pattern` 匹配的是参数 JSON 原文；源码中的引号写作 `\"`，上表为便于阅读省略了该转义。

因此：GET/HEAD 在 deny 规则不匹配时被放行；`shell_exec` 默认人工审批；下载并执行、删除根目录这类明确底线先于一切 allow/ask 命中。`immutable` 当前只是标记：播种不会覆盖同名的用户规则，领域层与 API 也不会删除它；规则的新增/修改/删除接口属于 M4.1。

## 4. 审批

`approvals` 记录一次 ask 决策：

- 状态：`pending` → `approved` / `rejected` / `timeout` / `cancelled`（数据库 CHECK；当前代码只会写 approved / rejected / timeout，`cancelled` 预留给后续能力）。
- `expires_at = now() + approval_timeout`（规则 `timeout_seconds` 优先，否则用 `policy.approval_timeout`，默认 60s）。
- 只有 `pending` 能被决策：`DecideApproval` 用 `WHERE status='pending'` 的 CAS 更新，已处理或不存在返回 conflict。
- worker 每 300ms 轮询一次审批状态；本地等待超时后会尝试 `ExpireApproval`（仅当 `expires_at < now()` 才置 timeout），然后按“未批准”处理。

接口（均需要管理令牌）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/approvals?status=&limit=` | 审批列表，按 id 倒序；`limit` 默认 50（>200 时按 50 处理） |
| POST | `/api/v1/approvals/{id}/decide` | 请求体 `{"approve":true|false,"note":"…"}`；返回审批详情 |

审批超时按“未通过”处理：执行器写审计（outcome 记为 `approval_timeout`）并返回错误，不执行工具。ask 发生时写入的任务事件 `approval_required` 会经 SSE 推送给客户端；审批决策本身不额外写任务事件（决策结果体现在审批接口返回、审计与 worker 后续行为中）。

## 5. 审计

`audit_events` 是每次工具调用的一行不可变记录：

- 字段：`tool`、`action`（命中的策略动作或默认动作）、`rule_name`（命中规则；使用默认动作时为空）、`args_digest`（参数原始 JSON 的 SHA-256 十六进制）、`outcome`、`detail`、`actor`（默认 `system`）、`node_id`（可空，通常为动作节点）、`mission_id`、`created_at`。
- 原始参数只存在于 `approvals.args`（JSONB）；审计只保存摘要，避免把敏感参数长期留存在审计表。
- `outcome` 取值：`executed`（执行成功）、`tool_error`（工具返回错误）、`denied_by_rule`（规则拒绝）、`rejected_by_user`（人工拒绝或审批取消）、`approval_timeout`（审批超时）、`approval_unavailable`（审批查询/等待失败或任务上下文取消）。
- 查询接口：`GET /api/v1/missions/{id}/audit`，返回 `{"audit":[...]}`，按 id 倒序，最多 200 条。

注意：策略判定或读规则阶段就报错时，调用在写审计之前返回，因此这类失败不会产生审计行；这是当前实现的边界。

## 6. 安全取舍与已知盲区

- **正则匹配原始 JSON 的固有绕过**：`arg_pattern` 匹配的是序列化文本，对参数顺序、空白、转义敏感。例如 `curl -o a.sh && sh a.sh` 不匹配“下载并执行”规则、`rm -fr /` 不匹配 `rm -rf /` 规则，它们会落到“shell 需人工审批”而**不是 deny**；最终依赖人工审批拦截。规则只覆盖最常见的高危形态，不追求完备。
- **参数校验在工具层**：注册表只校验 schema 是合法 JSON，各工具在 `Execute` 中自行校验必填字段；没有统一的执行前 JSON Schema 校验，也没有参数级策略（按 URL/目录/命令白名单）。
- **无网络出口与沙箱约束**：`http_probe` 可访问任意 http/https 目标（包括内网），`shell_exec` 以服务进程用户身份直接执行命令；DNS 重绑定、SSRF 与进程沙箱不在 M4 范围。
- **审批可用性**：ask 会阻塞 worker 直到审批或超时；没有独立的审批通知通道、审批队列与批量决策，审批人需要经 API/SSE 观察。
- **规则作用域**：规则是租户级的，没有任务级/资产级规则；没有规则 CRUD，也没有模型兜底审批。

## 7. 配置

| 配置项 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `policy.default_action` | `BEACONX_POLICY_DEFAULT_ACTION` | `ask` | 仅当没有规则命中时使用；取值 allow / ask / deny |
| `policy.approval_timeout` | `BEACONX_APPROVAL_TIMEOUT` | `60s` | ask 的默认等待超时；规则自带 `timeout_seconds` 时优先用规则值 |

实现细节：配置加载阶段会校验 `default_action` 只能是 `allow`/`ask`/`deny`，非法值导致启动失败；`approval_timeout` 必须为正数。执行器内部对零值有兜底默认值，但正常启动路径不依赖它。工具与策略只在配置了 LLM（`llm.base_url` 与 `llm.model` 同时非空）时才会被真实使用。

## 8. 测试与实测

- **工具单元测试**：注册表可见性与重复/非法校验、两个工具 schema 是合法 JSON、`http_probe` 正常执行与拒绝非 http(s)、`shell_exec` 成功与失败输出。
- **LLM 工具协议测试**：请求侧 `tools` / `tool_choice` / `tool_calls` 形状、单个与多个工具调用解析、tool 消息回灌、429 重试与密钥脱敏。
- **执行器集成测试 4 项**：allow 规则直接执行并写审计、ask 批准后执行、deny 规则阻止、人工拒绝阻止；需要真实 PostgreSQL。
- **Worker 工具循环测试**：假 LLM 返回 tool_calls → 执行器被调用 → 工具结果回灌 → 最终输出观察 JSON。
- **完整测试**：`go test ./...` 共 16 个测试包全部通过（数据库相关测试需要 `BEACONX_TEST_PG_DSN`）。
- **真实 DeepSeek 冒烟**：模型调用 `http_probe` 与 `shell_exec` 共 14 次工具调用，全部经过策略判定与审批；GET 请求走 allow，shell 命令走 ask 并经审批 API 批准；目标不可达时模型如实记录 TCP 连接失败并产出事实；审计表可见 allow/ask 与 executed/tool_error 两类结果。

## 9. M4.1 延期项

- MCP 连接池与外部工具接入。
- 技能（skills）版本化与加载。
- 规则未命中时的模型兜底审批（当前固定使用 `default_action`）。
- 策略规则的 CRUD/API 与更细的作用域（任务级、资产级）。
- 更完备的参数级策略（命令/URL/目录白名单、结构化参数匹配）。

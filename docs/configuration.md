# 配置说明

## 加载顺序

1. 内置默认值
2. 配置文件（`BEACONX_CONFIG`，默认 `./config.json`；不存在则跳过）
3. 环境变量（`BEACONX_*`，逐项覆盖）

配置在启动时解析一次；业务配置（Agent、模型、策略、推送等）存放在数据库，通过 UI 修改，不需要重启。

## 配置文件

完整示例见仓库根目录的 [config.example.json](../config.example.json)。

```json
{
  "server": {
    "addr": ":6969",
    "read_header_timeout": "10s",
    "idle_timeout": "60s",
    "request_timeout": "30s",
    "shutdown_timeout": "15s",
    "max_body_bytes": 8388608
  },
  "database": {
    "dsn": "postgres://user:pass@127.0.0.1:5432/beaconx?sslmode=disable",
    "max_conns": 10,
    "min_conns": 1,
    "connect_timeout": "8s"
  },
  "tenant": { "default_id": 1 },
  "state": { "dir": "data" },
  "log": { "level": "info", "format": "json" },
  "engine": {
    "max_concurrent": 2,
    "planner_interval": "2s",
    "worker_interval": "200ms",
    "lease_ttl": "30s",
    "heartbeat_interval": "10s"
  },
  "auth": {
    "admin_token": ""
  },
  "llm": {
    "base_url": "https://api.deepseek.com",
    "api_key": "",
    "model": "deepseek-flash",
    "max_tokens": 2048,
    "timeout": "60s",
    "max_retries": 2,
    "fallbacks": [
      {
        "base_url": "https://api.openai.com/v1",
        "api_key": "",
        "model": "gpt-4o-mini"
      }
    ]
  },
  "policy": {
    "default_action": "ask",
    "approval_timeout": "60s"
  },
  "evidence": {
    "dir": "",
    "key_hex": ""
  },
  "selfupdate": {
    "enabled": true,
    "repo": "orzl1/BeaconX"
  }
}
```

时间类字段既接受 Go duration 字符串（如 `"30s"`、`"200ms"`），也接受以纳秒为单位的整数。`llm.temperature` 是可选项，示例中省略：不配置时不发送该参数，使用服务端默认值。

## 环境变量

| 变量 | 对应配置 | 默认值 | 备注 |
| --- | --- | --- | --- |
| `BEACONX_CONFIG` | — | `config.json` | 配置文件路径 |
| `BEACONX_ADDR` | `server.addr` | `:6969` | 监听地址 |
| `BEACONX_REQUEST_TIMEOUT` | `server.request_timeout` | `30s` | 请求上下文超时 |
| `BEACONX_SHUTDOWN_TIMEOUT` | `server.shutdown_timeout` | `15s` | 优雅退出等待上限 |
| `BEACONX_DB_DSN` | `database.dsn` | 空（必填） | PostgreSQL 连接串 |
| `BEACONX_DB_MAX_CONNS` | `database.max_conns` | `10` | 连接池上限 |
| `BEACONX_DB_MIN_CONNS` | `database.min_conns` | `1` | 连接池保底 |
| `BEACONX_TENANT_DEFAULT_ID` | `tenant.default_id` | `1` | 单租户默认 ID |
| `BEACONX_HOME` | `state.dir` | `data` | 状态目录 |
| `BEACONX_LOG_LEVEL` | `log.level` | `info` | debug/info/warn/error |
| `BEACONX_LOG_FORMAT` | `log.format` | `json` | json/text |
| `BEACONX_MAX_CONCURRENT` | `engine.max_concurrent` | `2` | 同时运行的任务数上限 |
| `BEACONX_PLANNER_INTERVAL` | `engine.planner_interval` | `2s` | 每任务规划轮询间隔 |
| `BEACONX_WORKER_INTERVAL` | `engine.worker_interval` | `200ms` | 每任务领取动作的轮询间隔 |
| `BEACONX_LEASE_TTL` | `engine.lease_ttl` | `30s` | 动作租约时长 |
| `BEACONX_HEARTBEAT_INTERVAL` | `engine.heartbeat_interval` | `10s` | 动作租约续约间隔 |
| `BEACONX_ADMIN_TOKEN` | `auth.admin_token` | 空（启动生成临时令牌） | 管理接口令牌 |
| `BEACONX_LLM_BASE_URL` | `llm.base_url` | 空 | OpenAI 兼容端点根地址 |
| `BEACONX_LLM_API_KEY` | `llm.api_key` | 空 | 模型 API Key；只放环境变量或本地 `.env` |
| `BEACONX_LLM_MODEL` | `llm.model` | 空 | 模型名 |
| `BEACONX_LLM_MAX_TOKENS` | `llm.max_tokens` | `2048` | 单次回复输出上限 |
| `BEACONX_LLM_TIMEOUT` | `llm.timeout` | `60s` | 单次 HTTP 请求超时 |
| `BEACONX_LLM_MAX_RETRIES` | `llm.max_retries` | `2` | 最大重试次数 |
| `BEACONX_POLICY_DEFAULT_ACTION` | `policy.default_action` | `ask` | 无规则命中时的默认裁决 |
| `BEACONX_APPROVAL_TIMEOUT` | `policy.approval_timeout` | `60s` | ask 的默认审批等待超时 |
| `BEACONX_EVIDENCE_DIR` | `evidence.dir` | 空 | 证据根目录；为空时用 `<state.dir>/tenants/<tenant_id>/evidence` |
| `BEACONX_EVIDENCE_KEY` | `evidence.key_hex` | 空 | 64 位十六进制证据主密钥；为空时使用/生成 `<state.dir>/evidence.key` |
| `BEACONX_UPDATE_ENABLED` | `selfupdate.enabled` | `true` | 是否启用自更新（布尔值，非法值启动失败） |
| `BEACONX_UPDATE_REPO` | `selfupdate.repo` | `orzl1/BeaconX` | GitHub `owner/repo`；启用时不能为空 |

环境变量值为空时视为未设置，不会覆盖配置文件或默认值。

启动时会校验配置；非法值直接退出并给出一条可读错误，不做静默降级。

## 任务运行时

`engine` 段控制任务运行时（详见 [运行时与引擎](runtime.md)）：

- `max_concurrent`：同时运行的任务数上限，准入调度每 1s 按 FIFO 补位，默认 `2`。
- `planner_interval`：每个任务的规划轮询间隔，默认 `2s`。
- `worker_interval`：每个任务领取动作的轮询间隔，默认 `200ms`。
- `lease_ttl`：动作租约时长，默认 `30s`；领取动作时写入 `lease_until`，超时未续约的动作会被回收。
- `heartbeat_interval`：执行期租约续约间隔，默认 `10s`。

`engine` 中的非正数取值会在引擎侧回退到上述默认值，而不是导致启动失败。SSE 心跳（20s）与单次回放上限（500 条）目前是代码内默认值，不通过配置暴露。

## 管理鉴权

`auth.admin_token`（或环境变量 `BEACONX_ADMIN_TOKEN`）是任务接口的静态令牌：

- 配置了令牌时直接使用；未配置（或为空）时，进程启动会生成 24 字节随机数（48 位十六进制字符）作为临时令牌，并以 WARN 级别打印到日志（字段名 `token`），只对当前进程有效。
- 客户端优先使用 `Authorization: Bearer <token>`；SSE 端点也可用 `?token=<token>`。两种方式都使用常量时间比较，空令牌一律拒绝。
- 除 `/healthz`、`/readyz`、`/api/v1/system/*` 外，任务接口都需要该令牌；认证组件未装配时受保护路由返回 503（fail closed）。

## LLM 规划

`llm` 段配置 OpenAI 兼容模型端点（详见 [AI 规划与执行](ai.md)）：

- **启用条件**：`base_url` 与 `model` **同时非空**才启用 AI 规划；任一为空时运行时为 idle，使用 `IdlePlanner` / `NoopWorker`。启动日志会打印 `规划模式 mode=idle` 或 `规划模式 mode=llm model=<模型名>`。
- **校验**：启用时由模型客户端校验配置——`base_url` 必须是以 `http://` 或 `https://` 开头、可解析且带主机名的地址；`model` 不能为空。校验失败会记录“初始化模型客户端失败”并终止启动。只配置其中一项不会报错，只会保持 idle。
- **默认值**：`max_tokens=2048`、`timeout=60s`、`max_retries=2`；这些值取 `<=0` 时回退到默认值。`max_retries<=0` 不能关闭重试（会被当作 2）。
- **重试范围**：只有 HTTP 429、5xx 与网络错误会重试；其余 4xx 不重试。JSON 模式请求被服务端以 HTTP 400 拒绝时，客户端会回退为普通请求再试一次。
- **temperature**：可选，只支持配置文件（没有对应环境变量）；未设置时不发送该参数。
- **故障转移**：`llm.fallbacks` 是 JSON 数组（只支持配置文件，没有对应环境变量），主 profile 之后按顺序尝试；每个元素必须同时有 `base_url` 与 `model`，否则启动校验失败。只有 auth（401/403）、quota（402）、rate_limit（429）、transient（5xx/网络/超时）会切换到下一个 profile；context_overflow 与 fatal 立即失败。每次切换写一条 WARN 日志「模型切换」。
- **密钥安全**：`api_key` 应通过环境变量 `BEACONX_LLM_API_KEY` 或本地 `.env` 提供（`.env` 已被忽略），不要写入仓库或配置模板；`api_key` 为空时不会发送 `Authorization` 头（便于本地假服务）。fallback 的 `api_key` 同样只应写在本地配置或环境变量中。
- **工具调用**：配置了 LLM 后，Worker 会装配工具与策略执行器；工具调用链、审批与审计见 [工具与策略](tools-policy.md)。

## 工具策略与审批

`policy` 段控制工具调用的默认裁决与审批等待（详见 [工具与策略](tools-policy.md)）：

- `default_action`：当所有启用规则都未命中时使用的裁决，取值 `allow` / `ask` / `deny`，默认 `ask`（策略缺失即人工确认）。规则命中时以规则动作为准。
- `approval_timeout`：ask 的默认等待超时，默认 `60s`；如果命中规则自带 `timeout_seconds`（内置 ask 规则为 60s），优先使用规则值。
- 实现细节：配置加载阶段会校验 `default_action` 只能是 `allow`/`ask`/`deny`，非法值导致启动失败；`approval_timeout` 必须为正数。执行器内部对零值有兜底默认值，但正常启动路径不依赖它。
- 工具只在启用 LLM（`llm.base_url` 与 `llm.model` 同时非空）后被真实调用；idle 模式不执行工具。

## 证据存储

`evidence` 段控制加密证据库（详见 [证据链](evidence.md)）：

- `dir`：证据根目录，默认 `<state.dir>/tenants/<tenant_id>/evidence`；目录以 0700、blob 文件以 0600 创建。
- `key_hex`：64 位十六进制（32 字节）主密钥；为空时读取或生成 `<state.dir>/evidence.key`（0600）。格式非法会在启动校验阶段失败；密钥不会打印到日志。
- **密钥轮换暂不支持**（M5.1）：更换密钥会让存量 blob 无法解密、快照验签失败（表现为 tampered）。

## 自更新

`selfupdate` 段控制内置自更新（详见 [发布与自更新](release.md)）：

- `enabled`：默认 `true`；关闭后检查/应用接口在检查阶段失败（502 `update_check_failed`，错误信息“自更新已禁用”），回滚接口仍可用（只依赖 `.old` 备份）。503 `update_unavailable` 只会在 HTTP 层未装配 Updater 时出现（正常 `serve` 总会装配）。
- `repo`：GitHub `owner/repo`，默认 `orzl1/BeaconX`；`enabled=true` 时不能为空，否则启动校验失败。
- 自更新会访问 GitHub latest release API 并下载本平台 zip，校验 SHA256SUMS、先跑 `--version` 冒烟再替换二进制；当前二进制备份为 `<binary>.old`，可通过 rollback 接口还原；替换后需重启进程生效。
- **容器部署建议关闭**（`BEACONX_UPDATE_ENABLED=false`）：镜像内二进制归 root 所有且容器会被重建，升级应通过重新拉取镜像。
- GitHub API 对未认证请求有速率限制（约 60 次/小时/IP），频繁检查会失败并返回 `update_check_failed`。

## 状态目录

```
$BEACONX_HOME/
  jwt.key                 登录签名密钥（后续里程碑生成）
  tenants/<id>/sessions/  Agent 会话与执行记录
  tenants/<id>/evidence/  不可变证据快照
  tenants/<id>/traffic/   采集热数据（可丢弃）
  skills/                 技能包
  cache/                  可重建缓存
```

- 目录权限创建为 `0700`；Windows 上以部署文档中的 ACL 指引为准。
- v1 只有一个真实租户，但路径已按租户分区，后续开启多租户不需要搬迁文件。

## 数据库与迁移

- 迁移文件在 `migrations/`，命名 `NNNN_name.sql`，随二进制嵌入。
- 启动时（`serve`）或显式执行 `beaconx migrate` 时应用：先获取会话级咨询锁，再逐个在事务中执行未应用的版本，并记录校验和。
- **已发布的迁移不可修改**：校验和不匹配会拒绝启动，避免「代码与库结构悄悄分叉」。
- 迁移失败会终止启动；不存在「尽力而为」模式。

## 探针

| 端点 | 用途 | 判定 |
| --- | --- | --- |
| `/healthz` | 存活 | 进程可响应即 200 |
| `/readyz` | 就绪 | 数据库可连接且 schema 版本不低于二进制内置版本；否则 503 |

容器镜像的 `HEALTHCHECK` 使用 `/readyz`。

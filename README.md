# BeaconX

[![ci](https://github.com/orzl1/BeaconX/actions/workflows/ci.yml/badge.svg)](https://github.com/orzl1/BeaconX/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

自主安全测试平台。目标是在**可控授权范围**内，用多 Agent 完成从目标分解、侦察、验证到证据留存的完整流程；人负责审批与复核，而不是逐条输入命令。

> **当前状态：M6（前端与交付，已完成）**。M1–M5 已建成数据层、任务运行时、AI 规划、工具策略与证据链；M6 补齐 Next.js 静态控制台（`go:embed` 同源托管）、发布流水线（前端构建 + 5 平台产物 + SHA256SUMS）、自更新（检查/下载/校验/冒烟/回滚）与多阶段镜像。浏览器 e2e、URL 路由、CORS、发布签名与 Windows 自更新列入 M6.1，里程碑见 [路线图](docs/roadmap.md)。**M6 仍不是生产级产品**，请勿用于真实目标。

## 项目文档（强制同步）

- **[docs/project-map.md](docs/project-map.md) 是项目代码功能结构的唯一真源**：代码地图、数据模型、API 全表、状态机、运行时机制、配置、前端对接指南与缺口路线。
- **任何改动都必须同步更新它**（对应章节 + 文末「变更记录」），并在提交前运行 `make docs-check`；规则见 [AGENTS.md](AGENTS.md) 与 [CONTRIBUTING.md](CONTRIBUTING.md)。
- 文档当前含：**已实现 146 条 API（基础 70 + feature 76；✅）**；B8 清理后的剩余项统一列在「已知设计限制」（PDF 字体/图片、edges 5000、at-most-once、rotate 窗口、Agents 选择/热更新等）。
- 前端设计请以该文档的「API 全表」「前端对接指南」「原版页面映射与缺口」三章为依据。

## 快速开始

前置：Docker（含 Compose）。

```bash
docker compose up -d --build
open http://localhost:6969/            # Web 控制台（镜像已内含前端）
open http://localhost:6969/healthz     # {"status":"ok"}
curl -s http://localhost:6969/readyz   # 就绪探针（含数据库与迁移版本）
curl -s http://localhost:6969/api/v1/system/info
docker compose logs beaconx | grep 临时管理令牌   # 未配置 BEACONX_ADMIN_TOKEN 时
```

停止：

```bash
docker compose down          # 保留数据
docker compose down -v       # 连数据一起删除
```

## 本地开发

前置：Go 1.24+、一个可用的 PostgreSQL 16；构建前端还需要 Node.js ≥ 20.9 与 npm ≥ 10。

```bash
cp .env.example .env          # 按需修改 BEACONX_DB_DSN、BEACONX_ADMIN_TOKEN、LLM、策略、证据与自更新配置
set -a && . ./.env && set +a

make build
./bin/beaconx migrate         # 应用迁移（启动时也会自动应用）
./bin/beaconx serve           # 默认监听 :6969

# 可选：构建并嵌入前端（需要 Node.js）；未构建时 / 显示占位页
make web && make build
```

前端单独开发时：

```bash
cd web && npm ci && npm run dev   # http://localhost:3000，/api 反代到 127.0.0.1:6969
```

常用命令：

```bash
make test          # 单元测试
make test-race     # 竞态检测
make lint          # gofmt + go vet + 品牌门禁
make build         # 产出 bin/beaconx
make web           # 构建前端并复制到 internal/webui/dist
make docker-build  # 构建含前端的镜像
```

数据库集成测试需要真实 PostgreSQL：设置 `BEACONX_TEST_PG_DSN` 后运行 `go test -count=1 ./...`；未设置时相关测试自动跳过。

## 配置

优先级：**环境变量 > 配置文件 > 内置默认值**。配置文件默认 `./config.json`（可用 `BEACONX_CONFIG` 指定），结构见 [config.example.json](config.example.json)；完整说明见 [docs/configuration.md](docs/configuration.md)。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `BEACONX_DB_DSN` | 空（必填） | PostgreSQL 连接串 |
| `BEACONX_DB_MAX_CONNS` | `10` | 连接池上限 |
| `BEACONX_DB_MIN_CONNS` | `1` | 连接池保底 |
| `BEACONX_ADDR` | `:6969` | HTTP 监听地址 |
| `BEACONX_REQUEST_TIMEOUT` | `30s` | 请求上下文超时 |
| `BEACONX_SHUTDOWN_TIMEOUT` | `15s` | 优雅退出等待上限 |
| `BEACONX_HOME` | `data` | 状态目录（会话/证据/流量/技能） |
| `BEACONX_TENANT_DEFAULT_ID` | `1` | 单租户默认 ID（多租户从第一天预留） |
| `BEACONX_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `BEACONX_LOG_FORMAT` | `json` | `json` / `text` |
| `BEACONX_CONFIG` | `config.json` | 配置文件路径 |
| `BEACONX_MAX_CONCURRENT` | `2` | 同时运行的任务数上限 |
| `BEACONX_PLANNER_INTERVAL` | `2s` | 每任务规划轮询间隔 |
| `BEACONX_WORKER_INTERVAL` | `200ms` | 每任务领取动作的轮询间隔 |
| `BEACONX_LEASE_TTL` | `30s` | 动作租约时长 |
| `BEACONX_HEARTBEAT_INTERVAL` | `10s` | 动作租约续约间隔 |
| `BEACONX_ADMIN_TOKEN` | 空（启动生成临时令牌） | 管理接口令牌 |
| `BEACONX_LLM_BASE_URL` | 空 | OpenAI 兼容端点（如 `https://api.deepseek.com`） |
| `BEACONX_LLM_API_KEY` | 空 | 模型 API Key；只放环境变量或本地 `.env`，不要提交 |
| `BEACONX_LLM_MODEL` | 空 | 模型名（如 `deepseek-flash`） |
| `BEACONX_LLM_MAX_TOKENS` | `2048` | 单次回复输出上限 |
| `BEACONX_LLM_TIMEOUT` | `60s` | 单次 HTTP 请求超时 |
| `BEACONX_LLM_MAX_RETRIES` | `2` | 最大重试次数（仅 429 / 5xx / 网络错误） |
| `BEACONX_POLICY_DEFAULT_ACTION` | `ask` | 无规则命中时的默认裁决（allow / ask / deny） |
| `BEACONX_APPROVAL_TIMEOUT` | `60s` | ask 的默认审批等待超时 |
| `BEACONX_EVIDENCE_DIR` | 空 | 证据根目录；为空时用 `<状态目录>/tenants/<tenant_id>/evidence` |
| `BEACONX_EVIDENCE_KEY` | 空 | 64 位十六进制证据主密钥；为空时使用/生成 `<状态目录>/evidence.key` |
| `BEACONX_UPDATE_ENABLED` | `true` | 是否启用自更新；容器部署建议设为 `false` |
| `BEACONX_UPDATE_REPO` | `orzl1/BeaconX` | 自更新使用的 GitHub `owner/repo` |

`llm.temperature` 与 `llm.fallbacks` 只能写在配置文件里，没有对应环境变量：`llm.fallbacks` 是故障转移 JSON 数组（元素含 `base_url` / `api_key` / `model`，密钥只应放在本地配置或环境变量中）。

`llm.base_url` 与 `llm.model` **同时非空**才会启用 AI 规划；否则运行时为 idle（`IdlePlanner` / `NoopWorker`），日志打印 `规划模式 mode=idle`。启用后日志为 `规划模式 mode=llm model=<模型名>`，详见 [AI 规划与执行](docs/ai.md)。工具与策略只在 AI 启用后被真实调用。

token 预算是软上限：`spent_tokens` 超过 `max_tokens` 时，引擎会把任务置为 `paused`（事件 reason=「模型 token 预算超限暂停」），而不是判为失败；确认或调整预算后可用 `resume` 重新排队继续。

## HTTP 接口（M6.2-B4/B5/B6/B7/B8）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 存活探针 |
| GET | `/readyz` | 就绪探针：数据库连通 + 迁移已应用 |
| GET | `/api/v1/system/info` | 版本、构建信息、schema 版本、运行时长 |
| GET | `/api/v1/system/tenant` | 当前请求解析到的租户（验证租户上下文） |
| POST | `/api/v1/missions` | 创建任务（可带预算，见下方示例） |
| GET | `/api/v1/missions` | 任务列表（`?status=`、`?limit=`，默认 50） |
| GET | `/api/v1/missions/{id}` | 任务详情 |
| POST | `/api/v1/missions/{id}/pause` | 暂停运行中的任务 |
| POST | `/api/v1/missions/{id}/resume` | 恢复已暂停的任务（重新排队） |
| POST | `/api/v1/missions/{id}/cancel` | 取消任务 |
| DELETE | `/api/v1/missions/{id}` | 删除任务（级联删除数据，返回 204） |
| GET | `/api/v1/missions/{id}/events` | 任务事件流（SSE，支持 Last-Event-ID / `?since=`） |
| GET | `/api/v1/missions/{id}/audit` | 任务工具调用审计（按 id 倒序，最多 200 条） |
| GET | `/api/v1/missions/{id}/traffic` | 任务流量采集记录（按 id 倒序，最多 200 条） |
| GET | `/api/v1/approvals` | 审批列表（`?status=`、`?limit=`，默认 50） |
| POST | `/api/v1/approvals/{id}/decide` | 审批决策（`{"approve":true|false,"note":"…"}`） |
| POST | `/api/v1/traffic/{id}/evidence` | 把已完成的采集记录冻结为证据快照（201） |
| GET | `/api/v1/evidence/{id}/verify` | 校验证据签名与正文哈希（篡改返回 409 tampered） |
| GET | `/api/v1/companies` | 企业列表（`?q=` 名称/slug 模糊、`?limit=` 默认 50/上限 200） |
| POST | `/api/v1/companies` | 创建企业（`{"name","slug"}`，slug 冲突 409） |
| GET | `/api/v1/companies/{id}` | 企业详情 |
| PATCH | `/api/v1/companies/{id}` | 更新企业（`name`/`slug` 可选，空值保持不变） |
| DELETE | `/api/v1/companies/{id}` | 删除企业（级联删除范围，返回 204） |
| GET | `/api/v1/companies/{id}/scope` | 企业资产范围列表 |
| POST | `/api/v1/companies/{id}/scope` | 新增企业范围（`kind` 白名单，重复 409） |
| DELETE | `/api/v1/companies/{id}/scope/{scope_id}` | 删除企业范围（返回 204） |
| GET | `/api/v1/assets` | 资产列表（`?type=` 精确、`?q=` 标签模糊、`?limit=`） |
| POST | `/api/v1/assets` | 创建资产（带 `identities` 走 upsert，省略则建裸资产，201） |
| GET | `/api/v1/assets/{id}` | 资产详情（含身份键） |
| PATCH | `/api/v1/assets/{id}` | 更新资产（`label`/`parent_id`/`attrs` 可选，`parent_id:null` 清空） |
| DELETE | `/api/v1/assets/{id}` | 删除资产（级联身份与任务关联，子资产 parent 置空，204） |
| GET | `/api/v1/assets/{id}/identities` | 资产身份键列表 |
| GET | `/api/v1/missions/{id}/assets` | 任务已登记资产 |
| POST | `/api/v1/missions/{id}/assets` | 把资产登记到任务（`{"asset_id","source"?}`，重复 409） |
| DELETE | `/api/v1/missions/{id}/assets/{asset_id}` | 解除任务资产关联（返回 204） |
| GET | `/api/v1/missions/{id}/coverage` | 任务资产覆盖度（空任务返回全零，200） |
| GET | `/api/v1/missions/{id}/scope` | 任务范围列表（按 id 升序） |
| POST | `/api/v1/missions/{id}/scope` | 新增任务范围（`kind` 六种，重复 409，201 返回条目） |
| DELETE | `/api/v1/missions/{id}/scope/{scope_id}` | 删除任务范围（返回 204） |
| GET | `/api/v1/missions/{id}/plan` | 计划步骤（空任务返回 `steps:[]`） |
| GET | `/api/v1/missions/{id}/graph` | 作战图 `{nodes,edges,anchors}`（`?kind=&state=&limit=`，默认 500/上限 2000） |
| GET | `/api/v1/nodes/{id}` | 作战图节点详情 |
| PATCH | `/api/v1/nodes/{id}/state` | 更新节点状态（非法状态 400，kind 规则不满足 400） |
| GET | `/api/v1/missions/{id}/findings` | 发现列表（`?severity=`、`?status=`、`?limit=`） |
| POST | `/api/v1/missions/{id}/findings` | 创建发现（201，初始状态 `open`） |
| GET | `/api/v1/findings/{id}` | 发现详情（平铺字段 + `evidence:[]`） |
| PATCH | `/api/v1/findings/{id}` | 更新发现（`title`/`severity`/`detail`/`asset_id` 三态） |
| POST | `/api/v1/findings/{id}/status` | 发现状态流转（非法迁移 409） |
| GET | `/api/v1/findings/{id}/evidence` | 发现绑定的证据快照列表 |
| POST | `/api/v1/findings/{id}/evidence` | 绑定证据快照（201，重复绑定 409） |
| DELETE | `/api/v1/findings/{id}/evidence/{snapshot_id}` | 解除证据绑定（返回 204） |
| GET | `/api/v1/missions/{id}/retests` | 复测列表（按创建时间倒序） |
| POST | `/api/v1/missions/{id}/retests` | 发起复测（201，初始 `pending`） |
| GET | `/api/v1/retests/{id}` | 复测详情 |
| PATCH | `/api/v1/retests/{id}` | 更新复测结果（非法迁移 409） |
| GET | `/api/v1/missions/{id}/report` | 生成任务 Markdown 报告（`text/markdown; charset=utf-8`） |
| GET | `/api/v1/traffic` | 全局流量采集列表（`?mission_id=&tool=&phase=&limit=`，id 倒序，默认 50/上限 200） |
| GET | `/api/v1/traffic/{id}` | 流量采集详情（不含正文） |
| GET | `/api/v1/traffic/{id}/request` | 解密后的请求正文（`text/plain; charset=utf-8`；篡改 409 tampered） |
| GET | `/api/v1/traffic/{id}/response` | 解密后的响应正文（`text/plain; charset=utf-8`；篡改 409 tampered） |
| DELETE | `/api/v1/traffic/{id}` | 删除采集记录（仅删行，正文与已冻结证据保留，204） |
| GET | `/api/v1/commands` | 命令执行历史（audit_events 视图，`?mission_id=&tool=&outcome=&limit=`，默认 50/上限 200） |
| GET | `/api/v1/commands/{id}` | 命令执行详情 |
| GET | `/api/v1/llm-records` | LLM 调用记录（`?mission_id=&role=&model=&limit=`，默认 50/上限 500） |
| GET | `/api/v1/llm-records/{id}` | LLM 调用详情（不含正文；字节数/truncated/redacted/usage） |
| GET | `/api/v1/llm-records/{id}/request` | LLM 请求正文（application/json，已脱敏 messages） |
| GET | `/api/v1/llm-records/{id}/response` | LLM 响应正文（text/plain，已脱敏响应/错误） |
| GET | `/api/v1/missions/{id}/usage` | 任务用量：预算 + 全部调用记录 + 按角色/模型聚合 |
| GET | `/api/v1/llm-usage/summary` | 用量聚合（`?group_by=model|role|day&mission_id=&days=`，默认 30 天/上限 365） |
| GET | `/api/v1/dashboard` | Dashboard 聚合计数（任务/用量/审批/发现/资产/流量/最近任务） |
| GET | `/api/v1/system/update/check` | 检查 GitHub Release 新版本 |
| POST | `/api/v1/system/update/apply` | 下载、校验、冒烟并替换二进制（需重启生效） |
| POST | `/api/v1/system/update/rollback` | 用 `<binary>.old` 回滚上一版本（需重启生效） |
| GET | `/api/v1/logs` | 应用日志查询（?level=&limit=） |
| GET | `/api/v1/logs/stream` | 应用日志实时流（SSE，event: log） |
| GET | `/api/v1/settings` | 运行时设置（持久化+可读，B7 起热生效） |
| PATCH | `/api/v1/settings` | 更新运行时设置（白名单键；即时热生效；运行中任务预算不回溯） |
| GET | `/api/v1/evidence/{id}/audit` | 证据读取审计（🟡 仅部分动作埋点）（🟡 部分实现） |
| POST | `/api/v1/evidence/{id}/export` | 导出证据 zip（snapshot.json + 正文） |
| POST | `/api/v1/evidence/rotate` | 证据密钥轮换（200；新密钥不落盘，存量快照签名失效需离线重签） |
| GET | `/api/v1/findings` | 跨任务发现列表（?mission_id=&severity=&status=&asset_id=&after_id=&limit=） |
| GET | `/api/v1/findings/export` | 导出发现为 CSV（≤10000 行，防公式注入） |
| POST | `/api/v1/retests/{id}/run` | 执行复测（校验证据签名与解密，非重跑 exploit） |
| GET | `/api/v1/retests/{id}/evidence` | 复测关联证据列表 |
| POST | `/api/v1/missions/{id}/findings/from-node` | 从 fact/finding 节点创建发现（0016 部分唯一索引防重复；已绑定→409） |
| GET | `/api/v1/mcp/servers` | MCP 端点列表 |
| POST | `/api/v1/mcp/servers` | 新建 MCP 端点 |
| GET | `/api/v1/mcp/servers/{id}` | MCP 端点详情 |
| PATCH | `/api/v1/mcp/servers/{id}` | 更新 MCP 端点 |
| DELETE | `/api/v1/mcp/servers/{id}` | 删除 MCP 端点 |
| POST | `/api/v1/mcp/servers/{id}/refresh` | 刷新 MCP 工具清单（http/stdio；失败保留旧清单） |
| GET | `/api/v1/mcp/tools` | MCP 工具列表（?server_id=） |
| GET | `/api/v1/workspace/files` | 工作区文件列表（?mission_id=&path=&limit=） |
| GET | `/api/v1/workspace/files/{path...}` | 读取工作区文本文件（≤1MB UTF-8） |
| PUT | `/api/v1/workspace/files/{path...}` | 写入工作区文本文件（≤1MB） |
| POST | `/api/v1/workspace/upload` | 上传文件到工作区（≤10MB） |
| DELETE | `/api/v1/workspace/files/{path...}` | 删除工作区文件/目录（非空目录需 recursive） |
| GET | `/api/v1/traffic/{id}/export` | 导出流量为合成 HAR 1.2 |
| POST | `/api/v1/traffic/{id}/replay` | 重放请求（走 executor 策略门/审批/审计/证据；traffic_id best-effort） |
| DELETE | `/api/v1/missions/{id}/traffic` | 批量清理任务采集行（保留证据快照/正文） |
| GET | `/api/v1/notifications/channels` | 通知渠道列表 |
| POST | `/api/v1/notifications/channels` | 新建通知渠道 |
| GET | `/api/v1/notifications/channels/{id}` | 通知渠道详情 |
| PATCH | `/api/v1/notifications/channels/{id}` | 更新通知渠道（type 不可改） |
| DELETE | `/api/v1/notifications/channels/{id}` | 删除通知渠道（级联投递记录） |
| POST | `/api/v1/notifications/channels/{id}/test` | 测试渠道（一律 200 + delivery；webhook 真发，email/generic skipped）（🟡 部分实现） |
| GET | `/api/v1/notifications/deliveries` | 投递记录（?channel_id=&status=&limit=） |
| GET | `/api/v1/sync/sources` | 同步数据源列表 |
| POST | `/api/v1/sync/sources` | 新建同步数据源 |
| GET | `/api/v1/sync/sources/{id}` | 数据源详情 |
| PATCH | `/api/v1/sync/sources/{id}` | 更新数据源 |
| DELETE | `/api/v1/sync/sources/{id}` | 删除数据源 |
| GET | `/api/v1/sync/projects` | 外部项目列表（?source_id=） |
| POST | `/api/v1/sync/projects` | 新建/更新外部项目 |
| POST | `/api/v1/sync/run` | 触发同步（202 + running，异步 ≤60s） |
| GET | `/api/v1/sync/runs` | 同步运行记录（?source_id=&status=&limit=） |
| POST | `/api/v1/assets/{id}/identities` | 新增资产身份键（Normalize 后写入） |
| DELETE | `/api/v1/assets/{id}/identities/{kind}/{key...}` | 删除资产身份键（允许删到 0 条） |
| POST | `/api/v1/missions/{id}/scope/compile` | 把任务范围编译为策略草案（纯预览，不落库） |
| GET | `/api/v1/policy/rules` | 策略规则列表（含停用；priority DESC,id ASC） |
| POST | `/api/v1/policy/rules` | 创建策略规则（不可创建 immutable） |
| GET | `/api/v1/policy/rules/{id}` | 策略规则详情 |
| PATCH | `/api/v1/policy/rules/{id}` | 更新策略规则（immutable 限制；arg_pattern 仍可改 🟡）（🟡 部分实现） |
| DELETE | `/api/v1/policy/rules/{id}` | 删除策略规则（immutable 拒绝） |
| POST | `/api/v1/policy/simulate` | 策略裁决试算（启用规则前 200） |
| GET | `/api/v1/approvals/{id}` | 审批详情与上下文回放 |
| POST | `/api/v1/approvals/{id}/cancel` | 取消待审批项（仅 pending） |
| GET | `/api/v1/agents` | Agent 定义列表 |
| POST | `/api/v1/agents` | 创建 Agent 定义 |
| GET | `/api/v1/agents/{key}` | Agent 详情 |
| PATCH | `/api/v1/agents/{key}` | 更新 Agent 定义 |
| DELETE | `/api/v1/agents/{key}` | 删除 Agent 定义 |
| POST | `/api/v1/agents/{key}/test` | 校验 Agent 工具/提示词（不调 LLM；🟡 未接入 engine）（🟡 部分实现） |
| GET | `/api/v1/skills` | 技能列表（含缺失统计） |
| POST | `/api/v1/skills` | 创建技能元数据 |
| GET | `/api/v1/skills/{name}` | 技能详情 |
| PATCH | `/api/v1/skills/{name}` | 更新技能元数据 |
| DELETE | `/api/v1/skills/{name}` | 删除技能（含磁盘文件） |
| POST | `/api/v1/skills/upload` | 上传技能 zip（≤10MB/≤200 文件/仅文本） |
| GET | `/api/v1/skills/{name}/files` | 技能文件树 |
| GET | `/api/v1/skills/{name}/files/{path...}` | 读取技能文本文件（≤1MB） |
| PUT | `/api/v1/skills/{name}/files/{path...}` | 写入技能文本文件 |
| DELETE | `/api/v1/skills/{name}/files/{path...}` | 删除技能文件 |
| GET | `/api/v1/activity` | 全局活动流（`?limit=&before_id=&mission_id=&type=`，mission_events 倒序，默认 50/上限 200） |
| GET | `/api/v1/skills/{name}/versions` | 技能版本历史（保留最近 5 份快照） |
| POST | `/api/v1/skills/{name}/versions/{timestamp}/restore` | 恢复技能文件到指定版本（先给当前状态打快照） |

除 `/healthz`、`/readyz`、`/api/v1/system/*` 外，所有接口都需要 `Authorization: Bearer <BEACONX_ADMIN_TOKEN>`；浏览器 EventSource 无法自定义请求头，SSE 端点也可用 `?token=<token>` 携带令牌。未配置令牌时，进程启动会生成临时令牌并打印到日志（仅当前进程有效）。`/api/v1/system/update/*` 虽然属于 system 前缀，但仍需要令牌（`Routes()` 中未标记为 Public）。

创建任务时可传入预算字段（省略或 0 表示不限制）：

```bash
curl -s -X POST http://localhost:6969/api/v1/missions \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"示例任务","goal":"确认 example.com 可达并给出结论","max_tokens":20000,"max_steps":10}'
```

任务执行时，AI Worker 的每次工具调用都会经过策略门：命中 `ask` 需要人工审批，可通过审批接口查看与决策，例如：

```bash
curl -s "http://localhost:6969/api/v1/approvals?status=pending" \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"
curl -s -X POST http://localhost:6969/api/v1/approvals/1/decide \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"approve":true,"note":"已确认目标与范围"}'
```

工具真正执行时会先做两阶段流量采集（intent → result，参数与输出脱敏后加密落盘），可把完成的采集冻结为证据并校验：

```bash
curl -s "http://localhost:6969/api/v1/missions/1/traffic" \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"
curl -s -X POST "http://localhost:6969/api/v1/traffic/1/evidence" \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"
curl -s "http://localhost:6969/api/v1/evidence/1/verify" \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"
```

证据格式、签名与篡改校验见 [证据链](docs/evidence.md)。

## Web 控制台

- 镜像与发布包已内含前端（`go:embed` 同源托管），浏览器访问 `http://localhost:6969/` 即可；本地开发未执行 `make web` 时显示占位页。
- 令牌保存在本机 `localStorage`；普通请求用 Bearer，SSE 用 `?token=`。页面包含任务（列表/创建/状态操作）、任务详情（SSE 14 类事件、流量→冻结证据→校验）、审批与系统（探针/自更新）三区。
- 单页无 URL 路由、事件只保留最近 200 条；浏览器 e2e、CORS、URL 路由在 M6.1。详见 [Web 控制台](docs/web.md)。

## 发布与自更新

- 发布：推送 `v*` tag 触发 `.github/workflows/release.yml`，先构建前端，再交叉编译 5 个平台、打包 `beaconx-<goos>-<goarch>.zip`、生成 `SHA256SUMS` 并创建 GitHub Release。
- 自更新：`GET /api/v1/system/update/check` 检查最新 Release；`POST .../apply` 下载本平台包，校验 SHA256SUMS、跑 `--version` 冒烟后替换二进制（备份 `<binary>.old`，写 `update.json`）；`POST .../rollback` 还原。替换后需**重启进程**生效。
- 配置：`selfupdate.enabled`（默认 true）、`selfupdate.repo`（默认 `orzl1/BeaconX`）；容器部署建议设置 `BEACONX_UPDATE_ENABLED=false`，通过重新拉取镜像升级。
- 发布包只有 SHA256 校验和（无签名）；Windows 自更新未验证。详见 [发布与自更新](docs/release.md)。

接口契约以 [api/openapi.yaml](api/openapi.yaml) 为准，代码功能结构以 [docs/project-map.md](docs/project-map.md) 为唯一真源；路由表在 `internal/httpapi` 中声明式维护，三者需同步演进（`make docs-check` 会校验文档覆盖全部路由）。

## 目录结构

```
cmd/beaconx/            进程入口（serve / migrate / healthcheck / version）
web/                    Next.js 静态控制台（构建产物嵌入二进制）
internal/webui/         go:embed 前端托管（静态回退、占位页、/api JSON 404）
internal/config/        配置加载（文件 + 环境变量 + 默认值）
internal/logging/       结构化日志
internal/platform/      状态目录（按租户分区）
internal/tenant/        租户上下文
internal/httpapi/       声明式路由 + 中间件 + 处理器（含 SSE、审批、审计、证据与自更新）
internal/app/ai/        LLM 规划器与执行器（含工具循环）
internal/app/events/    进程内事件总线（SSE 分发、慢消费者丢弃）
internal/app/evidence/  证据存储（加密 blob、快照签名、篡改校验、脱敏）
internal/app/executor/  工具调用唯一入口（策略判定、审批等待、采集、审计）
internal/app/runner/    任务运行时（准入调度、规划/执行、租约、预算暂停、恢复）
internal/app/system/    进程与依赖状态
internal/app/tools/     工具注册表与内置工具（http_probe / shell_exec）
internal/domain/        领域模型（资产身份、作战图、任务状态机、计划决策、策略规则）
internal/infra/llm/     OpenAI 兼容客户端（工具协议、错误分类、故障转移路由）
internal/infra/pg/      PostgreSQL 连接池 + 资产/任务/计划/策略/证据 Store
internal/infra/selfupdate/ 自更新（检查/下载/校验/冒烟/备份/回滚）
internal/infra/migrate/ 版本化迁移（校验和 + 咨询锁）
migrations/             嵌入式 SQL 迁移
api/                    OpenAPI 契约
scripts/                CI 辅助脚本（品牌门禁等）
docs/                   项目真源（project-map）、术语、配置、路线图、数据模型、运行时、AI、工具策略、证据、Web、发布
```

## 工程约定

- **版本化迁移**：`migrations/NNNN_*.sql` 只增不改；启动时只应用未执行版本，校验和不匹配即拒绝启动。
- **租户预留**：所有业务表以 `tenant_id` 打头的主键/唯一键；v1 单租户，但目录、上下文与 store 均已按租户分区。
- **可移植**：纯 Go、`CGO_ENABLED=0`；CI 对 Linux/macOS/Windows 交叉编译，Windows 为后续版本预留。
- **默认失败关闭**：需要认证的路由在认证组件未装配时返回 503，而不是放行。
- **AI 默认关闭**：未配置 LLM 端点时使用空规划器/执行器；配置后才启用 LLM 规划，密钥只从环境变量或本地 `.env` 读取。
- **工具调用唯一入口**：所有工具调用必须经 `internal/app/executor`（策略 → 审批 → 采集 → 执行 → 审计），不允许 Agent 直接调用工具。
- **证据不可篡改**：正文加密落盘并按内容哈希寻址，快照带 HMAC 签名；校验失败一律返回 409 tampered，不覆盖疑似被篡改的数据。
- **前端嵌入二进制**：前端是纯静态目录，构建后 `go:embed` 进二进制，运行时不需要 Node；未构建时显示占位页。
- **自更新只做 SHA256**：发布包没有签名，自更新仅校验 SHA256SUMS 并做 `--version` 冒烟；失败不改当前二进制，支持 `.old` 回滚。
- **发现/复测状态迁移**：发现与复测的状态机默认拒绝；非法迁移返回 409，同状态不算迁移，终态不可再变。
- **洞察接口**：流量正文解密失败统一 409 `tampered`；B3 的 traffic/commands/llm-records 对非法 `limit` 返回 400，超上限钳制而非报错。
- **B4/B5 分页**：assets/findings/traffic/commands/llm-records 支持 `after_id`/`next_after_id`（assets 升序 id>，其余倒序 id<）；不传时旧行为不变。
- **日志与通知**：日志为进程级环形缓冲（2000）且无 after_id；通知 test 一律 200 + delivery（webhook 真发、email/generic skipped），无真实事件推送/SSRF 白名单（🟡）。
- **文件与同步**：Skills/Workspace 走沙箱路径校验与大小上限；Sync 默认拒私网、异步 ≤60s、逐条容错 skipped。
- **高风险能力**：流量重放已改走 executor（策略门/审批/审计/证据采集）；证据密钥轮换已实现但**存量快照签名会失效**，需离线重签；报告 PDF 依赖阅读器 CJK 字体。
- **B6 硬化**：证据轮换需更新 `BEACONX_EVIDENCE_KEY`/evidence.key 并重启；Workspace 默认 100MB 配额（`BEACONX_WORKSPACE_QUOTA_MB`）；MCP stdio 默认禁用、命令白名单/`BEACONX_MCP_STDIO_DIR`；Skills 版本保留最近 5 份；B7 起 restore 同时回滚 files+meta（旧快照给 warning）。
- **B7 收口**：settings PATCH 即时热生效（运行中任务预算不回溯）；证据轮换后自动重签快照（短暂 Verify 409 窗口）；LLM 正文按 6 个调用点采集、脱敏+64KB 截断，`BEACONX_LLM_CAPTURE` 默认 true；通知 webhook 默认拒私网、`BEACONX_NOTIFY_ALLOW_PRIVATE` 可放开；sync 终态按 `BEACONX_SYNC_RETENTION_DAYS` 清理；workspace 配额用 pg advisory lock 跨进程。
- **B8 收口**：通知敏感 config 加密存储（响应掩码）、后台事件推送 at-most-once（停机不补推）；证据审计补 create/rotate；rotate 全租户重签并重加密通知配置；启动维护按 `BEACONX_LLM_RETENTION_DAYS`（默认 7）清理 LLM 正文、sync pending>15min→failed；Agents 接入 Planner/Worker；graph 过滤下推（edges 仍 5000 上限）；Skills 保留数 `BEACONX_SKILLS_KEEP_VERSIONS`。
- **品牌门禁**：`scripts/check-brand.sh` 在 CI 中阻止第三方项目标识进入代码、依赖与二进制。

## 文档

- [路线图](docs/roadmap.md) — M0–M6 里程碑、M4.1/M5.1/M6.1 延期项与 v2 方向
- [Web 控制台](docs/web.md) — 前端架构、开发/构建、功能范围与已知限制
- [发布与自更新](docs/release.md) — 发布流水线、资产命名、自更新/回滚、镜像交付
- [运行时与引擎](docs/runtime.md) — 状态机、调度、租约、预算暂停、恢复、SSE 与鉴权
- [AI 规划与执行](docs/ai.md) — LLM 客户端、计划模型、规划/执行流程、预算、路由与工具循环
- [工具与策略](docs/tools-policy.md) — 工具注册表、策略规则、审批、审计与安全盲区
- [证据链](docs/evidence.md) — 两阶段采集、加密正文、快照签名、篡改校验与脱敏
- [数据模型](docs/data-model.md) — M1–M5 表结构、资产身份、作战图、覆盖度、事件流与证据表
- [术语表](docs/glossary.md) — 产品与代码统一用词
- [配置说明](docs/configuration.md) — 配置项、运行时参数、LLM、策略、证据、自更新、状态目录与迁移机制

## 许可

本项目采用 [Apache License 2.0](LICENSE) 许可。你可以自由使用、修改与分发，包括商业用途；分发时需保留许可证与版权声明，并遵守其中的专利与商标条款。

Copyright 2026 The BeaconX Authors

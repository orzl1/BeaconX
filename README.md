# BeaconX

[![ci](https://github.com/orzl1/BeaconX/actions/workflows/ci.yml/badge.svg)](https://github.com/orzl1/BeaconX/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

自主安全测试平台。目标是在**可控授权范围**内，用多 Agent 完成从目标分解、侦察、验证到证据留存的完整流程；人负责审批与复核，而不是逐条输入命令。

> **当前状态：M6（前端与交付，已完成）**。M1–M5 已建成数据层、任务运行时、AI 规划、工具策略与证据链；M6 补齐 Next.js 静态控制台（`go:embed` 同源托管）、发布流水线（前端构建 + 5 平台产物 + SHA256SUMS）、自更新（检查/下载/校验/冒烟/回滚）与多阶段镜像。浏览器 e2e、URL 路由、CORS、发布签名与 Windows 自更新列入 M6.1，里程碑见 [路线图](docs/roadmap.md)。**M6 仍不是生产级产品**，请勿用于真实目标。

## 项目文档（强制同步）

- **[docs/project-map.md](docs/project-map.md) 是项目代码功能结构的唯一真源**：代码地图、数据模型、API 全表、状态机、运行时机制、配置、前端对接指南与缺口路线。
- **任何改动都必须同步更新它**（对应章节 + 文末「变更记录」），并在提交前运行 `make docs-check`；规则见 [AGENTS.md](AGENTS.md) 与 [CONTRIBUTING.md](CONTRIBUTING.md)。
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

## HTTP 接口（M6 / M6.2-B1）

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
| GET | `/api/v1/system/update/check` | 检查 GitHub Release 新版本 |
| POST | `/api/v1/system/update/apply` | 下载、校验、冒烟并替换二进制（需重启生效） |
| POST | `/api/v1/system/update/rollback` | 用 `<binary>.old` 回滚上一版本（需重启生效） |

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

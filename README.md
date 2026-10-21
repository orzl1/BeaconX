# BeaconX

[![ci](https://github.com/orzl1/BeaconX/actions/workflows/ci.yml/badge.svg)](https://github.com/orzl1/BeaconX/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

自主安全测试平台。目标是在**可控授权范围**内，用多 Agent 完成从目标分解、侦察、验证到证据留存的完整流程；人负责审批与复核，而不是逐条输入命令。

> **当前状态：M2（运行时与引擎，已完成）**。进程、声明式 HTTP 层、版本化迁移、租户预留与 CI 已就绪；M1 建成数据层与图模型；M2 补齐任务状态机、准入调度、规划/执行循环、动作租约、崩溃恢复、SSE 事件流与管理令牌鉴权。下一阶段为 M3（规划与执行），里程碑见 [路线图](docs/roadmap.md)。**M2 仍不是可用产品**：默认规划器与执行器为空实现，请勿用于真实目标。

## 快速开始

前置：Docker（含 Compose）。

```bash
docker compose up -d --build
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

前置：Go 1.24+、一个可用的 PostgreSQL 16。

```bash
cp .env.example .env          # 按需修改 BEACONX_DB_DSN 与 BEACONX_ADMIN_TOKEN
set -a && . ./.env && set +a

make build
./bin/beaconx migrate         # 应用迁移（启动时也会自动应用）
./bin/beaconx serve           # 默认监听 :6969
```

常用命令：

```bash
make test          # 单元测试
make test-race     # 竞态检测
make lint          # gofmt + go vet + 品牌门禁
make build         # 产出 bin/beaconx
make docker-build  # 构建镜像
```

数据库集成测试需要真实 PostgreSQL：设置 `BEACONX_TEST_PG_DSN` 后运行 `go test -count=1 ./...`；未设置时相关测试自动跳过。

## 配置

优先级：**环境变量 > 配置文件 > 内置默认值**。配置文件默认 `./config.json`（可用 `BEACONX_CONFIG` 指定），结构见 [config.example.json](config.example.json)；完整说明见 [docs/configuration.md](docs/configuration.md)。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `BEACONX_DB_DSN` | 空（必填） | PostgreSQL 连接串 |
| `BEACONX_ADDR` | `:6969` | HTTP 监听地址 |
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

## HTTP 接口（M2）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 存活探针 |
| GET | `/readyz` | 就绪探针：数据库连通 + 迁移已应用 |
| GET | `/api/v1/system/info` | 版本、构建信息、schema 版本、运行时长 |
| GET | `/api/v1/system/tenant` | 当前请求解析到的租户（验证租户上下文） |
| POST | `/api/v1/missions` | 创建任务（请求体 JSON：`name` / `goal` / `description`） |
| GET | `/api/v1/missions` | 任务列表（`?status=`、`?limit=`，默认 50） |
| GET | `/api/v1/missions/{id}` | 任务详情 |
| POST | `/api/v1/missions/{id}/pause` | 暂停运行中的任务 |
| POST | `/api/v1/missions/{id}/resume` | 恢复已暂停的任务（重新排队） |
| POST | `/api/v1/missions/{id}/cancel` | 取消任务 |
| DELETE | `/api/v1/missions/{id}` | 删除任务（级联删除数据，返回 204） |
| GET | `/api/v1/missions/{id}/events` | 任务事件流（SSE，支持 Last-Event-ID / `?since=`） |

除 `/healthz`、`/readyz`、`/api/v1/system/*` 外，所有接口都需要 `Authorization: Bearer <BEACONX_ADMIN_TOKEN>`；浏览器 EventSource 无法自定义请求头，SSE 端点也可用 `?token=<token>` 携带令牌。未配置令牌时，进程启动会生成临时令牌并打印到日志（仅当前进程有效）。

接口契约以 [api/openapi.yaml](api/openapi.yaml) 为唯一来源，路由表在 `internal/httpapi` 中声明式维护，二者需同步演进。

## 目录结构

```
cmd/beaconx/            进程入口（serve / migrate / healthcheck / version）
internal/config/        配置加载（文件 + 环境变量 + 默认值）
internal/logging/       结构化日志
internal/platform/      状态目录（按租户分区）
internal/tenant/        租户上下文
internal/httpapi/       声明式路由 + 中间件 + 处理器（含 SSE 与鉴权）
internal/app/events/    进程内事件总线（SSE 分发、慢消费者丢弃）
internal/app/runner/    任务运行时（准入调度、规划/执行、租约、恢复）
internal/app/system/    进程与依赖状态
internal/domain/        领域模型（资产身份、作战图、任务状态机）
internal/infra/pg/      PostgreSQL 连接池 + 资产/任务 Store
internal/infra/migrate/ 版本化迁移（校验和 + 咨询锁）
migrations/             嵌入式 SQL 迁移
api/                    OpenAPI 契约
scripts/                CI 辅助脚本（品牌门禁等）
docs/                   术语、配置、路线图、数据模型、运行时
```

## 工程约定

- **版本化迁移**：`migrations/NNNN_*.sql` 只增不改；启动时只应用未执行版本，校验和不匹配即拒绝启动。
- **租户预留**：所有业务表以 `tenant_id` 打头的主键/唯一键；v1 单租户，但目录、上下文与 store 均已按租户分区。
- **可移植**：纯 Go、`CGO_ENABLED=0`；CI 对 Linux/macOS/Windows 交叉编译，Windows 为后续版本预留。
- **默认失败关闭**：需要认证的路由在认证组件未装配时返回 503，而不是放行。
- **运行时默认空实现**：M2 的 `IdlePlanner` / `NoopWorker` 让任务在一次规划周期内走完生命周期；M3 通过 `runner.Planner` / `runner.Worker` 注入真实 Agent，引擎逻辑不变。
- **品牌门禁**：`scripts/check-brand.sh` 在 CI 中阻止第三方项目标识进入代码、依赖与二进制。

## 文档

- [路线图](docs/roadmap.md) — M0–M6 里程碑与验收标准
- [运行时与引擎](docs/runtime.md) — M2 状态机、调度、租约、恢复、SSE 与鉴权
- [数据模型](docs/data-model.md) — M1–M2 表结构、资产身份、作战图、覆盖度与事件流
- [术语表](docs/glossary.md) — 产品与代码统一用词
- [配置说明](docs/configuration.md) — 配置项、运行时参数、状态目录与迁移机制

## 许可

本项目采用 [Apache License 2.0](LICENSE) 许可。你可以自由使用、修改与分发，包括商业用途；分发时需保留许可证与版权声明，并遵守其中的专利与商标条款。

Copyright 2026 The BeaconX Authors

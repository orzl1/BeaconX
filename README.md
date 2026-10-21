# BeaconX

[![ci](https://github.com/orzl1/BeaconX/actions/workflows/ci.yml/badge.svg)](https://github.com/orzl1/BeaconX/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

自主安全测试平台（Autonomous Security Testing Platform）。目标是在**可控授权范围**内，用多 Agent 完成从目标分解、侦察、验证到证据留存的完整流程；人负责审批与复核，而不是逐条输入命令。

> **当前状态：M0（骨架）**。已具备可运行的进程、声明式 HTTP 层、版本化迁移、租户预留、健康检查与 CI；业务能力（资产/作战图/Agent/审批/证据）按 [路线图](docs/roadmap.md) 逐步加入。**M0 不等于可用产品**，请勿用于真实目标。

## 快速开始

前置：Docker（含 Compose）。

```bash
docker compose up -d --build
open http://localhost:8787/healthz     # {"status":"ok"}
curl -s http://localhost:8787/readyz   # 就绪探针（含数据库与迁移版本）
curl -s http://localhost:8787/api/v1/system/info
```

停止：

```bash
docker compose down          # 保留数据
docker compose down -v       # 连数据一起删除
```

## 本地开发

前置：Go 1.24+、一个可用的 PostgreSQL 16。

```bash
cp .env.example .env          # 按需修改 BEACONX_DB_DSN
set -a && . ./.env && set +a

make build
./bin/beaconx migrate         # 应用迁移（启动时也会自动应用）
./bin/beaconx serve           # 默认监听 :8787
```

常用命令：

```bash
make test          # 单元测试
make test-race     # 竞态检测
make lint          # gofmt + go vet + 品牌门禁
make build         # 产出 bin/beaconx
make docker-build  # 构建镜像
```

## 配置

优先级：**环境变量 > 配置文件 > 内置默认值**。配置文件默认 `./config.json`（可用 `BEACONX_CONFIG` 指定），结构见 [config.example.json](config.example.json)；完整说明见 [docs/configuration.md](docs/configuration.md)。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `BEACONX_DB_DSN` | 空（必填） | PostgreSQL 连接串 |
| `BEACONX_ADDR` | `:8787` | HTTP 监听地址 |
| `BEACONX_HOME` | `data` | 状态目录（会话/证据/流量/技能） |
| `BEACONX_TENANT_DEFAULT_ID` | `1` | 单租户默认 ID（多租户从第一天预留） |
| `BEACONX_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `BEACONX_LOG_FORMAT` | `json` | `json` / `text` |
| `BEACONX_CONFIG` | `config.json` | 配置文件路径 |

## HTTP 接口（M0）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 存活探针 |
| GET | `/readyz` | 就绪探针：数据库连通 + 迁移已应用 |
| GET | `/api/v1/system/info` | 版本、构建信息、schema 版本、运行时长 |
| GET | `/api/v1/system/tenant` | 当前请求解析到的租户（验证租户上下文） |

接口契约以 [api/openapi.yaml](api/openapi.yaml) 为唯一来源，路由表在 `internal/httpapi` 中声明式维护，二者需同步演进。

## 目录结构

```
cmd/beaconx/            进程入口（serve / migrate / healthcheck / version）
internal/config/        配置加载（文件 + 环境变量 + 默认值）
internal/logging/       结构化日志
internal/platform/      状态目录（按租户分区）
internal/tenant/        租户上下文
internal/httpapi/       声明式路由 + 中间件 + 处理器
internal/app/system/    进程与依赖状态
internal/infra/pg/      PostgreSQL 连接池
internal/infra/migrate/ 版本化迁移（校验和 + 咨询锁）
migrations/             嵌入式 SQL 迁移
api/                    OpenAPI 契约
scripts/                CI 辅助脚本（品牌门禁等）
docs/                   术语、配置、路线图
```

## 工程约定

- **版本化迁移**：`migrations/NNNN_*.sql` 只增不改；启动时只应用未执行版本，校验和不匹配即拒绝启动。
- **租户预留**：所有业务表以 `tenant_id` 打头的主键/唯一键；v1 单租户，但目录、上下文与 store 均已按租户分区。
- **可移植**：纯 Go、`CGO_ENABLED=0`；CI 对 Linux/macOS/Windows 交叉编译，Windows 为后续版本预留。
- **默认失败关闭**：需要认证的路由在认证组件未装配时返回 503，而不是放行。
- **品牌门禁**：`scripts/check-brand.sh` 在 CI 中阻止第三方项目标识进入代码、依赖与二进制。

## 文档

- [路线图](docs/roadmap.md) — M0–M6 里程碑与验收标准
- [术语表](docs/glossary.md) — 产品与代码统一用词
- [配置说明](docs/configuration.md) — 配置项、状态目录、迁移机制

## 许可

本项目采用 [Apache License 2.0](LICENSE) 许可。你可以自由使用、修改与分发，包括商业用途；分发时需保留许可证与版权声明，并遵守其中的专利与商标条款。

Copyright 2026 The BeaconX Authors

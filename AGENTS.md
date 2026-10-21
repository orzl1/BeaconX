# BeaconX 开发约定（Agent 必读）

## 唯一真源文档

- **`docs/project-map.md` 是项目代码功能结构的唯一真源**：代码地图、数据模型、API 全表、状态机、运行时机制、配置、前端对接指南、缺口与路线。
- **任何改动都必须在同一次提交里更新它**：新增/修改 API、数据表、状态机、模块、配置项、运行机制、前端可用的字段或行为，都要更新对应章节，并在文末「变更记录」追加一条（日期、commit、变更点、影响章节）。
- 只改内部实现、不改变任何对外行为的重构，也需要在变更记录里写一条。
- 提交前运行 `make docs-check`（校验文档覆盖全部 HTTP 路由）与 `make lint`。

## 其他必须保持同步的文档

| 改动类型 | 必须同步 |
| --- | --- |
| HTTP API | `docs/project-map.md` + `api/openapi.yaml` + 必要时 README 接口表 |
| 数据库迁移 | `docs/project-map.md` §数据模型 + `docs/data-model.md` |
| 配置/环境变量 | `docs/project-map.md` §配置 + `docs/configuration.md` + `config.example.json` + `.env.example` |
| 前端/构建/发布 | `docs/project-map.md` §前端指南/§测试发布 + `docs/web.md` / `docs/release.md` |
| 安全策略/证据 | `docs/project-map.md` + `docs/tools-policy.md` / `docs/evidence.md` |

## 工程约束

- 文档、注释、日志、错误信息、用户可见文案统一中文；标识符、JSON 字段、错误码、环境变量、SQL 保留英文。
- 品牌门禁：`./scripts/check-brand.sh`，不得出现原项目名称痕迹。
- 默认端口 **6969**；单租户 v1，但所有表/查询保留 `tenant_id` 并作为联合主键首列。
- 迁移只追加、不修改已发布文件；校验和漂移会拒绝启动。
- 前端不造假数据：页面只展示真实接口返回。
- 工具调用必须经 `internal/app/executor`（策略门 + 审批 + 审计 + 证据采集），不得旁路。
- 测试：`BEACONX_TEST_PG_DSN='postgres://beaconx:beaconx@127.0.0.1:55432/beaconx?sslmode=disable' go test ./...`。
- 提交前：`gofmt`、`go vet`、`make lint`、`make docs-check`、`go test ./...`。

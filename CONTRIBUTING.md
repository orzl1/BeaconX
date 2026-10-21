# 贡献指南

## 开发流程

1. 先读 `docs/project-map.md`（项目唯一真源）与 `AGENTS.md`。
2. 小步修改；数据库只追加迁移；对外行为变更先想清楚兼容性。
3. 测试：`make test`；集成测试需要 PostgreSQL（见 README）。
4. 提交前：`make lint && make docs-check && make test`。

## 改动清单（必须逐项确认）

- [ ] **已更新 `docs/project-map.md`**：对应章节 + 文末「变更记录」。
- [ ] 涉及 HTTP API：已更新 `api/openapi.yaml`；必要时更新 README 接口表。
- [ ] 涉及数据表：已新增迁移（不修改历史迁移）并更新 `docs/project-map.md` 数据模型与 `docs/data-model.md`。
- [ ] 涉及配置：已更新 `docs/configuration.md`、`config.example.json`、`.env.example`。
- [ ] 涉及前端可用字段/行为：已更新 `docs/project-map.md` 前端对接指南。
- [ ] 新增/修改测试并通过 `make test`（含 PostgreSQL 集成）。
- [ ] `make lint`、`make docs-check` 通过；文档/注释为中文，无原项目品牌痕迹。

> 只改内部实现、不改变对外行为的重构，也需要在 `docs/project-map.md` 变更记录中写一条，说明“无对外行为变化”。

## 安全

- 不要提交任何密钥、令牌、真实 API Key；本地密钥放 `.env.local`（已 gitignore）。
- 工具调用必须经过策略门（`internal/app/executor`），不得新开旁路。

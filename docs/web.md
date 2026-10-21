# Web 控制台（M6）

本文描述 BeaconX M6 的管理前端：技术选型、开发/构建方式、页面功能、安全边界与已知限制。

权威来源是 `web/`（Next.js 16 App Router + React + TypeScript + 原生 CSS）、`internal/webui/webui.go`、`Makefile` 的 `web` 目标、`Dockerfile` 与 `.github/workflows/release.yml`；本文只做解释，若与代码不一致，以代码为准并回来修正本文。前端随二进制发布，Go 侧托管方式见 [发布与自更新](release.md)。

## 1. 架构

- **静态导出**：生产构建设置 `NEXT_EXPORT=1`，Next.js 以 `output: "export"` 输出纯静态目录 `web/out`（无 Node 运行时、无 Server Actions、无动态路由）。
- **Go 托管**：`make web` 把 `web/out` 复制到 `internal/webui/dist`，Go 通过 `//go:embed all:dist` 嵌入二进制；服务端把 `internal/webui/dist` 挂到 `/`，因此页面与 API 天然同源。
- **占位页**：仓库内 `internal/webui/dist/index.html` 是占位页。未执行 `make web` 时仍可启动，页面会提示执行 `make web` 或使用发布包中的完整前端。
- **路由规则**（`internal/webui/webui.go`）：
  - 已存在的静态文件按原样返回（`/_next/*` 等）；
  - 其余非 `/api/` 路径回退到 `index.html`（静态导出前端只有单页）；
  - `/api/` 下的未知路径返回 JSON 404（`{"error":{"code":"not_found",…}}`），避免把接口 404 误当页面。
- **Docker 镜像**：`Dockerfile` 先以 `node:22-alpine` 构建前端，再把 `web/out` 复制到 Go 构建阶段嵌入，最终单镜像同时包含 API 与前端（端口 6969）。

### 页面结构

```text
/（单页控制台）
├─ TokenBar            令牌输入 / 保存 / 清除（localStorage）
├─ 任务 tab
│  ├─ MissionsPanel    任务列表（5s 轮询）+ 创建 + 暂停/恢复/取消/删除
│  └─ MissionDetail    任务详情 + SSE 事件流（最多 200 条）+ 流量/证据
├─ 审批 tab
│  └─ ApprovalsPanel   pending 审批列表（5s 轮询）+ 批准/拒绝
└─ 系统 tab
   └─ SystemPanel      公开探针（10s 轮询）+ 自更新 check/apply
```

页面只使用一个页面组件与若干展示组件，不引入 UI 组件库或状态管理库；组件间通过 props 传递令牌与当前选中任务。

## 2. 开发与构建

环境要求：Node.js ≥ 20.9（Next.js 16 要求；本仓库验证使用 Node 24）、npm ≥ 10；可选本地 Go 后端（默认 `http://127.0.0.1:6969`）。

```bash
cd web
npm ci                      # 安装依赖
npm run dev                 # http://localhost:3000
npm run typecheck           # next typegen + tsc --noEmit
```

开发模式不设置 `NEXT_EXPORT`，`next.config.mjs` 会把 `/api/*` 反代到 `BEACONX_API_PROXY`（默认 `http://127.0.0.1:6969`），让页面、普通请求与 SSE 保持同源，避免浏览器因后端没有 CORS 而拦截：

```bash
BEACONX_API_PROXY=http://192.168.1.10:6969 npm run dev
```

生产/交付路径：

```bash
make web        # cd web && npm ci && NEXT_EXPORT=1 npm run build，然后复制到 internal/webui/dist
make build      # 构建嵌入前端的 bin/beaconx
./bin/beaconx serve
```

说明：

- `NEXT_PUBLIC_API_BASE` 会在构建期内联。留空（推荐）表示 API 与页面同源 `/api/v1`；只有 API 部署在另一个源时才设置，且需要后端自行提供 CORS（当前后端没有）。
- 开发模式下的 SSE 会经过 Next 代理；若观察到事件流被缓冲，请用生产构建 + Go 同源托管验证实时性。
- `web/package-lock.json` 会提交以锁定依赖；`node_modules/`、`.next/`、`out/` 不提交。

## 3. 功能范围

页面是单页控制台，顶部令牌栏 + 三个功能区（任务 / 审批 / 系统）。所有数据来自真实后端接口，没有 mock/fixture。

### 令牌

- 用户把管理令牌粘贴到顶部并保存到本机浏览器 `localStorage`（键名 `beaconx.admin_token`），不上传到其它地方。
- 普通请求使用 `Authorization: Bearer <token>`；SSE 因为 `EventSource` 不能自定义请求头，使用 `?token=` 查询参数（后端两种都支持）。
- 令牌留空时后端会生成临时令牌并打印到启动日志，从日志获取后填入。

### 任务

- 列表：`GET /api/v1/missions?limit=100`，每 5 秒自动刷新；展示状态、预算用量、时间等。
- 创建：`POST /api/v1/missions`，字段 `name` / `goal` / `max_tokens` / `max_steps`（后两者可空，0 或省略表示不限制）。
- 状态机按钮限制：暂停仅对 `running`、恢复仅对 `paused`、取消对排队/运行/暂停/收尾中、删除任意状态可用（二次确认）；删除返回 204。
- 任务详情：`GET /api/v1/missions/{id}`，每 5 秒刷新。

### 任务详情与事件流

- 订阅 `GET /api/v1/missions/{id}/events?token=…&since=0`，展示 SSE 事件；断线由浏览器 `EventSource` 自动重连并携带 `Last-Event-ID`，后端据此回放补洞；页面提供手动“重新连接”。
- 已登记 14 类事件：`created` / `admitted` / `started` / `paused` / `resumed` / `finalizing` / `completed` / `failed` / `cancelled` / `deleted` / `recovered` / `approval_required` / `budget_exhausted` / `step_budget_exhausted`。后端新增事件类型时，需要在 `web/src/lib/format.ts` 的 `STREAM_EVENT_TYPES` 中登记中文标签，否则页面收不到该类型的事件。
- 事件按 SSE 的 `id` 去重（重连回放会重复下发），页面只保留最近 200 条用于展示；完整历史以数据库为准。
- 收到状态类事件时刷新任务详情与流量列表。

### 流量与证据

- `GET /api/v1/missions/{id}/traffic` 列出两阶段采集记录（intent/result）。
- `POST /api/v1/traffic/{id}/evidence` 冻结证据；后端要求 `phase=result`，否则 409 conflict；页面如实展示后端错误。
- `GET /api/v1/evidence/{id}/verify` 校验签名与正文哈希：200 通过，409 `tampered` 表示被篡改或密钥不匹配。

### 审批

- `GET /api/v1/approvals?status=pending&limit=100`，每 5 秒刷新；展示工具名与参数 JSON。
- 决策：`POST /api/v1/approvals/{id}/decide`，请求体 `{"approve":bool,"note":string}`；已处理/过期由后端 409 拒绝，页面展示后端错误。

### 系统

- 公开探针 `GET /healthz`、`GET /readyz`、`GET /api/v1/system/info` 每 10 秒刷新（无需令牌）。
- 更新入口：`GET /api/v1/system/update/check`、`POST /api/v1/system/update/apply`；HTTP 层未装配自更新时后端返回 503 `update_unavailable`（正常 `serve` 总会装配），自更新被禁用时检查返回 502 `update_check_failed`，页面如实展示“更新接口不可用/自更新已禁用”；成功时展示后端返回的 JSON（`has_update` / `release` / `updated` / `version`），不伪造版本信息。

明确不展示：计划步骤与图节点（后端暂无公开查询接口）、任何 mock 数据。

## 4. 安全边界

- 令牌只保存在本机 `localStorage`，页面不打印、不发送给除 BeaconX 后端之外的任何服务。
- 同源部署（Go 托管静态文件）避免 CORS；后端当前不提供 CORS，跨源部署需要自行补充。
- SSE 的 `?token=` 可能出现在浏览器历史与代理访问日志中，生产部署应启用 HTTPS 并限制日志访问。
- 前端不做多用户、权限分级或记住我；它只是本机管理界面的薄壳。

## 5. 构建验证

- Node 22（`Dockerfile`、发布流水线）或 Node 24（本地验证）执行 `npm ci` 后 `NEXT_EXPORT=1 npm run build`，产物是 `web/out`（含 `index.html` 与 `_next/*` 静态资源）。
- Go 侧验证：`/` 返回前端页面；`/missions/123` 回退到 `index.html`；`/_next/*` 静态资源 200；`/api/v1/nope` 返回 JSON 404。
- `make web` 后重新 `make build`，二进制内嵌完整前端；未执行 `make web` 时显示占位页。
- 常规 CI（`.github/workflows/ci.yml`）与发布流水线（`.github/workflows/release.yml`）都包含「前端构建」job：安装依赖、类型检查、静态导出并校验 `web/out/index.html`。

### 错误展示

- 所有 API 错误统一解析后端 `{"error":{"code","message","request_id"}}` 结构：页面展示 `message`，并附带 `code` 与 `request_id` 便于排查。
- 401 在页面顶部提示重新输入令牌；409/503 等业务错误原样展示后端消息，不翻译成模糊文案。
- 事件流遇到 401 时 `EventSource` 会自动重试，页面显示“重连中”；修正令牌后可点“重新连接”立即重建连接。

## 6. 已知限制

- 单页应用，没有 URL 级路由；刷新页面后选中的任务会丢失，需要重新选择。
- 事件列表只保留最近 200 条（页面内存），完整历史以数据库为准。
- `EventSource` 只派发已注册的命名事件；后端新增事件类型需要同步 `STREAM_EVENT_TYPES`。
- 没有浏览器级 e2e 测试；开发代理下的 SSE 未做真机验证（M6.1）。
- 跨源部署缺少 CORS；发布包只有 SHA256 校验（无签名）；Windows 自更新未验证（M6.1）。

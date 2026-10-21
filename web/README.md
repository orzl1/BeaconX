# BeaconX Web 控制台（M6）

BeaconX 的最小可用管理前端：任务管理与事件流、工具审批、流量证据冻结与校验、健康/系统信息。
技术栈刻意保持最小：**Next.js（App Router）+ React + TypeScript + 原生 CSS**，不引入 Tailwind、
UI 组件库、图表库或数据请求库。

构建产物是**纯静态目录** `web/out`，随后由 Go 服务（`go:embed`）托管；API 与页面同源，
因此运行时不需要 Node。

## 环境要求

- Node.js ≥ 20.9（Next.js 16 要求；本仓库验证使用 Node 24）
- npm ≥ 10
- 可选：本地运行的 BeaconX 后端（默认 `http://127.0.0.1:6969`）

## 安装

使用工作区内的 npm 缓存（与仓库约定一致）：

```bash
cd web
npm_config_cache=/Users/dude/Desktop/Agent/BeaconX/.cache/npm npm install
```

`package-lock.json` 会被提交以锁定依赖版本；`node_modules/`、`.next/`、`out/` 已在
`web/.gitignore` 中排除。

## 开发

```bash
cd web
npm run dev          # http://localhost:3000
```

开发模式下 `next.config.mjs` 会把 `/api/*` 反代到 `BEACONX_API_PROXY`（默认
`http://127.0.0.1:6969`），光标与 SSE 都保持同源，避免后端（当前没有 CORS）被浏览器拦截。
如需指向其他后端：

```bash
BEACONX_API_PROXY=http://192.168.1.10:6969 npm run dev
```

> 开发模式的 SSE 会经过 Next 的代理；若观察到事件流被缓冲，请用生产构建 + Go 托管
> （同源静态文件）路径验证实时性。

## 构建（静态导出）

```bash
cd web
NEXT_EXPORT=1 NEXT_PUBLIC_API_BASE= npm run build
```

- `NEXT_EXPORT=1` 时 Next 输出纯静态目录 `out/`，入口为 `web/out/index.html`；
- `NEXT_PUBLIC_API_BASE` 会在构建期内联：留空（推荐）表示 API 与页面同源 `/api/v1`；
  仅当 API 部署在另一个源时才设置（需要后端自行提供 CORS，当前后端没有）；
- `npm run build` 已内置 `NEXT_EXPORT=1`，上面的显式写法便于在 CI 中说明意图。

类型检查（不需要先构建，内部会先用 `next typegen` 生成路由类型）：

```bash
cd web
npm run typecheck
```

构建后自检：

```bash
test -f out/index.html && echo "index.html 已生成"
grep -o 'src="[^"]*\.js"' out/index.html | head    # 静态资源引用
grep -o '<title>[^<]*</title>' out/index.html      # 文案/标题
```

## 目录结构

```text
web/
  next.config.mjs      # 静态导出 / 开发代理
  src/app/layout.tsx   # 根布局（中文 lang、全局样式）
  src/app/page.tsx     # 单页控制台入口（令牌 + 三个功能区）
  src/components/
    TokenBar.tsx       # 顶部令牌输入与 localStorage 持久化
    MissionsPanel.tsx  # 任务列表 / 创建 / 暂停恢复取消删除
    MissionDetail.tsx  # 任务详情 + SSE 事件流 + 流量证据
    ApprovalsPanel.tsx # 待审批列表与批准/拒绝
    SystemPanel.tsx    # 健康/就绪/系统信息/自更新入口
    ui.tsx             # 无依赖的小展示组件
  src/lib/
    api.ts             # 全部 HTTP/SSE 调用与错误模型
    types.ts           # 后端 JSON 契约类型
    format.ts          # 中文标签与格式化
    hooks.ts           # 轮询/挂载 hooks
  src/styles.css       # 原生 CSS（响应式 + 深色模式）
```

## 令牌与安全

- 管理令牌由用户粘贴到页面顶部，保存在**本机浏览器 localStorage**（键名
  `beaconx.admin_token`），不会上传到除 BeaconX 后端之外的任何地方。
- 普通请求带 `Authorization: Bearer <token>`；任务事件流因为是 `EventSource`，只能把令牌
  放在 `?token=` 查询参数里（后端两种方式都支持，均为常量时间比较）。
  注意：查询参数可能出现在浏览器历史、代理访问日志中；生产部署请启用 HTTPS 并限制日志访问。
- 令牌留空时，后端（`BEACONX_ADMIN_TOKEN` 未配置时）会生成临时令牌并打印到启动日志，
  请从日志中获取后填入页面。
- 本前端不存储任何凭据到服务端，也没有"记住我/多用户"概念。

## 功能范围与事实边界（M6）

已实现（全部调用真实后端接口）：

1. **任务**：列表（`GET /api/v1/missions`）、创建（`POST /api/v1/missions`，字段
   `name`/`goal`/`max_tokens`/`max_steps`）、暂停/恢复/取消（`POST .../pause|resume|cancel`）、
   删除（`DELETE`，204）。
2. **任务详情**：`GET /api/v1/missions/{id}`；SSE
   `GET /api/v1/missions/{id}/events?token=…&since=0`，展示 created / admitted / finalizing /
   completed / approval_required 等事件，断线由 EventSource 自动重连并携带 `Last-Event-ID`。
3. **证据**：`GET /api/v1/missions/{id}/traffic` 列表；`POST /api/v1/traffic/{id}/evidence`
   冻结（要求 `phase=result`，否则 409）；`GET /api/v1/evidence/{id}/verify` 校验
   （200 = 通过；409 `tampered` = 签名/正文哈希不一致）。
4. **审批**：`GET /api/v1/approvals?status=pending`；`POST /api/v1/approvals/{id}/decide`
   `{"approve":bool,"note":string}`；展示工具名与参数 JSON。
5. **系统**：`GET /healthz`、`/readyz`、`/api/v1/system/info`；更新入口
   `GET /api/v1/system/update/check`、`POST /api/v1/system/update/apply`。
   更新接口由后端提供：未配置自更新时后端返回 503 `update_unavailable`，前端如实展示
   "更新接口不可用"；成功时展示后端返回的 JSON（`has_update` / `release` / `updated` / `version`），
   既不伪造版本信息，也不把接口缺失说成"已是最新"。

明确不做（避免把不存在的数据当真实数据展示）：

- **计划步骤 / 图节点**：后端当前没有公开查询接口，页面不展示，也不使用占位图。
- **假数据 / mock**：仓库内没有任何 fixture；空状态就是空状态。
- **多用户、权限分级、国际化**：M6 范围外。

## 交互说明

- 任务列表每 5 秒自动刷新；任务详情与流量每 5 秒刷新，同时事件流实时推送状态变化。
- 暂停仅对 `running` 可用；恢复仅对 `paused` 可用；取消对排队/运行/暂停/收尾中可用；
  删除任意状态可用（会二次确认，后端级联删除）。
- 审批决策后列表立即刷新；已处理/过期的审批会被后端以 409 拒绝，页面会展示后端错误信息。
- 所有失败都会在对应面板内展示后端 `error.message`（并附带 `code` 与 `request_id`），
  401 会在页面顶部提示重新输入令牌。

## 已知限制

- 单页应用，没有 URL 级路由；刷新页面后选中的任务会丢失（需重新选择）。
- 事件列表只保留最近 200 条（页面内存），完整历史以数据库为准。
- 事件流若因令牌错误持续 401，EventSource 会自动重试；页面提示"重连中"，请修正令牌后点
  "重新连接"。
- `EventSource` 只派发**已注册的命名事件**：后端新增任务事件类型时，需要在
  `src/lib/format.ts` 的 `STREAM_EVENT_TYPES` 中登记中文标签，否则页面收不到该类型的事件。
- 开发模式的 SSE 经 Next 代理，极端情况下可能被缓冲；生产交付路径是 Go 同源托管静态文件。

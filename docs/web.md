# Web 控制台

本文描述 BeaconX 管理前端：技术选型、开发/构建、页面与安全边界。

权威来源是 `web/`（Next.js 16 App Router + React + TypeScript + 原生 CSS）、`internal/webui/webui.go`、`Makefile` 的 `web` 目标。若与代码不一致，以代码为准并回来修正本文。前端随二进制发布，Go 侧托管见 [发布与自更新](release.md)。页面与接口的字段约定以 [项目地图](project-map.md) §8 为准。

## 1. 架构

- **静态导出**：`NEXT_EXPORT=1` 时 Next.js 以 `output: "export"` 输出 `web/out`。没有 Node 运行时、没有 Server Actions。
- **单页 HTML**：只生成首页。任务、发现等地址由浏览器 `location` 决定，页面内用 History API 切换。Go 对非 `/api/`、且磁盘上不存在的路径回退 `index.html`，因此刷新 `/missions/12` 仍能打开对应任务。
- **开发回退**：`next dev` 把未匹配路径 rewrite 到 `/`，并把 `/api`、`/healthz`、`/readyz` 反代到 `BEACONX_API_PROXY`（默认 `http://127.0.0.1:6969`）。
- **Go 托管**：`make web` 把 `web/out` 复制到 `internal/webui/dist`，二进制用 `go:embed` 挂到 `/`。

### 页面

侧栏两组。功能：概览、任务、发现、流量、命令、模型记录、资产、工作区、同步、对话（未开放）。系统：模型配置（未开放，可跳到设置）、Agents、MCP、技能、工具（无列表接口，页面列出代码里的 `http_probe` 与 `shell_exec`）、通知、策略、审批、日志、设置。

任务详情用查询参数 `?tab=` 切换：概览、事件、计划、作战图、资产、覆盖度、发现、复测、审批、流量、报告。

对话和模型配置没有后端接口，页面只说明缺口，不放假数据。已有写接口都有按钮：企业/资产/发现/渠道/MCP/技能/同步源/策略的修改，技能文件删除，任务流量清空，证据校验/审计/导出，列表游标「加载更多」。

## 2. 开发与构建

Node.js ≥ 20.9，npm ≥ 10。

```bash
cd web
npm ci
npm run dev          # http://localhost:3000
npm run typecheck
```

```bash
make web             # 静态构建并复制到 internal/webui/dist
make build
./bin/beaconx serve  # http://127.0.0.1:6969
```

`NEXT_PUBLIC_API_BASE` 留空表示同源 `/api/v1`。后端不提供 CORS。

## 3. 外观

- 浅色：工作区纯白（`#fff`），侧栏纯黑。
- 深色：整页纯黑（`#000`），用边线分开区块。右上角切换，记在 `localStorage` 键 `beaconx.theme`。
- 主按钮在浅色是黑底白字，在深色是白底黑字。状态用蓝、琥珀、紫、绿、红配文字，不用颜色单独表达。

## 4. 数据

所有列表、详情、按钮都打真实接口。空列表展示空状态。错误展示后端 `error.message`，并附 `code` 与 `request_id`。401 提示重新保存令牌。

令牌键 `beaconx.admin_token`。普通请求 `Authorization: Bearer`。SSE（任务事件、日志）用 `?token=`。

轮询与按需加载见项目地图 §8.3：任务与审批 5 秒，流量/命令/资产 10 秒，概览 15 秒，流量与模型正文点击后才取。

任务事件在 `web/src/lib/format.ts` 的 `STREAM_EVENT_TYPES` 登记，含规划诊断三类。页面按 SSE id 去重，只留最近 200 条。

## 5. 安全边界

- 令牌只在本机浏览器。
- 同源部署，后端无 CORS。
- SSE 的 `?token=` 可能进浏览器历史和代理日志。
- 流量重放、证据轮换、自更新应用都有确认，请求仍由后端策略门和权限决定。

## 6. 已知限制

- 静态导出没有按 id 预生成的 HTML 文件，深链依赖服务器回退到 `index.html`。
- 事件列表只留最近 200 条。
- 后端新增命名事件时，要同步 `STREAM_EVENT_TYPES`，否则 EventSource 收不到。
- 没有浏览器 e2e。对话、模型 profile、工具清单接口仍未提供。工具页展示的是代码登记的内置参数，不是实时注册表。

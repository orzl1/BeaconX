# 发布与自更新（M6）

本文描述 BeaconX 的发布流水线、发布资产、自更新与回滚机制，以及镜像交付方式。

权威来源是 `.github/workflows/release.yml`、`internal/infra/selfupdate/selfupdate.go`、`internal/httpapi/update.go`、`cmd/beaconx/main.go`、`internal/config/config.go` 与 `Dockerfile`；本文只做解释，若与代码不一致，以代码为准并回来修正本文。前端构建与托管见 [Web 控制台](web.md)。

## 1. 发布流水线

发布由 tag 触发（也可以手动 `workflow_dispatch`）：

```text
push tag v*  →  release.yml
                  ├─ web     前端构建（node 22 + npm ci + NEXT_EXPORT=1 npm run build）
                  ├─ build   5 平台交叉编译 + 打包 zip
                  └─ release 汇总产物 → SHA256SUMS → gh release create
```

- **web job**：`actions/setup-node@v4`（node 22、npm 缓存指向 `web/package-lock.json`）→ `npm ci --no-audit --no-fund` → `NEXT_EXPORT=1 npm run build` → 校验 `web/out/index.html` 存在 → 上传 `web-dist` artifact。
- **build job**：`needs: web`，下载 `web-dist` 覆盖到 `internal/webui/dist`，再按矩阵交叉编译：
  - `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`；
  - `CGO_ENABLED=0 go build -trimpath`，`ldflags` 注入 `Version=${GITHUB_REF_NAME}`、`Commit=${GITHUB_SHA}`、`BuildTime`（UTC）；
  - 产物统一放到打包目录并命名为 `beaconx`（Windows 为 `beaconx.exe`），再打成 zip：`beaconx-<goos>-<goarch>.zip`，保证 zip 内可执行文件名与自更新解压逻辑一致。
- **release job**：下载全部 `dist-*` artifact → `sha256sum *.zip > SHA256SUMS` → `gh release create <tag> dist/*.zip dist/SHA256SUMS --title "BeaconX <tag>" --notes ... --verify-tag`。

产物命名约定：

| 资产 | 说明 |
| --- | --- |
| `beaconx-linux-amd64.zip` 等 | 每平台一个 zip，内含可执行文件 |
| `SHA256SUMS` | 所有 zip 的 sha256sum 清单（`<hash>  <文件名>`） |

自更新按运行时的 `runtime.GOOS` / `runtime.GOARCH` 选择 `beaconx-<goos>-<goarch>.zip`，并读取同一个 Release 的 `SHA256SUMS` 做校验。

一次典型 Release 的资产列表：

```text
beaconx-linux-amd64.zip
beaconx-linux-arm64.zip
beaconx-darwin-amd64.zip
beaconx-darwin-arm64.zip
beaconx-windows-amd64.zip
SHA256SUMS
```

`SHA256SUMS` 是 `sha256sum *.zip` 的输出（`<64 位哈希>  <文件名>`）。tag 必须是已存在的 `v` 前缀版本（`gh release create --verify-tag`）；当前版本通过 ldflags 注入二进制，可用 `beaconx version` 核对。

## 2. 自更新流程

`internal/infra/selfupdate` 实现「检查 → 下载 → 校验 → 冒烟 → 换装 → 回滚」：

1. **检查**：`GET https://api.github.com/repos/<repo>/releases/latest`（默认 `Accept: application/vnd.github+json`、`User-Agent: beaconx-selfupdate`），解析 `tag_name` / `html_url` / `body` / `published_at` / `assets`；在 assets 中查找本平台 zip 与 `SHA256SUMS`，缺少任一即报错。
2. **版本判断**：按语义化版本逐段比较数字；`dev` 视为永远比正式版本旧（允许更新）；相同版本视为无更新。
3. **下载**：zip 与 SHA256SUMS 分别限流 200MiB / 4MiB；只从 Release 提供的 HTTPS 地址下载（GitHub API 默认 HTTPS；测试可把 APIBase 指向 httptest）。
4. **校验**：从 SHA256SUMS 中找到 zip 文件名对应的哈希，与下载内容 sha256 比对（大小写不敏感），不匹配返回 `ErrChecksumMismatch`。
5. **解压**：从 zip 中查找 basename 为 `beaconx` 或 `beaconx.exe` 的条目，写入状态目录 `selfupdate/beaconx.new`（0755）。
6. **冒烟**：以 10s 超时执行 `beaconx.new --version`，输出必须包含 `beaconx`，否则返回 `ErrSmokeFailed` 并删除临时文件；**当前二进制不会被改动**。
7. **换装**：把当前可执行文件复制为 `<binary>.old`，再把新文件 `rename` 到当前路径；最后写 `selfupdate/update.json`（`{"version":…,"installed_at":…}`，0600）。
8. **回滚**：`<binary>.old` 存在时 rename 回当前路径；API 返回“已回滚，需重启生效”。

注意：

- 换装后需要**重启进程**才生效（当前进程仍运行旧二进制）。
- `update.json`、`beaconx.new`、`<binary>.old` 都位于状态目录或可执行文件旁；容器内通常没有写权限，见下文“容器部署”。
- 更新失败不会破坏当前文件：校验或冒烟失败发生在换装之前；rename 失败会保留备份。

## 3. 自更新 API

三个接口都需要管理令牌（`Authorization: Bearer` 或 SSE 场景的 `?token=` 不涉及此接口）：

| 方法 | 路径 | 成功响应 | 失败语义 |
| --- | --- | --- | --- |
| GET | `/api/v1/system/update/check` | `{"has_update":bool,"release":{…}}` | Updater 未装配 503 `update_unavailable`（正常 `serve` 总会装配）；禁用或检查失败 502 `update_check_failed` |
| POST | `/api/v1/system/update/apply` | 有更新：`{"updated":true,"version":…,"message":"更新完成，需重启生效"}`；无更新：`{"updated":false,"version":…,"message":"当前已是最新版本"}` | Updater 未装配 503；检查失败 502；下载/校验/冒烟失败 409 `update_failed` |
| POST | `/api/v1/system/update/rollback` | `{"rolled_back":true,"message":"已回滚，需重启生效"}` | Updater 未装配 503；无备份/回滚失败 409 `rollback_failed` |

`release` 对象字段：`version`、`tag`、`url`、`notes`、`published_at`、`asset_url`、`asset_name`、`checksum_url`。

## 4. 配置

| 配置项 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `selfupdate.enabled` | `BEACONX_UPDATE_ENABLED` | `true` | 是否启用自更新；解析为布尔值，非法值启动失败 |
| `selfupdate.repo` | `BEACONX_UPDATE_REPO` | `orzl1/BeaconX` | GitHub `owner/repo`；启用时不能为空，否则启动校验失败 |

- 关闭自更新后，检查/应用接口在检查阶段返回 502 `update_check_failed`（错误信息“自更新已禁用”），回滚接口仍可用（只依赖 `.old` 备份）；503 `update_unavailable` 只会在 HTTP 层未装配 Updater 时出现（正常 `serve` 总会装配）。
- **容器部署建议关闭**：镜像内二进制归 root 所有、容器文件系统通常只读或会被重建，自更新在容器里没有意义；应通过重新拉取镜像升级。设置 `BEACONX_UPDATE_ENABLED=false` 或忽略相关接口。
- GitHub API 对未认证请求有速率限制（约每小时 60 次/IP）；频繁轮询会返回 403，接口表现为 `update_check_failed`，前端会展示后端错误。

## 5. 镜像交付

`Dockerfile` 是多阶段构建：

1. `node:22-alpine`：`npm ci --no-audit --no-fund`、`NEXT_EXPORT=1 npm run build`，产出 `web/out`；
2. `golang:1.24-alpine`：复制源码与 `web/out` 到 `internal/webui/dist`，`CGO_ENABLED=0` 构建并注入版本/commit/build_time；
3. `alpine:3.21`：只保留二进制、CA 证书与时区，非 root 用户 `beaconx`，暴露 6969，`HEALTHCHECK` 使用 `/readyz`。

因此 `docker build -t beaconx:dev .` 得到的是**同时包含 API 与前端**的单镜像，访问 `/` 即为控制台；无需额外的前端容器。

## 6. 安全与已知限制

- 发布包只有 **SHA256 校验和**，没有 GPG/cosign 签名；校验和来自同一个 Release，能防传输损坏与普通篡改，不能防“同时篡改包与 SHA256SUMS 的发布者”。
- Windows 自更新未验证（运行中可执行文件可能无法替换、备份/回滚路径未实测）；容器内自更新未支持。
- 发布包内可执行文件统一命名为 `beaconx`（Windows 为 `beaconx.exe`），与 `selfupdate` 的解压查找逻辑一致；若手工重命名发布包内文件，自更新会因找不到可执行文件而失败。
- 自更新只处理单个二进制；前端已嵌入二进制，因此升级前端同样通过升级二进制完成。
- 浏览器级 e2e、URL 路由、CORS、发布签名与 Windows 自更新列入 M6.1。

## 7. 手工验证清单

```bash
# 本地构建（含前端）
make web && make build
./bin/beaconx version

# 镜像（含前端）
docker build -t beaconx:dev .
docker run --rm -p 6969:6969 -e BEACONX_DB_DSN=... beaconx:dev   # 访问 http://127.0.0.1:6969/

# 自更新接口（需要管理令牌）
curl -s http://127.0.0.1:6969/api/v1/system/update/check \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"
curl -s -X POST http://127.0.0.1:6969/api/v1/system/update/apply \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"
curl -s -X POST http://127.0.0.1:6969/api/v1/system/update/rollback \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"
```

- 发布前确认 tag 形如 `v1.2.3`（`gh release create --verify-tag` 要求 tag 已存在）。
- 发布后检查 Release 是否包含 5 个平台 zip 与 `SHA256SUMS`，并用 `sha256sum -c SHA256SUMS` 复核。

## 8. 故障排查

- `503 update_unavailable`：HTTP 层没有装配 Updater（通常只会出现在自定义装配/测试环境）；正常启动的 `serve` 一定会创建 updater。
- `502 update_check_failed`：GitHub API 访问失败（网络、代理、未认证速率限制约 60 次/小时/IP）、Release 缺少本平台 zip 或 `SHA256SUMS`、或自更新被禁用（`selfupdate.enabled=false` 时检查阶段返回 `ErrDisabled`）。查看服务日志与后端错误 `message`。
- `409 update_failed`：zip 与 `SHA256SUMS` 不匹配（下载损坏、发布时漏打/文件名不一致）、新二进制 `--version` 冒烟失败、或当前二进制所在目录不可写（容器/只读文件系统）。
- `409 rollback_failed`：没有 `<binary>.old`（从未成功更新过，或清理过状态目录）。
- 更新成功但 `beaconx version` 仍是旧版本：没有重启进程；当前进程继续运行旧二进制。
- Windows 上运行中的 `.exe` 可能无法被替换（自更新未在 Windows 验证）。

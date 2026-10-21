# 证据链（M5）

本文描述 BeaconX M5 的证据存储与校验：两阶段流量采集、内容加密落盘、快照签名、篡改检测与脱敏工具，以及对应 HTTP 接口。

权威来源是 `internal/app/evidence`、`migrations/0006_evidence.sql`、`internal/infra/pg/evidence_store.go`、`internal/app/executor/executor.go`、`internal/httpapi/evidence_api.go`、`cmd/beaconx/main.go` 与 `internal/config/config.go`；本文只做解释，若与代码不一致，以代码为准并回来修正本文。

## 1. 两阶段采集

工具调用由执行器（`internal/app/executor`）统一采集，流程是：

1. **intent 阶段**：工具真正执行前，把工具参数 JSON 脱敏后加密落盘为 blob，并写入一条 `traffic_exchanges`（`phase='intent'`、`request_hash` / `request_size`、`redaction`）。
2. **执行**：调用工具。
3. **result 阶段**：把工具输出文本脱敏后加密落盘，更新同一行为 `phase='result'`，写入 `response_hash` / `response_size`、`status_code`（来自工具 `Meta["status"]`，`http_probe` 有、`shell_exec` 为 NULL）、`error`、`completed_at`。

要点：

- 只有**通过策略门并真正执行**的调用会被采集；`deny` 或审批被拒绝的调用在执行前就返回，不会产生流量记录（但仍会写审计）。
- 中途崩溃会留下 `phase='intent'` 的行（`completed_at` 为空），可据此识别“已登记但未完成”的采集。
- 采集是**尽力而为**：脱敏、落盘或写库失败只记 WARN 日志，不影响工具本身的执行与审计。
- M5 采集的是**工具层**的请求参数与输出文本，不是原始网络报文；真正的 MITM 代理抓包属于 M5.1。

## 2. 正文格式与内容寻址

证据正文（blob）从不以明文落盘：

- 明文的 SHA-256 十六进制串作为内容地址；文件路径为 `<evidence.dir>/blobs/<hash 前两位>/<hash>.enc`。
- 文件内容为 `nonce(12 字节) || AES-256-GCM 密文`；AAD 是明文的 sha256 十六进制串，因此把合法文件复制到另一个哈希路径下会直接解密失败，而不是等到末尾比较才发现。
- 读取时重新校验：解密、大小比对、sha256 比对，任一失败都返回 `evidence.ErrTampered`。
- 相同内容幂等：文件已存在且能通过校验时直接复用；已存在但校验失败时返回 `ErrTampered`，**绝不覆盖**疑似被篡改的证据。
- 写入是原子的：临时文件（0600）→ `fsync` → `rename` → 目录 `fsync`（Windows 跳过目录 fsync），崩溃最多留下无引用的临时文件。

### 存储布局

```text
<evidence.dir>/
  blobs/
    ab/
      ab12cd34…ef.enc    # nonce(12) || AES-256-GCM 密文，0600
```

- 根目录与 `blobs/` 由 `evidence.New` 以 0700 创建；临时文件只在原子写入期间存在，成功 rename 后会被幂等清理。
- 目录缺失、权限不足、磁盘写满等 I/O 错误作为普通错误返回；调用方（执行器）只记 WARN，不阻塞工具执行。

## 3. 密钥管理

- 主密钥固定 32 字节；由它经 HMAC-SHA256 派生两把子密钥：`encKey`（标签 `beaconx/evidence/aes-256-gcm/v1`）与 `macKey`（标签 `beaconx/evidence/hmac-sha256/v1`）。加密与签名不复用同一把密钥，标签带版本号、不可修改。
- 来源优先级：
  1. `evidence.key_hex`（环境变量 `BEACONX_EVIDENCE_KEY`）：必须是 64 位十六进制（32 字节），否则启动失败；
  2. 否则读取状态目录下的 `evidence.key`；文件不存在时生成 32 字节随机密钥并以 0600 写入（十六进制文本），之后复用；文件损坏（非 32 字节）启动失败。
- 主密钥不落磁盘到证据存储内部、不打印到日志、不出现在错误信息里；证据目录与 blob 目录权限为 0700，blob 文件 0600。
- **不支持密钥轮换**（M5.1）：更换密钥会导致已有 blob 无法解密、快照验签失败，表现为 `ErrTampered`。丢失密钥等同于永久失去存量证据。

## 4. 证据快照与签名

快照（`evidence.Snapshot`）只记录元数据，正文通过 `BlobRef{hash,size}` 引用：

| 字段 | 说明 |
| --- | --- |
| `mission_id` | 快照所属任务 |
| `tool` | 产生该调用的工具名 |
| `request` / `response` | 正文的 `BlobRef`（sha256 + 明文大小） |
| `redaction` | 正文采用的脱敏策略标记（当前为 `json-keys`） |
| `created_at` | 冻结时间，参与计算前统一转 UTC（RFC3339Nano） |
| `id` | 规范化元数据的 SHA-256（由 `Seal` 计算） |
| `signature` | 对“含 ID、不含 Signature”的规范化 JSON 做 HMAC-SHA256 |

`Seal` 只做规范化与计算，不要求正文已存在；相同输入重复 Sealed 结果相同。`Verify` 不信任快照里的任何字段：先由元数据重算 ID，再由「重算 ID + 元数据」重算签名，比对使用 `hmac.Equal`；ID、元数据、签名的任何改动或换密钥都会返回 `ErrTampered`。

持久化只保存元数据、正文引用与签名（`mission_id/tool/request_hash/request_size/response_hash/response_size/signature/redaction/created_at`）；快照 ID 可由这些字段重算，不单独存列。API 返回的 `evidence_id` 是 `evidence_snapshots` 的行 ID。

## 5. 篡改检测

三个层面的篡改都必须能被发现：

1. **数据库字段被改**：`CreateEvidenceSnapshot` 只写签名；`verify` 接口用数据库字段重新 `Seal` 并比较签名，任一字段改动都会得到 `409 tampered`。
2. **正文被改或缺失**：`GetBlob` 重新解密并校验大小与 sha256，失败返回 `ErrTampered`，`verify` 接口映射为 `409 tampered`。
3. **签名本身被改**：签名必须是 64 位小写十六进制并在 `hmac.Equal` 中比对，失败同上。

采集未完成（`phase != 'result'`）时不能冻结证据，返回 `409 conflict`。

## 6. 脱敏

- `RedactHeaders`：敏感头（Authorization、Proxy-Authorization、Cookie、Set-Cookie、X-Api-Key、X-Auth-Token 等）整值替换为 `[REDACTED]`；返回副本，不修改入参。
- `RedactJSON`：递归遍历 JSON，键名（不区分大小写）包含 `password` / `passwd` / `token` / `secret` / `apikey` / `api_key` / `api-key` / `authorization` / `cookie` / `credential` 等片段时，其值整体替换为 `[REDACTED]`；使用 `UseNumber` 保留数字原文，输出压缩且稳定，重复脱敏结果不变。
- 已知取舍：
  - 键名片段匹配会**过度脱敏**（例如 `tokenizer` 也会被替换），对证据采集来说宁可多脱不可漏脱；
  - 执行器对工具输出做的是“尽力 JSON 脱敏”：`http_probe` 的输出是文本、`shell_exec` 输出也可能是纯文本，JSON 解析失败时**保留原文**，这是已知缺口（M5.1 补纯文本脱敏）；
  - M5 采集的是工具参数与输出，`RedactHeaders` 尚未接入真实代理链路（M5.1 MITM 时使用）。

## 7. HTTP 接口

三个接口都需要管理令牌：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/missions/{id}/traffic` | 任务采集记录列表，按 id 倒序，最多 200 条；返回 `{"traffic":[...]}` |
| POST | `/api/v1/traffic/{id}/evidence` | 把一条已完成的采集记录冻结为证据快照；成功返回 201 与快照 |
| GET | `/api/v1/evidence/{id}/verify` | 校验证据签名与正文哈希；成功返回 `{"verified":true,"evidence_id":…,"signature":…}` |

请求示例：

```bash
# 查看任务采集记录
curl -s "http://localhost:6969/api/v1/missions/1/traffic" \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"

# 冻结证据（请求体可为空）
curl -s -X POST "http://localhost:6969/api/v1/traffic/1/evidence" \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"

# 校验证据
curl -s "http://localhost:6969/api/v1/evidence/1/verify" \
  -H "Authorization: Bearer $BEACONX_ADMIN_TOKEN"
```

采集记录响应示例（节选）：

```json
{"traffic":[{"id":1,"mission_id":1,"tool":"http_probe","phase":"result","request_hash":"…","request_size":42,"response_hash":"…","response_size":128,"status_code":200,"redaction":"json-keys","created_at":"2026-10-21T07:00:00Z","completed_at":"2026-10-21T07:00:00Z"}]}
```

证据快照响应示例：

```json
{"tenant_id":1,"id":1,"mission_id":1,"tool":"http_probe","request_hash":"…","request_size":42,"response_hash":"…","response_size":128,"signature":"…","redaction":"json-keys","created_at":"2026-10-21T07:00:00Z"}
```

校验响应示例：

```json
{"verified":true,"evidence_id":1,"signature":"…"}
```

错误码：

- `400 bad_request`：id 非法。
- `404 not_found`：任务、采集记录或证据不存在。
- `409 conflict`：采集尚未完成（`phase != 'result'`）。
- `409 tampered`：签名或正文哈希校验失败（数据库字段、blob 或签名被改动）。
- `503 evidence_unavailable`：证据存储未装配（冻结与校验接口）；采集列表在运行时未就绪时返回 `runtime_unavailable`。
- `401 unauthorized`：缺少令牌或令牌错误。

## 8. 配置

| 配置项 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `evidence.dir` | `BEACONX_EVIDENCE_DIR` | 空 | 证据根目录；为空时使用 `<state.dir>/tenants/<tenant_id>/evidence` |
| `evidence.key_hex` | `BEACONX_EVIDENCE_KEY` | 空 | 64 位十六进制主密钥；为空时使用/生成 `<state.dir>/evidence.key` |

`evidence.key_hex` 非空但格式非法会在启动校验阶段失败。密钥文件不存在时自动生成；生成与读取过程不会打印密钥。

## 9. 测试与验收

- **evidence 包测试 36 个**：blob 往返、文件形状与权限、幂等复用、并发写同一内容、13 类篡改场景（截断/翻转/换密钥/改哈希/丢文件/错误 AAD 等）、快照签名往返、时间规范化、JSON 往返后验签、跨密钥验签失败、脱敏头与 JSON 递归用例。
- **执行器集成测试**：工具调用触发两阶段采集（intent→result，正文可解密校验）；篡改数据库字段或 blob 后 `verify` 返回 409。
- **运行时测试**：token 预算超限时引擎把任务置为 `paused`（不硬失败），可用 `resume` 继续。
- **接口验收**：POST 快照后 `verify` 正常返回 verified；改数据库字段或改 blob 文件后返回 409 `tampered`。
- **完整测试**：`go test ./...` 共 17 个测试包全部通过（数据库相关测试需要 `BEACONX_TEST_PG_DSN`）。

## 10. M5.1 延期项

- MITM 代理与真实网络流量采集（当前只采集工具参数与输出）。
- 漏洞复测（retester）与证据版本令牌的自动流转。
- 证据密钥轮换与多密钥并存。
- 纯文本输出与 HTTP 头的脱敏接入（`RedactHeaders` 目前仅提供工具函数）。
- 证据保留策略/GC 与批量导出（未在本里程碑实现）。

## 11. B4/B5 证据增强与风险

- **导出**：`POST /api/v1/evidence/{id}/export` 返回 zip（snapshot.json + 请求/响应正文），成功后写 `evidence_access_log`（action=export）；导出前会校验 blob 哈希/签名。
- **审计**：`GET /api/v1/evidence/{id}/audit` 读取 `evidence_access_log`（动作 read/verify/export）；当前只有 export/verify 部分路径埋点，正文读取与既有 `GET /evidence/{id}/verify` 尚未全量记录（🟡）。
- **密钥轮换（⬜ 未实现）**：`POST /api/v1/evidence/rotate` 当前返回 **501**；原因是 blob 没有 key-id 前缀，无法区分新旧密钥并安全重写，需要先做密钥版本化。
- **复测执行**：`POST /api/v1/retests/{id}/run` 会重算快照签名并解密校验关联证据（不是重跑 exploit），成功→passed、失败→failed、无证据→error。
- **风险提示**：流量重放（`/traffic/{id}/replay`）绕过 executor 的策略/审批，属于 🟡 高风险能力；HAR 导出是合成格式（不是真实 pcap）。

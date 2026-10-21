# 配置说明

## 加载顺序

1. 内置默认值
2. 配置文件（`BEACONX_CONFIG`，默认 `./config.json`；不存在则跳过）
3. 环境变量（`BEACONX_*`，逐项覆盖）

配置在启动时解析一次；业务配置（Agent、模型、策略、推送等）存放在数据库，通过 UI 修改，不需要重启。

## 配置文件

完整示例见仓库根目录的 [config.example.json](../config.example.json)。

```json
{
  "server": {
    "addr": ":6969",
    "read_header_timeout": "10s",
    "idle_timeout": "60s",
    "request_timeout": "30s",
    "shutdown_timeout": "15s",
    "max_body_bytes": 8388608
  },
  "database": {
    "dsn": "postgres://user:pass@127.0.0.1:5432/beaconx?sslmode=disable",
    "max_conns": 10,
    "min_conns": 1,
    "connect_timeout": "8s"
  },
  "tenant": { "default_id": 1 },
  "state": { "dir": "data" },
  "log": { "level": "info", "format": "json" }
}
```

## 环境变量

| 变量 | 对应配置 | 默认值 | 备注 |
| --- | --- | --- | --- |
| `BEACONX_CONFIG` | — | `config.json` | 配置文件路径 |
| `BEACONX_ADDR` | `server.addr` | `:6969` | 监听地址 |
| `BEACONX_REQUEST_TIMEOUT` | `server.request_timeout` | `30s` | 请求上下文超时 |
| `BEACONX_SHUTDOWN_TIMEOUT` | `server.shutdown_timeout` | `15s` | 优雅退出等待上限 |
| `BEACONX_DB_DSN` | `database.dsn` | 空（必填） | PostgreSQL 连接串 |
| `BEACONX_DB_MAX_CONNS` | `database.max_conns` | `10` | 连接池上限 |
| `BEACONX_DB_MIN_CONNS` | `database.min_conns` | `1` | 连接池保底 |
| `BEACONX_TENANT_DEFAULT_ID` | `tenant.default_id` | `1` | 单租户默认 ID |
| `BEACONX_HOME` | `state.dir` | `data` | 状态目录 |
| `BEACONX_LOG_LEVEL` | `log.level` | `info` | debug/info/warn/error |
| `BEACONX_LOG_FORMAT` | `log.format` | `json` | json/text |

启动时会校验配置；非法值直接退出并给出一条可读错误，不做静默降级。

## 状态目录

```
$BEACONX_HOME/
  jwt.key                 登录签名密钥（后续里程碑生成）
  tenants/<id>/sessions/  Agent 会话与执行记录
  tenants/<id>/evidence/  不可变证据快照
  tenants/<id>/traffic/   采集热数据（可丢弃）
  skills/                 技能包
  cache/                  可重建缓存
```

- 目录权限创建为 `0700`；Windows 上以部署文档中的 ACL 指引为准。
- v1 只有一个真实租户，但路径已按租户分区，后续开启多租户不需要搬迁文件。

## 数据库与迁移

- 迁移文件在 `migrations/`，命名 `NNNN_name.sql`，随二进制嵌入。
- 启动时（`serve`）或显式执行 `beaconx migrate` 时应用：先获取会话级咨询锁，再逐个在事务中执行未应用的版本，并记录校验和。
- **已发布的迁移不可修改**：校验和不匹配会拒绝启动，避免「代码与库结构悄悄分叉」。
- 迁移失败会终止启动；不存在「尽力而为」模式。

## 探针

| 端点 | 用途 | 判定 |
| --- | --- | --- |
| `/healthz` | 存活 | 进程可响应即 200 |
| `/readyz` | 就绪 | 数据库可连接且 schema 版本不低于二进制内置版本；否则 503 |

容器镜像的 `HEALTHCHECK` 使用 `/readyz`。

// BeaconX HTTP API 的响应类型。
//
// 来源：internal/infra/pg/{models,policy_store,evidence_store}.go 与
// internal/app/system/service.go 的 JSON 标签。字段名与后端保持一致（snake_case），
// 不做前端侧重命名，避免与真实接口悄悄漂移。

/** 任务状态取值（migrations/0003_runtime.sql 的 CHECK 约束）。 */
export type MissionStatus =
  | "queued"
  | "running"
  | "paused"
  | "finalizing"
  | "done"
  | "failed"
  | "cancelled";

/** 一次授权测试任务。 */
export interface Mission {
  tenant_id: number;
  id: number;
  name: string;
  goal: string;
  description: string;
  status: MissionStatus | string;
  queued: boolean;
  queued_at?: string | null;
  started_at?: string | null;
  finished_at?: string | null;
  deadline_at?: string | null;
  reason: string;
  max_tokens: number;
  spent_tokens: number;
  max_steps: number;
  steps_used: number;
  created_at: string;
  updated_at: string;
}

/** 创建任务请求体（对应 internal/httpapi/missions.go 的 missionCreateRequest）。 */
export interface CreateMissionInput {
  name: string;
  goal: string;
  description?: string;
  max_tokens?: number;
  max_steps?: number;
}

/** 审批状态：pending → approved / rejected / timeout。 */
export type ApprovalStatus = "pending" | "approved" | "rejected" | "timeout" | string;

/** 等待人工决策的工具调用。 */
export interface Approval {
  tenant_id: number;
  id: number;
  mission_id: number;
  node_id: number;
  tool: string;
  args: Record<string, unknown> | null;
  status: ApprovalStatus;
  note: string;
  decided_by: string;
  decided_at?: string | null;
  expires_at: string;
  created_at: string;
}

/** 两阶段采集：intent（请求已采集）→ result（执行完成）。 */
export type TrafficPhase = "intent" | "result" | string;

/** 一次工具调用的采集记录。 */
export interface TrafficExchange {
  tenant_id: number;
  id: number;
  mission_id: number;
  node_id?: number | null;
  tool: string;
  phase: TrafficPhase;
  request_hash: string;
  request_size: number;
  response_hash: string;
  response_size: number;
  status_code?: number | null;
  error: string;
  redaction: string;
  created_at: string;
  completed_at?: string | null;
}

/** 冻结后的证据快照（元数据 + HMAC 签名，正文按内容哈希存储）。 */
export interface EvidenceSnapshot {
  tenant_id: number;
  id: number;
  mission_id: number;
  tool: string;
  request_hash: string;
  request_size: number;
  response_hash: string;
  response_size: number;
  signature: string;
  redaction: string;
  created_at: string;
}

/** GET /api/v1/evidence/{id}/verify 成功响应。 */
export interface VerifyResult {
  verified: boolean;
  evidence_id: number;
  signature: string;
}

/** GET /healthz。 */
export interface HealthStatus {
  status: string;
}

/** /readyz 的单项检查。 */
export interface ReadinessCheck {
  name: string;
  ok: boolean;
  error?: string;
}

/** GET /readyz（服务未就绪时 HTTP 503，但响应体仍是该结构）。 */
export interface Readiness {
  ready: boolean;
  checks: ReadinessCheck[];
}

/** GET /api/v1/system/info。 */
export interface SystemInfo {
  service: string;
  version: string;
  commit: string;
  build_time: string;
  started_at: string;
  uptime: string;
  schema_version: number;
  latest_migration: number;
}

/** SSE 帧解析后的前端展示结构（不假设后端一定下发完整事件对象）。 */
export interface StreamEvent {
  key: string;
  id?: number;
  type: string;
  /** 事件自身的时间（完整事件对象里才有；负载帧通常没有）。 */
  createdAt?: string;
  /** 浏览器收到该帧的本地时间，保证列表一定有可读时间。 */
  receivedAt: string;
  detail: unknown;
}

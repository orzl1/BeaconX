// 与 Go 后端通信的唯一入口。
//
// 约定（见 README 与 internal/httpapi）：
//   - 默认同源 /api/v1：静态导出产物由 Go 服务托管时天然同源；开发模式由
//     next.config.mjs 的 rewrites 反代到本地后端。
//   - 可选 NEXT_PUBLIC_API_BASE 指向其他源（后端当前未提供 CORS，跨源需自行处理）。
//   - 令牌：Authorization: Bearer <token>；SSE 因为 EventSource 不能自定义请求头，
//     走 ?token=<token> 查询参数（后端两种方式都支持）。
//   - 错误响应统一为 {"error":{"code","message","request_id"}}。

import type {
  Approval,
  CreateMissionInput,
  EvidenceSnapshot,
  HealthStatus,
  Mission,
  Readiness,
  SystemInfo,
  TrafficExchange,
  VerifyResult,
} from "@/lib/types";

/** localStorage 中保存管理令牌的键名。 */
export const TOKEN_STORAGE_KEY = "beaconx.admin_token";

const RAW_API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? "";
/** 去掉末尾斜杠；为空串表示同源。 */
export const API_BASE = RAW_API_BASE.replace(/\/+$/, "");

/** 把 API 路径拼成可请求的 URL。 */
export function apiUrl(path: string): string {
  return `${API_BASE}${path}`;
}

/** 生成 SSE 地址：EventSource 无法设置请求头，令牌走查询参数。 */
export function sseUrl(path: string, token: string): string {
  const separator = path.includes("?") ? "&" : "?";
  return `${apiUrl(path)}${separator}token=${encodeURIComponent(token)}`;
}

interface ErrorDetail {
  code: string;
  message: string;
  requestId?: string;
}

/** 后端错误响应解析结果；解析失败时返回 null。 */
function parseErrorEnvelope(payload: unknown): ErrorDetail | null {
  if (!payload || typeof payload !== "object") return null;
  const envelope = payload as { error?: unknown };
  if (!envelope.error || typeof envelope.error !== "object") return null;
  const detail = envelope.error as { code?: unknown; message?: unknown; request_id?: unknown };
  const code = typeof detail.code === "string" ? detail.code : "";
  const message = typeof detail.message === "string" ? detail.message : "";
  if (!code && !message) return null;
  return {
    code,
    message,
    requestId: typeof detail.request_id === "string" ? detail.request_id : undefined,
  };
}

/** API 调用失败。status=0 表示网络层失败（未拿到 HTTP 响应）。 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;
  /** 原始响应体（JSON 已解析；非 JSON 为字符串；空响应为 null）。 */
  readonly payload: unknown;

  constructor(status: number, code: string, message: string, requestId: string | undefined, payload: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
    this.payload = payload;
  }
}

/** 是否是"令牌无效/缺失"类错误。 */
export function isAuthError(error: unknown): error is ApiError {
  return error instanceof ApiError && error.status === 401;
}

/** 把任意异常转成可直接展示的中文文案。 */
export function describeError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 0) return error.message;
    const suffix = error.code ? `（${error.code}${error.requestId ? `，请求 ${error.requestId}` : ""}）` : "";
    return `${error.message}${suffix}`;
  }
  if (error instanceof Error) return error.message;
  return String(error);
}

interface RequestOptions {
  token?: string;
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  body?: unknown;
  signal?: AbortSignal;
}

/** 底层 JSON 请求。非 2xx 抛 ApiError；204 返回 undefined。 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { token, method = "GET", body, signal } = options;
  const headers: Record<string, string> = { Accept: "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";

  let response: Response;
  try {
    response = await fetch(apiUrl(path), {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
      cache: "no-store",
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") throw error;
    throw new ApiError(0, "network_error", "无法连接后端服务：请确认 BeaconX 已启动且地址可访问", undefined, null);
  }

  if (response.status === 204) return undefined as T;

  const text = await response.text();
  let payload: unknown = null;
  if (text) {
    try {
      payload = JSON.parse(text) as unknown;
    } catch {
      payload = text;
    }
  }

  if (!response.ok) {
    const detail = parseErrorEnvelope(payload);
    if (detail) {
      throw new ApiError(response.status, detail.code, detail.message, detail.requestId, payload);
    }
    throw new ApiError(
      response.status,
      "",
      `${method} ${path} 请求失败（HTTP ${response.status}）`,
      undefined,
      payload,
    );
  }

  return payload as T;
}

// ---- 健康与系统信息（公开，无需令牌） ----

export function getHealth(): Promise<HealthStatus> {
  return request<HealthStatus>("/healthz");
}

/** /readyz 未就绪时返回 503，但响应体仍是 Readiness；调用方需要容错解析。 */
export function getReadiness(): Promise<Readiness> {
  return request<Readiness>("/readyz");
}

export function getSystemInfo(): Promise<SystemInfo> {
  return request<SystemInfo>("/api/v1/system/info");
}

// ---- 任务 ----

export function listMissions(token: string, signal?: AbortSignal): Promise<{ missions: Mission[] }> {
  return request<{ missions: Mission[] }>("/api/v1/missions?limit=100", { token, signal });
}

export function getMission(token: string, missionId: number, signal?: AbortSignal): Promise<Mission> {
  return request<Mission>(`/api/v1/missions/${missionId}`, { token, signal });
}

export function createMission(token: string, input: CreateMissionInput): Promise<Mission> {
  return request<Mission>("/api/v1/missions", { token, method: "POST", body: input });
}

/** action 只能是 pause / resume / cancel（后端 API 面）。 */
export function missionAction(
  token: string,
  missionId: number,
  action: "pause" | "resume" | "cancel",
): Promise<Mission> {
  return request<Mission>(`/api/v1/missions/${missionId}/${action}`, { token, method: "POST", body: {} });
}

/** 删除任务：后端级联删除数据，成功返回 204。 */
export function deleteMission(token: string, missionId: number): Promise<void> {
  return request<void>(`/api/v1/missions/${missionId}`, { token, method: "DELETE" });
}

// ---- 审批 ----

export function listApprovals(token: string, status = "pending", signal?: AbortSignal): Promise<{ approvals: Approval[] }> {
  return request<{ approvals: Approval[] }>(
    `/api/v1/approvals?status=${encodeURIComponent(status)}&limit=100`,
    { token, signal },
  );
}

export function decideApproval(
  token: string,
  approvalId: number,
  approve: boolean,
  note: string,
): Promise<Approval> {
  return request<Approval>(`/api/v1/approvals/${approvalId}/decide`, {
    token,
    method: "POST",
    body: { approve, note },
  });
}

// ---- 流量与证据 ----

export function listTraffic(token: string, missionId: number, signal?: AbortSignal): Promise<{ traffic: TrafficExchange[] }> {
  return request<{ traffic: TrafficExchange[] }>(`/api/v1/missions/${missionId}/traffic`, { token, signal });
}

/** 把已完成的采集记录冻结为证据快照（201）。 */
export function freezeEvidence(token: string, trafficId: number): Promise<EvidenceSnapshot> {
  return request<EvidenceSnapshot>(`/api/v1/traffic/${trafficId}/evidence`, { token, method: "POST", body: {} });
}

/** 校验证据签名与正文哈希；被篡改时后端返回 409 tampered（抛 ApiError）。 */
export function verifyEvidence(token: string, evidenceId: number): Promise<VerifyResult> {
  return request<VerifyResult>(`/api/v1/evidence/${evidenceId}/verify`, { token });
}

// ---- 自更新（后端尚未实现；前端按 404/405/501 容错展示"暂不可用"） ----

export function checkUpdate(token: string): Promise<unknown> {
  return request<unknown>("/api/v1/system/update/check", { token });
}

export function applyUpdate(token: string): Promise<unknown> {
  return request<unknown>("/api/v1/system/update/apply", { token, method: "POST", body: {} });
}

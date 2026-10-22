// 与 Go 后端通信的唯一入口。默认同源 /api/v1。
// 令牌走 Authorization: Bearer；SSE 走 ?token=。错误信封 {"error":{code,message,request_id}}。

import type {
  ActivityItem,
  Agent,
  AgentTestResult,
  Approval,
  Asset,
  AssetIdentity,
  AuditEvent,
  Company,
  CompanyScope,
  Coverage,
  CreateMissionInput,
  DashboardStats,
  EvidenceSnapshot,
  Finding,
  FindingEvidence,
  GraphView,
  HealthStatus,
  LLMExchangeDetail,
  LLMUsageRecord,
  LogEntry,
  McpServer,
  McpTool,
  Mission,
  MissionScopeEntry,
  MissionUsage,
  NotificationChannel,
  NotificationDelivery,
  OperationNode,
  PlanStep,
  PolicyRule,
  Readiness,
  Retest,
  RollbackResult,
  SettingsView,
  Skill,
  SkillFile,
  SkillVersion,
  SyncProject,
  SyncRun,
  SyncSource,
  SystemInfo,
  TrafficExchange,
  UpdateApplyResult,
  UpdateCheckResult,
  UsageGroup,
  VerifyResult,
  WorkspaceList,
} from "@/lib/types";

export const TOKEN_STORAGE_KEY = "beaconx.admin_token";
export const THEME_STORAGE_KEY = "beaconx.theme";

const RAW_API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? "";
export const API_BASE = RAW_API_BASE.replace(/\/+$/, "");

export function apiUrl(path: string): string {
  return `${API_BASE}${path}`;
}

export function sseUrl(path: string, token: string): string {
  const separator = path.includes("?") ? "&" : "?";
  return `${apiUrl(path)}${separator}token=${encodeURIComponent(token)}`;
}

export function encodePath(path: string): string {
  return path.split("/").map((part) => encodeURIComponent(part)).join("/");
}

export function qs(params: Record<string, string | number | boolean | undefined | null>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === "") continue;
    search.set(key, String(value));
  }
  const text = search.toString();
  return text ? `?${text}` : "";
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;
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

export function isAuthError(error: unknown): error is ApiError {
  return error instanceof ApiError && error.status === 401;
}

export function describeError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 0) return error.message;
    const suffix = error.code ? `（${error.code}${error.requestId ? `，请求 ${error.requestId}` : ""}）` : "";
    return `${error.message || "请求失败"}${suffix}`;
  }
  if (error instanceof Error) return error.message;
  return String(error);
}

function parseErrorEnvelope(payload: unknown): { code: string; message: string; requestId?: string } | null {
  if (!payload || typeof payload !== "object") return null;
  const envelope = payload as { error?: unknown };
  if (!envelope.error || typeof envelope.error !== "object") return null;
  const detail = envelope.error as { code?: unknown; message?: unknown; request_id?: unknown };
  const code = typeof detail.code === "string" ? detail.code : "";
  const message = typeof detail.message === "string" ? detail.message : "";
  if (!code && !message) return null;
  return { code, message, requestId: typeof detail.request_id === "string" ? detail.request_id : undefined };
}

interface RequestOptions {
  token?: string;
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  body?: unknown;
  signal?: AbortSignal;
  accept?: string;
}

async function send(path: string, options: RequestOptions = {}): Promise<Response> {
  const { token, method = "GET", body, signal, accept = "application/json" } = options;
  const headers: Record<string, string> = { Accept: accept };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  try {
    return await fetch(apiUrl(path), {
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
}

async function readPayload(response: Response): Promise<unknown> {
  if (response.status === 204) return undefined;
  const text = await response.text();
  if (!text) return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

function fail(response: Response, method: string, path: string, payload: unknown): never {
  const detail = parseErrorEnvelope(payload);
  if (detail) throw new ApiError(response.status, detail.code, detail.message, detail.requestId, payload);
  throw new ApiError(response.status, "", `${method} ${path} 请求失败（HTTP ${response.status}）`, undefined, payload);
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const response = await send(path, options);
  const payload = await readPayload(response);
  if (!response.ok) fail(response, options.method ?? "GET", path, payload);
  return payload as T;
}

export async function requestText(path: string, options: RequestOptions = {}): Promise<string> {
  const response = await send(path, { ...options, accept: options.accept ?? "*/*" });
  const text = await response.text();
  if (!response.ok) {
    let payload: unknown = text;
    try {
      payload = text ? (JSON.parse(text) as unknown) : null;
    } catch {
      payload = text;
    }
    fail(response, options.method ?? "GET", path, payload);
  }
  return text;
}

export async function downloadFile(path: string, token: string, fallbackName: string): Promise<void> {
  const response = await send(path, { token, accept: "*/*" });
  if (!response.ok) fail(response, "GET", path, await readPayload(response));
  const blob = await response.blob();
  const header = response.headers.get("Content-Disposition") ?? "";
  const matched = /filename\*=UTF-8''([^;]+)|filename="?([^";]+)"?/i.exec(header);
  const name = decodeURIComponent(matched?.[1] || matched?.[2] || fallbackName);
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = name;
  anchor.click();
  URL.revokeObjectURL(url);
}

export async function uploadFile<T>(path: string, token: string, file: File, fieldName = "file", query = ""): Promise<T> {
  const body = new FormData();
  body.append(fieldName, file, file.name);
  let response: Response;
  try {
    response = await fetch(apiUrl(`${path}${query}`), {
      method: "POST",
      headers: { Authorization: `Bearer ${token}`, Accept: "application/json" },
      body,
      cache: "no-store",
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") throw error;
    throw new ApiError(0, "network_error", "无法连接后端服务：请确认 BeaconX 已启动且地址可访问", undefined, null);
  }
  const payload = await readPayload(response);
  if (!response.ok) fail(response, "POST", path, payload);
  return payload as T;
}

export function getHealth(): Promise<HealthStatus> {
  return request("/healthz");
}

export async function getReadiness(): Promise<Readiness> {
  try {
    return await request<Readiness>("/readyz");
  } catch (error) {
    if (error instanceof ApiError && error.payload && typeof error.payload === "object" && "checks" in error.payload) {
      return error.payload as Readiness;
    }
    throw error;
  }
}

export function getSystemInfo(): Promise<SystemInfo> {
  return request("/api/v1/system/info");
}

export function listMissions(token: string, query: { status?: string; limit?: number } = {}, signal?: AbortSignal) {
  return request<{ missions: Mission[] }>(`/api/v1/missions${qs({ status: query.status, limit: query.limit ?? 100 })}`, { token, signal });
}
export function getMission(token: string, id: number, signal?: AbortSignal) {
  return request<Mission>(`/api/v1/missions/${id}`, { token, signal });
}
export function createMission(token: string, input: CreateMissionInput) {
  return request<Mission>("/api/v1/missions", { token, method: "POST", body: input });
}
export function missionAction(token: string, id: number, action: "pause" | "resume" | "cancel") {
  return request<Mission>(`/api/v1/missions/${id}/${action}`, { token, method: "POST", body: {} });
}
export function deleteMission(token: string, id: number) {
  return request<void>(`/api/v1/missions/${id}`, { token, method: "DELETE" });
}

export function listApprovals(token: string, query: { status?: string; limit?: number } = {}, signal?: AbortSignal) {
  return request<{ approvals: Approval[] }>(`/api/v1/approvals${qs({ status: query.status, limit: query.limit ?? 100 })}`, { token, signal });
}
export function getApproval(token: string, id: number) {
  return request<Approval>(`/api/v1/approvals/${id}`, { token });
}
export function decideApproval(token: string, id: number, approve: boolean, note: string) {
  return request<Approval>(`/api/v1/approvals/${id}/decide`, { token, method: "POST", body: { approve, note } });
}
export function cancelApproval(token: string, id: number) {
  return request<Approval>(`/api/v1/approvals/${id}/cancel`, { token, method: "POST", body: {} });
}

export function listMissionTraffic(token: string, missionId: number, signal?: AbortSignal) {
  return request<{ traffic: TrafficExchange[] }>(`/api/v1/missions/${missionId}/traffic`, { token, signal });
}
export function listTraffic(token: string, query: { mission_id?: number; tool?: string; phase?: string; limit?: number; after_id?: number }, signal?: AbortSignal) {
  return request<{ traffic: TrafficExchange[]; next_after_id?: number }>(`/api/v1/traffic${qs(query)}`, { token, signal });
}
export function trafficBody(token: string, id: number, part: "request" | "response") {
  return requestText(`/api/v1/traffic/${id}/${part}`, { token });
}
export function deleteTraffic(token: string, id: number) {
  return request<void>(`/api/v1/traffic/${id}`, { token, method: "DELETE" });
}
export function clearMissionTraffic(token: string, missionId: number) {
  return request<void>(`/api/v1/missions/${missionId}/traffic`, { token, method: "DELETE" });
}
export function replayTraffic(token: string, id: number) {
  return request<{ policy_gated: boolean; output: string; status: number; traffic_id: number }>(`/api/v1/traffic/${id}/replay`, { token, method: "POST", body: {} });
}
export function freezeEvidence(token: string, trafficId: number) {
  return request<EvidenceSnapshot>(`/api/v1/traffic/${trafficId}/evidence`, { token, method: "POST", body: {} });
}
export function verifyEvidence(token: string, evidenceId: number) {
  return request<VerifyResult>(`/api/v1/evidence/${evidenceId}/verify`, { token });
}
export function listEvidenceAudit(token: string, evidenceId: number) {
  return request<{ audit: Array<Record<string, unknown>> }>(`/api/v1/evidence/${evidenceId}/audit`, { token });
}
export function rotateEvidence(token: string, newKeyHex: string) {
  return request<{ rotated: number; resigned: number; notifications_rencrypted: number; notice: string }>("/api/v1/evidence/rotate", {
    token, method: "POST", body: { new_key_hex: newKeyHex },
  });
}

export function getDashboard(token: string, signal?: AbortSignal) {
  return request<DashboardStats>("/api/v1/dashboard", { token, signal });
}
export function listActivity(token: string, query: { limit?: number; before_id?: number; mission_id?: number } = {}, signal?: AbortSignal) {
  return request<{ activity: ActivityItem[]; next_before_id?: number }>(`/api/v1/activity${qs(query)}`, { token, signal });
}

export function listCompanies(token: string, query: { q?: string; limit?: number } = {}, signal?: AbortSignal) {
  return request<{ companies: Company[] }>(`/api/v1/companies${qs({ ...query, limit: query.limit ?? 100 })}`, { token, signal });
}
export function createCompany(token: string, input: { name: string; slug: string }) {
  return request<Company>("/api/v1/companies", { token, method: "POST", body: input });
}
export function deleteCompany(token: string, id: number) {
  return request<void>(`/api/v1/companies/${id}`, { token, method: "DELETE" });
}
export function updateCompany(token: string, id: number, input: { name?: string; slug?: string; attrs?: Record<string, unknown> | null }) {
  return request<Company>(`/api/v1/companies/${id}`, { token, method: "PATCH", body: input });
}
export function listCompanyScope(token: string, id: number, signal?: AbortSignal) {
  return request<{ scope: CompanyScope[] }>(`/api/v1/companies/${id}/scope`, { token, signal });
}
export function addCompanyScope(token: string, id: number, input: { kind: string; value: string; source?: string }) {
  return request<CompanyScope>(`/api/v1/companies/${id}/scope`, { token, method: "POST", body: input });
}
export function deleteCompanyScope(token: string, id: number, scopeId: number) {
  return request<void>(`/api/v1/companies/${id}/scope/${scopeId}`, { token, method: "DELETE" });
}

export function listAssets(token: string, query: { type?: string; q?: string; limit?: number; after_id?: number } = {}, signal?: AbortSignal) {
  return request<{ assets: Asset[]; next_after_id?: number }>(`/api/v1/assets${qs({ ...query, limit: query.limit ?? 100 })}`, { token, signal });
}
export function createAsset(token: string, input: { type: string; label: string; parent_id?: number | null; identities?: AssetIdentity[] }) {
  return request<Asset>("/api/v1/assets", { token, method: "POST", body: input });
}
export function updateAsset(token: string, id: number, input: { label?: string; parent_id?: number | null; attrs?: Record<string, unknown> }) {
  return request<Asset>(`/api/v1/assets/${id}`, { token, method: "PATCH", body: input });
}
export function deleteAsset(token: string, id: number) {
  return request<void>(`/api/v1/assets/${id}`, { token, method: "DELETE" });
}
export function addAssetIdentity(token: string, id: number, input: { kind: string; key: string }) {
  return request<AssetIdentity>(`/api/v1/assets/${id}/identities`, { token, method: "POST", body: input });
}
export function deleteAssetIdentity(token: string, id: number, kind: string, key: string) {
  return request<void>(`/api/v1/assets/${id}/identities/${encodeURIComponent(kind)}/${encodePath(key)}`, { token, method: "DELETE" });
}
export function listMissionAssets(token: string, missionId: number, signal?: AbortSignal) {
  return request<{ assets: Asset[] }>(`/api/v1/missions/${missionId}/assets`, { token, signal });
}
export function addMissionAsset(token: string, missionId: number, input: { asset_id: number; source?: string }) {
  return request<{ mission_id: number; asset_id: number; source: string }>(`/api/v1/missions/${missionId}/assets`, { token, method: "POST", body: input });
}
export function removeMissionAsset(token: string, missionId: number, assetId: number) {
  return request<void>(`/api/v1/missions/${missionId}/assets/${assetId}`, { token, method: "DELETE" });
}
export function getCoverage(token: string, missionId: number, signal?: AbortSignal) {
  return request<Coverage>(`/api/v1/missions/${missionId}/coverage`, { token, signal });
}

export function listMissionScope(token: string, missionId: number, signal?: AbortSignal) {
  return request<{ scope: MissionScopeEntry[] }>(`/api/v1/missions/${missionId}/scope`, { token, signal });
}
export function addMissionScope(token: string, missionId: number, input: { kind: string; value: string; note?: string }) {
  return request<MissionScopeEntry>(`/api/v1/missions/${missionId}/scope`, { token, method: "POST", body: input });
}
export function deleteMissionScope(token: string, missionId: number, scopeId: number) {
  return request<void>(`/api/v1/missions/${missionId}/scope/${scopeId}`, { token, method: "DELETE" });
}
export function compileScope(token: string, missionId: number) {
  return request<{ mission_id: number; compiled_rules: unknown[]; notes: string[] }>(`/api/v1/missions/${missionId}/scope/compile`, { token, method: "POST", body: {} });
}
export function getPlan(token: string, missionId: number, signal?: AbortSignal) {
  return request<{ steps: PlanStep[] }>(`/api/v1/missions/${missionId}/plan`, { token, signal });
}
export function getGraph(token: string, missionId: number, query: { kind?: string; state?: string; limit?: number; after_id?: number } = {}, signal?: AbortSignal) {
  return request<GraphView>(`/api/v1/missions/${missionId}/graph${qs(query)}`, { token, signal });
}
export function getNode(token: string, id: number) {
  return request<OperationNode>(`/api/v1/nodes/${id}`, { token });
}
export function patchNodeState(token: string, id: number, state: string) {
  return request<OperationNode>(`/api/v1/nodes/${id}/state`, { token, method: "PATCH", body: { state } });
}

export function listFindings(token: string, query: { mission_id?: number; severity?: string; status?: string; asset_id?: number; limit?: number; after_id?: number } = {}, signal?: AbortSignal) {
  return request<{ findings: Finding[]; next_after_id?: number }>(`/api/v1/findings${qs({ ...query, limit: query.limit ?? 100 })}`, { token, signal });
}
export function listMissionFindings(token: string, missionId: number, query: { severity?: string; status?: string; limit?: number } = {}, signal?: AbortSignal) {
  return request<{ findings: Finding[] }>(`/api/v1/missions/${missionId}/findings${qs({ ...query, limit: query.limit ?? 100 })}`, { token, signal });
}
export function createFinding(token: string, missionId: number, input: { title: string; severity?: string; detail?: string; asset_id?: number; node_id?: number }) {
  return request<Finding>(`/api/v1/missions/${missionId}/findings`, { token, method: "POST", body: input });
}
export function createFindingFromNode(token: string, missionId: number, input: { node_id: number; title?: string; severity?: string }) {
  return request<Finding>(`/api/v1/missions/${missionId}/findings/from-node`, { token, method: "POST", body: input });
}
export function getFinding(token: string, id: number, signal?: AbortSignal) {
  return request<Finding>(`/api/v1/findings/${id}`, { token, signal });
}
export function updateFinding(token: string, id: number, input: { title?: string; severity?: string; detail?: string; asset_id?: number | null; node_id?: number | null }) {
  return request<Finding>(`/api/v1/findings/${id}`, { token, method: "PATCH", body: input });
}
export function transitionFinding(token: string, id: number, status: string) {
  return request<Finding>(`/api/v1/findings/${id}/status`, { token, method: "POST", body: { status } });
}
export function bindFindingEvidence(token: string, id: number, snapshotId: number) {
  return request<{ finding_id: number; snapshot_id: number }>(`/api/v1/findings/${id}/evidence`, { token, method: "POST", body: { snapshot_id: snapshotId } });
}
export function unbindFindingEvidence(token: string, id: number, snapshotId: number) {
  return request<void>(`/api/v1/findings/${id}/evidence/${snapshotId}`, { token, method: "DELETE" });
}

export function listRetests(token: string, missionId: number, signal?: AbortSignal) {
  return request<{ retests: Retest[] }>(`/api/v1/missions/${missionId}/retests?limit=100`, { token, signal });
}
export function createRetest(token: string, missionId: number, input: { finding_id: number; note?: string }) {
  return request<Retest>(`/api/v1/missions/${missionId}/retests`, { token, method: "POST", body: input });
}
export function patchRetest(token: string, id: number, input: { status: string; note?: string }) {
  return request<Retest>(`/api/v1/retests/${id}`, { token, method: "PATCH", body: input });
}
export function runRetest(token: string, id: number) {
  return request<Retest>(`/api/v1/retests/${id}/run`, { token, method: "POST", body: {} });
}
export function listRetestEvidence(token: string, id: number) {
  return request<{ evidence: Array<{ snapshot_id: number }> }>(`/api/v1/retests/${id}/evidence`, { token });
}
export function missionReport(token: string, missionId: number, format?: string) {
  return requestText(`/api/v1/missions/${missionId}/report${qs({ format })}`, { token });
}

export function listCommands(token: string, query: { mission_id?: number; tool?: string; outcome?: string; limit?: number; after_id?: number } = {}, signal?: AbortSignal) {
  return request<{ commands: AuditEvent[]; next_after_id?: number }>(`/api/v1/commands${qs({ ...query, limit: query.limit ?? 100 })}`, { token, signal });
}
export function getCommand(token: string, id: number) {
  return request<AuditEvent>(`/api/v1/commands/${id}`, { token });
}
export function missionAudit(token: string, missionId: number, signal?: AbortSignal) {
  return request<{ audit: AuditEvent[] }>(`/api/v1/missions/${missionId}/audit`, { token, signal });
}
export function listLLMRecords(token: string, query: { mission_id?: number; role?: string; model?: string; limit?: number; after_id?: number } = {}, signal?: AbortSignal) {
  return request<{ records: LLMUsageRecord[]; next_after_id?: number }>(`/api/v1/llm-records${qs({ ...query, limit: query.limit ?? 100 })}`, { token, signal });
}
export function getLLMRecord(token: string, id: number, signal?: AbortSignal) {
  return request<LLMExchangeDetail>(`/api/v1/llm-records/${id}`, { token, signal });
}
export function llmBody(token: string, id: number, part: "request" | "response") {
  return requestText(`/api/v1/llm-records/${id}/${part}`, { token });
}
export function missionUsage(token: string, missionId: number, signal?: AbortSignal) {
  return request<MissionUsage>(`/api/v1/missions/${missionId}/usage`, { token, signal });
}
export function llmSummary(token: string, query: { group_by?: string; days?: number } = {}, signal?: AbortSignal) {
  return request<{ group_by: string; groups: UsageGroup[] }>(`/api/v1/llm-usage/summary${qs(query)}`, { token, signal });
}

export function listPolicyRules(token: string, signal?: AbortSignal) {
  return request<{ rules: PolicyRule[] }>("/api/v1/policy/rules?limit=200", { token, signal });
}
export function createPolicyRule(token: string, input: { name: string; tool: string; arg_pattern: string; action: string; priority: number; timeout_seconds: number; enabled: boolean }) {
  return request<PolicyRule>("/api/v1/policy/rules", { token, method: "POST", body: input });
}
export function updatePolicyRule(token: string, id: number, input: Partial<Pick<PolicyRule, "enabled" | "name" | "tool" | "arg_pattern" | "action" | "priority" | "timeout_seconds">>) {
  return request<PolicyRule>(`/api/v1/policy/rules/${id}`, { token, method: "PATCH", body: input });
}
export function deletePolicyRule(token: string, id: number) {
  return request<void>(`/api/v1/policy/rules/${id}`, { token, method: "DELETE" });
}
export function simulatePolicy(token: string, input: { tool: string; args: Record<string, unknown> }) {
  return request<{ action: string; rule_id: number; rule_name: string; reason: string }>("/api/v1/policy/simulate", { token, method: "POST", body: input });
}

export function getSettings(token: string, signal?: AbortSignal) {
  return request<{ settings: SettingsView }>("/api/v1/settings", { token, signal });
}
export function patchSettings(token: string, settings: SettingsView) {
  return request<{ settings: SettingsView }>("/api/v1/settings", { token, method: "PATCH", body: { settings } });
}
export function listLogs(token: string, query: { level?: string; limit?: number } = {}, signal?: AbortSignal) {
  return request<{ logs: LogEntry[] }>(`/api/v1/logs${qs({ ...query, limit: query.limit ?? 200 })}`, { token, signal });
}

export function listChannels(token: string, signal?: AbortSignal) {
  return request<{ channels: NotificationChannel[] }>("/api/v1/notifications/channels?limit=100", { token, signal });
}
export function createChannel(token: string, input: { name: string; type: string; config: Record<string, unknown>; enabled: boolean }) {
  return request<NotificationChannel>("/api/v1/notifications/channels", { token, method: "POST", body: input });
}
export function updateChannel(token: string, id: number, input: { name?: string; config?: Record<string, unknown> | null; enabled?: boolean }) {
  return request<NotificationChannel>(`/api/v1/notifications/channels/${id}`, { token, method: "PATCH", body: input });
}
export function deleteChannel(token: string, id: number) {
  return request<void>(`/api/v1/notifications/channels/${id}`, { token, method: "DELETE" });
}
export function testChannel(token: string, id: number) {
  return request<{ sent: boolean; channel_id: number; delivery: NotificationDelivery }>(`/api/v1/notifications/channels/${id}/test`, { token, method: "POST", body: {} });
}
export function listDeliveries(token: string, signal?: AbortSignal) {
  return request<{ deliveries: NotificationDelivery[] }>("/api/v1/notifications/deliveries?limit=100", { token, signal });
}

export function listMcpServers(token: string, signal?: AbortSignal) {
  return request<{ servers: McpServer[] }>("/api/v1/mcp/servers?limit=100", { token, signal });
}
export function createMcpServer(token: string, input: Record<string, unknown>) {
  return request<McpServer>("/api/v1/mcp/servers", { token, method: "POST", body: input });
}
export function updateMcpServer(token: string, id: number, input: { name?: string; url?: string; command?: string; enabled?: boolean }) {
  return request<McpServer>(`/api/v1/mcp/servers/${id}`, { token, method: "PATCH", body: input });
}
export function deleteMcpServer(token: string, id: number) {
  return request<void>(`/api/v1/mcp/servers/${id}`, { token, method: "DELETE" });
}
export function refreshMcpServer(token: string, id: number) {
  return request<{ tools: McpTool[] }>(`/api/v1/mcp/servers/${id}/refresh`, { token, method: "POST", body: {} });
}
export function listMcpTools(token: string, serverId?: number, signal?: AbortSignal) {
  return request<{ tools: McpTool[] }>(`/api/v1/mcp/tools${qs({ server_id: serverId })}`, { token, signal });
}

export function listSkills(token: string, signal?: AbortSignal) {
  return request<{ skills: Skill[] }>("/api/v1/skills?limit=100", { token, signal });
}
export function createSkill(token: string, input: { name: string; description: string; version?: string }) {
  return request<Skill>("/api/v1/skills", { token, method: "POST", body: input });
}
export function updateSkill(token: string, name: string, input: { description?: string; version?: string }) {
  return request<Skill>(`/api/v1/skills/${encodeURIComponent(name)}`, { token, method: "PATCH", body: input });
}
export function deleteSkill(token: string, name: string) {
  return request<void>(`/api/v1/skills/${encodeURIComponent(name)}`, { token, method: "DELETE" });
}
export function deleteSkillFile(token: string, name: string, path: string) {
  return request<void>(`/api/v1/skills/${encodeURIComponent(name)}/files/${encodePath(path)}`, { token, method: "DELETE" });
}
export function listSkillFiles(token: string, name: string, signal?: AbortSignal) {
  return request<{ files: SkillFile[] }>(`/api/v1/skills/${encodeURIComponent(name)}/files`, { token, signal });
}
export function readSkillFile(token: string, name: string, path: string) {
  return request<{ path: string; content: string }>(`/api/v1/skills/${encodeURIComponent(name)}/files/${encodePath(path)}`, { token });
}
export function writeSkillFile(token: string, name: string, path: string, content: string) {
  return request<{ path: string; size: number }>(`/api/v1/skills/${encodeURIComponent(name)}/files/${encodePath(path)}`, { token, method: "PUT", body: { content } });
}
export function listSkillVersions(token: string, name: string, signal?: AbortSignal) {
  return request<{ versions: SkillVersion[] }>(`/api/v1/skills/${encodeURIComponent(name)}/versions`, { token, signal });
}
export function restoreSkillVersion(token: string, name: string, timestamp: string) {
  return request<Skill>(`/api/v1/skills/${encodeURIComponent(name)}/versions/${encodeURIComponent(timestamp)}/restore`, { token, method: "POST", body: {} });
}

export function listAgents(token: string, signal?: AbortSignal) {
  return request<{ agents: Agent[] }>("/api/v1/agents?limit=100", { token, signal });
}
export function getAgent(token: string, key: string, signal?: AbortSignal) {
  return request<Agent>(`/api/v1/agents/${encodeURIComponent(key)}`, { token, signal });
}
export function createAgent(token: string, input: { key: string; name: string; role: string; tools: string[]; system_prompt: string; model: string; enabled: boolean }) {
  return request<Agent>("/api/v1/agents", { token, method: "POST", body: input });
}
export function updateAgent(token: string, key: string, input: Partial<Agent>) {
  return request<Agent>(`/api/v1/agents/${encodeURIComponent(key)}`, { token, method: "PATCH", body: input });
}
export function deleteAgent(token: string, key: string) {
  return request<void>(`/api/v1/agents/${encodeURIComponent(key)}`, { token, method: "DELETE" });
}
export function testAgent(token: string, key: string) {
  return request<AgentTestResult>(`/api/v1/agents/${encodeURIComponent(key)}/test`, { token, method: "POST", body: {} });
}

export function listWorkspace(token: string, missionId: number, path = "", signal?: AbortSignal) {
  return request<WorkspaceList>(`/api/v1/workspace/files${qs({ mission_id: missionId, path, limit: 500 })}`, { token, signal });
}
export function readWorkspace(token: string, missionId: number, path: string) {
  return request<{ path: string; content: string }>(`/api/v1/workspace/files/${encodePath(path)}${qs({ mission_id: missionId })}`, { token });
}
export function writeWorkspace(token: string, missionId: number, path: string, content: string) {
  return request<{ path: string; size: number }>(`/api/v1/workspace/files/${encodePath(path)}${qs({ mission_id: missionId })}`, { token, method: "PUT", body: { content } });
}
export function deleteWorkspace(token: string, missionId: number, path: string, recursive = false) {
  return request<void>(`/api/v1/workspace/files/${encodePath(path)}${qs({ mission_id: missionId, recursive: recursive ? "1" : undefined })}`, { token, method: "DELETE" });
}

export function listSyncSources(token: string, signal?: AbortSignal) {
  return request<{ sources: SyncSource[] }>("/api/v1/sync/sources?limit=100", { token, signal });
}
export function createSyncSource(token: string, input: { name: string; type: string; config: Record<string, unknown>; enabled: boolean }) {
  return request<SyncSource>("/api/v1/sync/sources", { token, method: "POST", body: input });
}
export function updateSyncSource(token: string, id: number, input: { name?: string; type?: string; config?: Record<string, unknown>; enabled?: boolean }) {
  return request<SyncSource>(`/api/v1/sync/sources/${id}`, { token, method: "PATCH", body: input });
}
export function deleteSyncSource(token: string, id: number) {
  return request<void>(`/api/v1/sync/sources/${id}`, { token, method: "DELETE" });
}
export function listSyncProjects(token: string, sourceId?: number, signal?: AbortSignal) {
  return request<{ projects: SyncProject[] }>(`/api/v1/sync/projects${qs({ source_id: sourceId, limit: 100 })}`, { token, signal });
}
export function createSyncProject(token: string, input: { source_id: number; external_id: string; name: string; data?: Record<string, unknown> }) {
  return request<SyncProject>("/api/v1/sync/projects", { token, method: "POST", body: input });
}
export function runSync(token: string, input: { source_id?: number; project_id?: number }) {
  return request<SyncRun>("/api/v1/sync/run", { token, method: "POST", body: input });
}
export function listSyncRuns(token: string, signal?: AbortSignal) {
  return request<{ runs: SyncRun[] }>("/api/v1/sync/runs?limit=50", { token, signal });
}

export function checkUpdate(token: string) {
  return request<UpdateCheckResult>("/api/v1/system/update/check", { token });
}
export function applyUpdate(token: string) {
  return request<UpdateApplyResult>("/api/v1/system/update/apply", { token, method: "POST", body: {} });
}
export function rollbackUpdate(token: string) {
  return request<RollbackResult>("/api/v1/system/update/rollback", { token, method: "POST", body: {} });
}

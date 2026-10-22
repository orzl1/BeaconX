// BeaconX HTTP 响应类型。字段名与后端 JSON 保持一致。

export interface Mission {
  tenant_id: number;
  id: number;
  name: string;
  goal: string;
  description: string;
  status: string;
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

export interface CreateMissionInput {
  name: string;
  goal: string;
  description?: string;
  max_tokens?: number;
  max_steps?: number;
}

export interface Approval {
  tenant_id: number;
  id: number;
  mission_id: number;
  node_id: number;
  tool: string;
  args: Record<string, unknown> | null;
  status: string;
  note: string;
  decided_by: string;
  decided_at?: string | null;
  expires_at: string;
  created_at: string;
  node?: OperationNode | null;
  mission?: Mission | null;
}

export interface AuditEvent {
  tenant_id: number;
  id: number;
  mission_id: number;
  node_id?: number | null;
  tool: string;
  action: string;
  rule_name: string;
  args_digest: string;
  outcome: string;
  detail: string;
  actor: string;
  created_at: string;
}

export interface TrafficExchange {
  tenant_id: number;
  id: number;
  mission_id: number;
  node_id?: number | null;
  tool: string;
  phase: string;
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

export interface VerifyResult {
  verified: boolean;
  evidence_id: number;
  signature: string;
}

export interface HealthStatus {
  status: string;
}

export interface Readiness {
  ready: boolean;
  checks: Array<{ name: string; ok: boolean; error?: string }>;
}

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

export interface StreamEvent {
  key: string;
  id?: number;
  type: string;
  createdAt?: string;
  receivedAt: string;
  detail: unknown;
}

export interface Company {
  tenant_id: number;
  id: number;
  name: string;
  slug: string;
  attrs?: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
}

export interface CompanyScope {
  tenant_id: number;
  id: number;
  company_id: number;
  kind: string;
  value: string;
  source: string;
  created_at: string;
}

export interface AssetIdentity {
  tenant_id?: number;
  asset_id?: number;
  kind: string;
  key: string;
  created_at?: string;
}

export interface Asset {
  tenant_id: number;
  id: number;
  type: string;
  label: string;
  parent_id?: number | null;
  attrs?: Record<string, unknown> | null;
  identities?: AssetIdentity[];
  created_at: string;
  updated_at: string;
}

export interface Coverage {
  total: number;
  tested: number;
  percent: number;
  by_type: Array<{ type: string; total: number; tested: number; percent: number }>;
}

export interface MissionScopeEntry {
  tenant_id: number;
  id: number;
  mission_id: number;
  kind: string;
  value: string;
  allowed_actions: string[];
  note: string;
  created_at: string;
}

export interface PlanStep {
  tenant_id: number;
  id: number;
  mission_id: number;
  key: string;
  title: string;
  detail: string;
  status: string;
  priority: number;
  depends_on: string[];
  created_at: string;
  updated_at: string;
}

export interface OperationNode {
  tenant_id: number;
  id: number;
  mission_id: number;
  kind: string;
  state: string;
  title: string;
  body: string;
  payload?: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
}

export interface OperationEdge {
  tenant_id: number;
  id: number;
  mission_id: number;
  src_id: number;
  dst_id: number;
  rel: string;
  created_at: string;
}

export interface OperationAnchor {
  tenant_id: number;
  node_id: number;
  asset_id: number;
  role: string;
  created_at: string;
}

export interface GraphView {
  nodes: OperationNode[];
  edges: OperationEdge[];
  anchors: OperationAnchor[];
  next_after_id?: number;
}

export interface Finding {
  tenant_id: number;
  id: number;
  mission_id: number;
  asset_id?: number | null;
  node_id?: number | null;
  title: string;
  severity: string;
  status: string;
  detail: string;
  evidence_count: number;
  created_at: string;
  updated_at: string;
  evidence?: FindingEvidence[];
}

export interface FindingEvidence {
  tenant_id: number;
  finding_id: number;
  snapshot_id: number;
  created_at: string;
}

export interface Retest {
  tenant_id: number;
  id: number;
  mission_id: number;
  finding_id: number;
  status: string;
  note: string;
  requested_by: string;
  created_at: string;
  started_at?: string | null;
  finished_at?: string | null;
}

export interface LLMUsageRecord {
  tenant_id: number;
  id: number;
  mission_id?: number | null;
  role: string;
  model: string;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  created_at: string;
}

export interface LLMExchangeDetail {
  id: number;
  mission_id: number;
  role: string;
  model: string;
  error?: string;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  request_bytes: number;
  response_bytes: number;
  truncated: boolean;
  redacted: boolean;
  created_at: string;
}

export interface UsageGroup {
  key: string;
  calls: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
}

export interface MissionUsage {
  mission_id: number;
  max_tokens: number;
  spent_tokens: number;
  max_steps: number;
  steps_used: number;
  records: LLMUsageRecord[];
  by_role: Array<{ role: string; calls: number; total_tokens: number }>;
  by_model: Array<{ model: string; calls: number; total_tokens: number }>;
}

export interface DashboardHour {
  hour: string;
  missions_created: number;
  missions_done: number;
  tokens: number;
  approvals_requested: number;
}

export interface DashboardStats {
  missions: { total: number; by_status: Record<string, number> };
  tokens: { spent_total: number; calls: number; today: number };
  approvals: { pending: number };
  findings: { total: number; by_severity: Record<string, number> };
  assets: { total: number };
  traffic: { total: number };
  recent_missions: Array<{ id: number; name: string; status: string; spent_tokens: number; created_at: string }>;
  hourly?: DashboardHour[];
}

export interface ActivityItem {
  id: number;
  mission_id: number;
  type: string;
  payload?: Record<string, unknown> | null;
  created_at: string;
}

export interface PolicyRule {
  tenant_id: number;
  id: number;
  name: string;
  tool: string;
  arg_pattern: string;
  action: string;
  priority: number;
  timeout_seconds: number;
  enabled: boolean;
  immutable: boolean;
  created_at: string;
  updated_at: string;
}

export interface SettingsView {
  max_concurrent: number;
  default_budget: { max_tokens: number; max_steps: number };
  default_action: string;
  approval_timeout: string;
}

export interface LogEntry {
  id: number;
  time: string;
  level: string;
  msg: string;
  attrs: Record<string, unknown>;
}

export interface NotificationChannel {
  tenant_id: number;
  id: number;
  name: string;
  type: string;
  config: Record<string, unknown> | null;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface NotificationDelivery {
  tenant_id: number;
  id: number;
  channel_id: number;
  event: string;
  status: string;
  detail: string;
  created_at: string;
}

export interface McpServer {
  id: number;
  name: string;
  transport: string;
  url: string;
  command: string;
  args: string[] | null;
  env: Record<string, string> | null;
  headers: Record<string, string> | null;
  enabled: boolean;
  last_status: string;
  last_error: string;
  last_refreshed_at?: string | null;
  created_at: string;
  updated_at: string;
}

export interface McpTool {
  server_id: number;
  name: string;
  description: string;
  input_schema?: Record<string, unknown> | null;
}

export interface Skill {
  tenant_id: number;
  name: string;
  description: string;
  version: string;
  file_count: number;
  created_at: string;
  updated_at: string;
}

export interface SkillFile {
  path: string;
  size: number;
  updated_at?: string;
}

export interface SkillVersion {
  timestamp: string;
  files: number;
  size: number;
  has_meta?: boolean;
}

export interface Agent {
  tenant_id: number;
  id: number;
  key: string;
  name: string;
  role: string;
  system_prompt: string;
  tools: string[];
  model: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface AgentTestResult {
  key: string;
  valid_tools: string[];
  unknown_tools: string[];
  warnings: string[];
}

export interface WorkspaceFile {
  path: string;
  size: number;
  modified_at: string;
  is_dir: boolean;
}

export interface WorkspaceList {
  files: WorkspaceFile[];
  root: string;
  quota_bytes: number;
  used_bytes: number;
}

export interface SyncSource {
  tenant_id: number;
  id: number;
  name: string;
  type: string;
  config: Record<string, unknown> | null;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface SyncProject {
  tenant_id: number;
  id: number;
  source_id: number;
  external_id: string;
  name: string;
  data: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
}

export interface SyncRun {
  tenant_id: number;
  id: number;
  source_id: number;
  project_id?: number | null;
  status: string;
  stats: Record<string, unknown> | null;
  error: string;
  started_at?: string | null;
  finished_at?: string | null;
  created_at: string;
}

export interface UpdateCheckResult {
  has_update: boolean;
  release?: {
    version: string;
    tag: string;
    url: string;
    notes: string;
    published_at: string;
    asset_name: string;
  } | null;
}

export interface UpdateApplyResult {
  updated: boolean;
  version: string;
  message: string;
}

export interface RollbackResult {
  rolled_back: boolean;
  message: string;
}

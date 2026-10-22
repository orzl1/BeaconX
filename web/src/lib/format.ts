import type { Mission } from "@/lib/types";

export const MISSION_STATUSES = ["queued", "running", "paused", "finalizing", "done", "failed", "cancelled"] as const;
export const SEVERITIES = ["info", "low", "medium", "high", "critical"] as const;
export const FINDING_STATUSES = ["open", "confirmed", "retest", "fixed", "closed", "false_positive"] as const;
export const ASSET_TYPES = ["root_domain", "subdomain", "ip", "service", "app", "endpoint"] as const;
export const COMPANY_SCOPE_KINDS = ["root_domain", "subdomain", "ip", "cidr", "url", "keyword", "icp"] as const;
export const MISSION_SCOPE_KINDS = ["root_domain", "subdomain", "ip", "cidr", "url", "keyword"] as const;
export const NODE_KINDS = ["goal", "hypothesis", "action", "fact", "finding", "hint", "digest"] as const;
export const NODE_STATES = ["open", "running", "done", "exhausted", "blocked"] as const;

export const FINDING_NEXT: Record<string, string[]> = {
  open: ["confirmed", "false_positive", "closed"],
  confirmed: ["fixed", "retest", "closed"],
  retest: ["confirmed", "fixed", "closed"],
  fixed: ["retest", "closed"],
};

export const RETEST_NEXT: Record<string, string[]> = {
  pending: ["running"],
  running: ["passed", "failed", "error"],
};

const LABEL: Record<string, string> = {
  queued: "排队中", running: "运行中", paused: "已暂停", finalizing: "收尾中", done: "已完成", failed: "已失败", cancelled: "已取消",
  pending: "待审批", approved: "已批准", rejected: "已拒绝", timeout: "已超时",
  intent: "请求已采集", result: "已完成",
  info: "信息", low: "低", medium: "中", high: "高", critical: "严重",
  open: "待确认", confirmed: "已确认", retest: "复测中", fixed: "已修复", closed: "已关闭", false_positive: "误报",
  passed: "通过", error: "出错",
  root_domain: "根域", subdomain: "子域", ip: "IP", cidr: "CIDR", url: "URL", keyword: "关键词", icp: "备案", service: "服务", app: "应用", endpoint: "端点",
  goal: "目标", hypothesis: "假设", action: "动作", fact: "事实", finding: "发现", hint: "提示", digest: "折叠摘要",
  exhausted: "已穷尽", blocked: "受阻",
  spawns: "派生", derived_from: "推导自", yields: "产出", proves: "证明", covers: "折叠覆盖",
  target: "授权目标", touched: "接触过", tested: "已测试", evidence: "证据",
  ready: "就绪", skipped: "已跳过",
  executed: "已执行", tool_error: "工具错误", denied_by_rule: "规则拒绝", rejected_by_user: "人工拒绝", approval_unavailable: "审批不可用",
  allow: "放行", ask: "询问", deny: "拒绝",
  created: "创建", admitted: "准入", started: "启动", resumed: "恢复", completed: "完成", deleted: "删除", recovered: "恢复对账",
  approval_required: "需要审批", budget_exhausted: "Token 预算耗尽", step_budget_exhausted: "步骤预算耗尽",
  planner_error: "规划错误", planner_persist_error: "规划落库错误", planner_done_with_pending: "规划结束仍有待办",
};

export function label(value?: string | null): string {
  if (!value) return "—";
  return LABEL[value] ?? value;
}

export function nodeStateText(state: string): string {
  if (state === "open") return "待开始";
  if (state === "running") return "执行中";
  if (state === "done") return "已完成";
  return label(state);
}

export function stepStatusText(status: string): string {
  if (status === "pending") return "待派发";
  if (status === "running") return "执行中";
  return label(status);
}

export function missionStatusTone(status: string): string {
  switch (status) {
    case "running": return "info";
    case "paused": return "warning";
    case "finalizing": return "accent";
    case "done": case "passed": case "executed": case "allow": case "result": return "success";
    case "failed": case "error": case "critical": case "high": case "deny": case "tool_error": case "denied_by_rule":
    case "budget_exhausted": case "step_budget_exhausted": case "planner_error": return "danger";
    case "approval_required": case "ask": case "pending": case "medium": case "confirmed": return "warning";
    default: return "neutral";
  }
}

export function missionActions(status: string) {
  return {
    pause: status === "running",
    resume: status === "paused",
    cancel: status === "queued" || status === "running" || status === "paused" || status === "finalizing",
  };
}

export function allowedNodeStates(kind: string): string[] {
  if (kind === "goal") return ["open", "done", "blocked"];
  if (kind === "fact" || kind === "finding" || kind === "hint" || kind === "digest") return ["open", "done", "exhausted", "blocked"];
  return [...NODE_STATES];
}

export const STREAM_EVENT_TYPES = [
  "created", "admitted", "started", "paused", "resumed", "finalizing", "completed", "failed", "cancelled", "deleted", "recovered",
  "approval_required", "budget_exhausted", "step_budget_exhausted", "planner_error", "planner_persist_error", "planner_done_with_pending",
] as const;

export const REFRESH_EVENT_TYPES = new Set<string>([
  "created", "admitted", "started", "paused", "resumed", "finalizing", "completed", "failed", "cancelled", "recovered",
  "approval_required", "budget_exhausted", "step_budget_exhausted",
]);

export function formatTime(value?: string | null): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

export function formatClock(value?: string | null): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

export function formatNumber(value?: number | null): string {
  if (value === null || value === undefined || Number.isNaN(value)) return "0";
  return value.toLocaleString("zh-CN");
}

export function formatBytes(value?: number | null): string {
  const bytes = value ?? 0;
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function shortHash(hash?: string | null): string {
  if (!hash) return "—";
  return hash.length > 14 ? `${hash.slice(0, 12)}…` : hash;
}

export function budgetPercent(used: number, max: number): number | null {
  if (!Number.isFinite(max) || max <= 0) return null;
  return Math.max(0, Math.min(100, (used / max) * 100));
}

export function budgetText(used: number, max: number): string {
  return `${formatNumber(used)} / ${max > 0 ? formatNumber(max) : "不限"}`;
}

export function missionBudgetText(mission: Mission): string {
  return `token ${budgetText(mission.spent_tokens, mission.max_tokens)} · 步 ${budgetText(mission.steps_used, mission.max_steps)}`;
}

export function parseObject(text: string): Record<string, unknown> {
  const trimmed = text.trim();
  if (!trimmed) return {};
  const value: unknown = JSON.parse(trimmed);
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("需要一个 JSON 对象");
  return value as Record<string, unknown>;
}

export function pretty(value: unknown): string {
  if (value === undefined || value === null || value === "") return "";
  if (typeof value === "string") return value;
  try { return JSON.stringify(value, null, 2); } catch { return String(value); }
}

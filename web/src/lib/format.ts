// 展示层格式化与中文文案映射。
//
// 所有映射只做"已知取值 → 中文标签"的翻译，未知取值原样展示，
// 不猜测后端语义，也不产出任何虚构数据。

import type { Mission } from "@/lib/types";

/** 任务状态标签（与 migrations/0003_runtime.sql 的 CHECK 约束一致）。 */
const MISSION_STATUS_LABELS: Record<string, string> = {
  queued: "排队中",
  running: "运行中",
  paused: "已暂停",
  finalizing: "收尾中",
  done: "已完成",
  failed: "已失败",
  cancelled: "已取消",
};

export function missionStatusLabel(status: string): string {
  return MISSION_STATUS_LABELS[status] ?? status ?? "未知";
}

/** 任务状态对应的徽标色调（见 styles.css 的 .badge-* ）。 */
export function missionStatusTone(status: string): string {
  switch (status) {
    case "running":
      return "info";
    case "paused":
      return "warning";
    case "finalizing":
      return "accent";
    case "done":
      return "success";
    case "failed":
      return "danger";
    case "cancelled":
      return "neutral";
    default:
      return "neutral";
  }
}

/** 审批状态标签（pending → approved / rejected / timeout）。 */
const APPROVAL_STATUS_LABELS: Record<string, string> = {
  pending: "待审批",
  approved: "已批准",
  rejected: "已拒绝",
  timeout: "已超时",
};

export function approvalStatusLabel(status: string): string {
  return APPROVAL_STATUS_LABELS[status] ?? status ?? "未知";
}

export function approvalStatusTone(status: string): string {
  switch (status) {
    case "pending":
      return "warning";
    case "approved":
      return "success";
    case "rejected":
      return "danger";
    case "timeout":
      return "neutral";
    default:
      return "neutral";
  }
}

/** 采集阶段：intent（已采集请求）/ result（执行完成）。 */
const TRAFFIC_PHASE_LABELS: Record<string, string> = {
  intent: "请求已采集",
  result: "已完成",
};

export function trafficPhaseLabel(phase: string): string {
  return TRAFFIC_PHASE_LABELS[phase] ?? phase ?? "未知";
}

export function trafficPhaseTone(phase: string): string {
  switch (phase) {
    case "result":
      return "success";
    case "intent":
      return "warning";
    default:
      return "neutral";
  }
}

/**
 * 任务事件类型 → 中文标签。
 *
 * 前 11 个来自 internal/domain/task 的状态机事件；approval_required 由执行器
 * 直接写入（internal/app/executor/executor.go）；预算类事件来自 M3。
 * 未知类型原样展示，避免把不认识的语义翻译错。
 */
const EVENT_TYPE_LABELS: Record<string, string> = {
  created: "创建",
  admitted: "准入",
  started: "启动",
  paused: "暂停",
  resumed: "恢复",
  finalizing: "进入收尾",
  completed: "完成",
  failed: "失败",
  cancelled: "取消",
  deleted: "删除",
  recovered: "恢复对账",
  approval_required: "需要审批",
  budget_exhausted: "Token 预算耗尽",
  step_budget_exhausted: "步骤预算耗尽",
};

/** 需要在事件流上显式注册监听的类型（EventSource 只派发已注册的命名事件）。 */
export const STREAM_EVENT_TYPES = [
  "created",
  "admitted",
  "started",
  "paused",
  "resumed",
  "finalizing",
  "completed",
  "failed",
  "cancelled",
  "deleted",
  "recovered",
  "approval_required",
  "budget_exhausted",
  "step_budget_exhausted",
] as const;

/** 收到这些事件时刷新任务详情与流量列表。 */
export const REFRESH_EVENT_TYPES = new Set<string>([
  "created",
  "admitted",
  "started",
  "paused",
  "resumed",
  "finalizing",
  "completed",
  "failed",
  "cancelled",
  "recovered",
]);

export function eventTypeLabel(type: string): string {
  return EVENT_TYPE_LABELS[type] ?? type ?? "未知事件";
}

export function eventTypeTone(type: string): string {
  switch (type) {
    case "completed":
      return "success";
    case "failed":
      return "danger";
    case "cancelled":
    case "deleted":
      return "neutral";
    case "paused":
    case "approval_required":
      return "warning";
    case "budget_exhausted":
    case "step_budget_exhausted":
      return "danger";
    default:
      return "info";
  }
}

/** "2026-10-21T13:04:59Z" → "2026-10-21 21:04:59"（本地时区）；非法值原样返回。 */
export function formatTime(value?: string | null): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  );
}

/** 只显示时:分:秒（事件流用）。 */
export function formatClock(value?: string | null): string {
  if (!value) return formatTime(new Date().toISOString());
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

/** 长哈希只展示前 12 位，避免撑爆表格。 */
export function shortHash(hash?: string | null): string {
  if (!hash) return "—";
  return hash.length > 14 ? `${hash.slice(0, 12)}…` : hash;
}

/** 预算百分比；max<=0 表示不限，返回 null。 */
export function budgetPercent(used: number, max: number): number | null {
  if (!Number.isFinite(max) || max <= 0) return null;
  return Math.max(0, Math.min(100, (used / max) * 100));
}

/** 任务预算的展示文案，例如 "1.2k / 50k token"、"3 / 不限 步"。 */
export function missionBudgetText(mission: Mission): string {
  const tokens = `${formatNumber(mission.spent_tokens)} / ${mission.max_tokens > 0 ? formatNumber(mission.max_tokens) : "不限"}`;
  const steps = `${formatNumber(mission.steps_used)} / ${mission.max_steps > 0 ? formatNumber(mission.max_steps) : "不限"}`;
  return `token ${tokens} · 步 ${steps}`;
}

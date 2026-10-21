"use client";

// 系统面板：健康探针、就绪检查、构建信息与自更新入口。
//
// 事实边界：
//   - /healthz、/readyz、/api/v1/system/info 是公开端点（见 internal/httpapi/handler.go）。
//   - 更新接口（/api/v1/system/update/check|apply）后端尚未实现；这里按 404/405/501
//     容错显示“暂不可用”，不伪造版本号或更新结果。

import { useCallback, useEffect, useState } from "react";

import {
  ApiError,
  applyUpdate,
  checkUpdate,
  describeError,
  getHealth,
  getReadiness,
  getSystemInfo,
  isAuthError,
} from "@/lib/api";
import { formatTime } from "@/lib/format";
import { useInterval } from "@/lib/hooks";
import type { HealthStatus, Readiness, ReadinessCheck, SystemInfo } from "@/lib/types";

import { Badge, EmptyState, ErrorBanner, JsonBlock, NoticeBanner, SectionCard } from "@/components/ui";

const POLL_INTERVAL_MS = 10000;

interface SystemPanelProps {
  token: string;
  onAuthError: (message: string) => void;
}

type UpdateStatus = "idle" | "checking" | "applying" | "ok" | "unavailable" | "error";

interface UpdateState {
  status: UpdateStatus;
  message: string;
  payload?: unknown;
}

function asReadiness(value: unknown): Readiness | null {
  if (!value || typeof value !== "object") return null;
  const record = value as Record<string, unknown>;
  if (typeof record.ready !== "boolean" || !Array.isArray(record.checks)) return null;
  return { ready: record.ready, checks: record.checks as ReadinessCheck[] };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** 更新接口的响应是后端定义的 JSON；这里只做"已知字段"的摘要，未知形状回退为原始 JSON。 */
function summarizeUpdatePayload(payload: unknown, action: "check" | "apply"): string {
  if (!isRecord(payload)) {
    return action === "check" ? "检查完成（响应见下方）。" : "更新请求已提交（响应见下方）。";
  }
  if (action === "apply") {
    const version = typeof payload.version === "string" ? payload.version : "";
    const message = typeof payload.message === "string" ? payload.message : "";
    if (typeof payload.updated === "boolean") {
      return payload.updated
        ? `更新完成${version ? `（${version}）` : ""}${message ? `：${message}` : ""}`
        : `无需更新${version ? `（当前 ${version}）` : ""}${message ? `：${message}` : ""}`;
    }
  } else {
    const release = isRecord(payload.release) ? payload.release : null;
    const version = release && typeof release.version === "string" ? release.version : "";
    if (typeof payload.has_update === "boolean") {
      return payload.has_update
        ? `发现新版本${version ? ` ${version}` : ""}；下载地址与校验和见下方响应。`
        : "当前已是最新版本。";
    }
  }
  return action === "check" ? "检查完成（响应见下方）。" : "更新请求已提交（响应见下方）。";
}

export function SystemPanel({ token, onAuthError }: SystemPanelProps) {
  const [health, setHealth] = useState<HealthStatus | null>(null);
  const [healthError, setHealthError] = useState<string | null>(null);
  const [readiness, setReadiness] = useState<Readiness | null>(null);
  const [readinessError, setReadinessError] = useState<string | null>(null);
  const [info, setInfo] = useState<SystemInfo | null>(null);
  const [infoError, setInfoError] = useState<string | null>(null);
  const [updateState, setUpdateState] = useState<UpdateState>({ status: "idle", message: "尚未检查更新。" });

  const loadPublic = useCallback(async () => {
    try {
      setHealth(await getHealth());
      setHealthError(null);
    } catch (error) {
      setHealth(null);
      setHealthError(describeError(error));
    }

    try {
      setReadiness(await getReadiness());
      setReadinessError(null);
    } catch (error) {
      // 未就绪时后端返回 503，但响应体仍是 Readiness 结构；能解析就用它展示细节。
      const parsed = error instanceof ApiError ? asReadiness(error.payload) : null;
      if (parsed) {
        setReadiness(parsed);
        setReadinessError("就绪检查未通过（HTTP 503），详见下方检查项。");
      } else {
        setReadiness(null);
        setReadinessError(describeError(error));
      }
    }

    try {
      setInfo(await getSystemInfo());
      setInfoError(null);
    } catch (error) {
      setInfo(null);
      setInfoError(describeError(error));
    }
  }, []);

  useEffect(() => {
    void loadPublic();
  }, [loadPublic]);

  useInterval(() => void loadPublic(), POLL_INTERVAL_MS);

  const handleUpdateError = useCallback(
    (error: unknown, action: string) => {
      if (isAuthError(error)) {
        onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
        setUpdateState({ status: "error", message: `${action}需要有效的管理令牌。` });
        return;
      }
      if (error instanceof ApiError && [404, 405, 501, 502, 503].includes(error.status)) {
        setUpdateState({
          status: "unavailable",
          message: `更新接口不可用：${error.message}${error.code ? `（${error.code}）` : ""}`,
        });
        return;
      }
      setUpdateState({ status: "error", message: `${action}失败：${describeError(error)}` });
    },
    [onAuthError],
  );

  const check = useCallback(async () => {
    if (!token) {
      setUpdateState({ status: "error", message: "请先在页面顶部保存管理令牌。" });
      return;
    }
    setUpdateState({ status: "checking", message: "正在检查更新…" });
    try {
      const payload = await checkUpdate(token);
      setUpdateState({ status: "ok", message: summarizeUpdatePayload(payload, "check"), payload });
    } catch (error) {
      handleUpdateError(error, "检查更新");
    }
  }, [token, handleUpdateError]);

  // 保存令牌后自动探测一次，让"接口是否存在"一眼可见；失败按容错文案展示。
  useEffect(() => {
    if (token) void check();
  }, [token, check]);

  async function apply() {
    if (!token) {
      setUpdateState({ status: "error", message: "请先在页面顶部保存管理令牌。" });
      return;
    }
    if (!window.confirm("确定触发后端自更新？升级会重启服务，当前页面可能短暂不可用。")) return;
    setUpdateState({ status: "applying", message: "正在提交更新请求…" });
    try {
      const payload = await applyUpdate(token);
      setUpdateState({ status: "ok", message: summarizeUpdatePayload(payload, "apply"), payload });
    } catch (error) {
      handleUpdateError(error, "触发更新");
    }
  }

  return (
    <div className="panel-stack">
      <SectionCard
        headingId="health-heading"
        title="健康与就绪"
        description={`公开探针，每 ${POLL_INTERVAL_MS / 1000} 秒自动刷新，无需令牌。`}
        actions={
          <button type="button" className="btn btn-sm" onClick={() => void loadPublic()}>
            刷新
          </button>
        }
      >
        <div className="kv-grid">
          <div className="kv">
            <span className="kv-key">GET /healthz</span>
            <span className="kv-value">
              {health ? (
                <Badge tone={health.status === "ok" ? "success" : "warning"}>
                  {health.status === "ok" ? "正常" : "异常"}
                  <span className="mono small muted">（{health.status}）</span>
                </Badge>
              ) : (
                <span className="muted">未获取</span>
              )}
            </span>
          </div>
          <div className="kv">
            <span className="kv-key">GET /readyz</span>
            <span className="kv-value">
              {readiness ? (
                <Badge tone={readiness.ready ? "success" : "warning"}>
                  {readiness.ready ? "就绪" : "未就绪"}
                </Badge>
              ) : (
                <span className="muted">未获取</span>
              )}
            </span>
          </div>
        </div>

        {healthError ? <ErrorBanner message={`健康检查失败：${healthError}`} /> : null}
        {readinessError ? <ErrorBanner message={readinessError} /> : null}

        {readiness && readiness.checks.length > 0 ? (
          <div className="checks">
            {readiness.checks.map((check) => (
              <div className="check-row" key={check.name}>
                <Badge tone={check.ok ? "success" : "danger"}>{check.ok ? "通过" : "失败"}</Badge>
                <span className="check-name">{check.name}</span>
                {check.error ? <span className="check-error">{check.error}</span> : null}
              </div>
            ))}
          </div>
        ) : null}
      </SectionCard>

      <SectionCard
        headingId="info-heading"
        title="系统信息"
        description="GET /api/v1/system/info：构建元数据、schema 版本与运行时长。"
      >
        {infoError ? <ErrorBanner message={`系统信息获取失败：${infoError}`} /> : null}
        {info ? (
          <>
            <div className="kv-grid">
              <div className="kv">
                <span className="kv-key">服务</span>
                <span className="kv-value mono">{info.service}</span>
              </div>
              <div className="kv">
                <span className="kv-key">版本</span>
                <span className="kv-value mono">{info.version}</span>
              </div>
              <div className="kv">
                <span className="kv-key">Commit</span>
                <span className="kv-value mono">{info.commit}</span>
              </div>
              <div className="kv">
                <span className="kv-key">构建时间</span>
                <span className="kv-value">{info.build_time}</span>
              </div>
              <div className="kv">
                <span className="kv-key">启动时间</span>
                <span className="kv-value">{formatTime(info.started_at)}</span>
              </div>
              <div className="kv">
                <span className="kv-key">运行时长</span>
                <span className="kv-value">{info.uptime}</span>
              </div>
              <div className="kv">
                <span className="kv-key">数据库 schema</span>
                <span className="kv-value mono">
                  {info.schema_version} / {info.latest_migration}
                </span>
              </div>
            </div>
            <JsonBlock value={info} label="原始响应" />
          </>
        ) : !infoError ? (
          <p className="loading-text">正在获取系统信息…</p>
        ) : null}
      </SectionCard>

      <SectionCard
        headingId="update-heading"
        title="自更新"
        description="GET /api/v1/system/update/check、POST /api/v1/system/update/apply；后端未配置自更新时按不可用如实展示。"
        actions={
          <div className="update-actions">
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => void check()}
              disabled={updateState.status === "checking" || updateState.status === "applying"}
            >
              {updateState.status === "checking" ? "检查中…" : "检查更新"}
            </button>
            <button
              type="button"
              className="btn btn-danger btn-sm"
              onClick={() => void apply()}
              disabled={updateState.status === "checking" || updateState.status === "applying"}
            >
              {updateState.status === "applying" ? "提交中…" : "立即更新"}
            </button>
          </div>
        }
      >
        {updateState.status === "unavailable" ? (
          <NoticeBanner message={updateState.message} />
        ) : updateState.status === "error" ? (
          <ErrorBanner message={updateState.message} />
        ) : (
          <NoticeBanner message={updateState.message} />
        )}
        {updateState.payload !== undefined ? <JsonBlock value={updateState.payload} label="接口响应" /> : null}
        <p className="hint">
          更新接口由后端提供：未配置自更新时返回 503 update_unavailable，前端按不可用展示；
          成功时展示后端返回的 JSON（has_update / release / updated / version 等），不伪造版本结论。
        </p>
      </SectionCard>

      {!token ? (
        <EmptyState
          title="更新入口需要令牌"
          hint="健康检查与系统信息无需令牌；检查更新 / 触发更新需要管理令牌。"
        />
      ) : null}
    </div>
  );
}

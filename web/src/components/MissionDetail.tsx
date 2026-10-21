"use client";

// 任务详情：任务信息 + SSE 事件流 + 流量采集与证据冻结/校验。
//
// 事件流直接消费后端 SSE（internal/httpapi/sse.go）：
//   - 帧格式含 id / event / data；data 有时是事件负载，有时是完整事件对象，
//     解析时两者都要兼容（见 normalizeStreamEvent）。
//   - 断线由浏览器 EventSource 自动重连，并带上 Last-Event-ID；后端据此补洞。
//   - 页面只保留最近 200 条用于展示，避免长时间运行把 DOM 拖垮。

import { useCallback, useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";

import {
  ApiError,
  describeError,
  freezeEvidence,
  getMission,
  isAuthError,
  listTraffic,
  sseUrl,
  verifyEvidence,
} from "@/lib/api";
import {
  budgetPercent,
  eventTypeLabel,
  eventTypeTone,
  formatBytes,
  formatClock,
  formatNumber,
  formatTime,
  missionStatusLabel,
  missionStatusTone,
  REFRESH_EVENT_TYPES,
  shortHash,
  STREAM_EVENT_TYPES,
  trafficPhaseLabel,
  trafficPhaseTone,
} from "@/lib/format";
import { useInterval, useMountedRef } from "@/lib/hooks";
import type { Mission, StreamEvent, TrafficExchange } from "@/lib/types";

import { Badge, EmptyState, ErrorBanner, JsonBlock, NoticeBanner, ProgressBar, SectionCard } from "@/components/ui";

const POLL_INTERVAL_MS = 5000;
const MAX_EVENTS = 200;

interface EvidenceState {
  ok: boolean;
  text: string;
}

function parseJson(text: string): unknown {
  if (!text) return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * 把一帧 SSE 归一成展示结构。
 *
 * 后端在事件有 payload 时只下发 payload；没有 payload 时下发完整事件对象。
 * 这里用 "是否有 mission_id + payload 字段" 区分，两种形状都能正确展示，
 * 并且以 SSE 的 id 作为去重键（重连回放会重复下发同一事件）。
 */
function normalizeStreamEvent(
  fallbackType: string,
  message: MessageEvent<string>,
  seen: Set<string>,
): StreamEvent | null {
  const raw = parseJson(message.data);
  let type = fallbackType;
  let detail: unknown = raw;
  let createdAt: string | undefined;
  let id: number | undefined;

  if (message.lastEventId) {
    const parsedId = Number(message.lastEventId);
    if (Number.isSafeInteger(parsedId) && parsedId > 0) id = parsedId;
  }

  if (isRecord(raw)) {
    if (typeof raw.type === "string" && raw.type) type = raw.type;
    if (typeof raw.created_at === "string") createdAt = raw.created_at;
    if (id === undefined && typeof raw.id === "number") id = raw.id;
    if (typeof raw.mission_id === "number") {
      // 完整事件对象（created 事件的 payload 为空时后端会发整个对象）：
      // 展示时只关心业务负载，缺省为 null，避免把信封字段当成事件内容。
      detail = "payload" in raw ? (raw.payload ?? null) : null;
    }
  }

  const key = id !== undefined ? `id-${id}` : `${type}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  if (id !== undefined) {
    if (seen.has(key)) return null;
    seen.add(key);
  }

  return { key, id, type, createdAt, receivedAt: new Date().toISOString(), detail };
}

interface MissionDetailProps {
  token: string;
  missionId: number | null;
  onAuthError: (message: string) => void;
}

export function MissionDetail({ token, missionId, onAuthError }: MissionDetailProps) {
  const [mission, setMission] = useState<Mission | null>(null);
  const [missionError, setMissionError] = useState<string | null>(null);
  const [events, setEvents] = useState<StreamEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const [reconnectKey, setReconnectKey] = useState(0);

  const [traffic, setTraffic] = useState<TrafficExchange[]>([]);
  const [trafficLoading, setTrafficLoading] = useState(false);
  const [trafficError, setTrafficError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [evidenceByTraffic, setEvidenceByTraffic] = useState<Record<number, number>>({});
  const [evidenceState, setEvidenceState] = useState<Record<string, EvidenceState>>({});
  const [manualEvidenceId, setManualEvidenceId] = useState("");
  const [manualResult, setManualResult] = useState<EvidenceState | null>(null);

  const mountedRef = useMountedRef();
  const seenEventIds = useRef<Set<string>>(new Set());

  const loadMission = useCallback(async () => {
    if (!missionId || !token) return;
    try {
      const data = await getMission(token, missionId);
      if (!mountedRef.current) return;
      setMission(data);
      setMissionError(null);
    } catch (error) {
      if (!mountedRef.current) return;
      if (isAuthError(error)) {
        onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
        return;
      }
      if (error instanceof ApiError && error.status === 404) {
        setMission(null);
        setMissionError("任务不存在或已被删除。");
        return;
      }
      setMissionError(describeError(error));
    }
  }, [missionId, token, onAuthError, mountedRef]);

  const loadTraffic = useCallback(async () => {
    if (!missionId || !token) return;
    setTrafficLoading(true);
    try {
      const data = await listTraffic(token, missionId);
      if (!mountedRef.current) return;
      setTraffic(data.traffic ?? []);
      setTrafficError(null);
    } catch (error) {
      if (!mountedRef.current) return;
      if (isAuthError(error)) onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
      setTrafficError(describeError(error));
    } finally {
      if (mountedRef.current) setTrafficLoading(false);
    }
  }, [missionId, token, onAuthError, mountedRef]);

  // 切换任务时清空全部本地状态，避免上一个任务的事件/证据串到新任务。
  useEffect(() => {
    setMission(null);
    setMissionError(null);
    setEvents([]);
    setConnected(false);
    setTraffic([]);
    setTrafficError(null);
    setEvidenceByTraffic({});
    setEvidenceState({});
    setManualEvidenceId("");
    setManualResult(null);
    seenEventIds.current = new Set();
  }, [missionId]);

  useEffect(() => {
    if (!missionId || !token) return;
    void loadMission();
    void loadTraffic();
  }, [missionId, token, loadMission, loadTraffic]);

  useInterval(() => {
    void loadMission();
    void loadTraffic();
  }, missionId && token ? POLL_INTERVAL_MS : null);

  // 单条任务级 SSE。EventSource 自带断线重连并回传 Last-Event-ID。
  useEffect(() => {
    if (!missionId || !token) return;
    let disposed = false;
    const seen = seenEventIds.current;
    const source = new EventSource(sseUrl(`/api/v1/missions/${missionId}/events?since=0`, token));

    source.onopen = () => {
      if (!disposed) setConnected(true);
    };
    source.onerror = () => {
      if (!disposed) setConnected(false);
    };

    const ingest = (message: MessageEvent<string>, fallbackType: string) => {
      if (disposed) return;
      const event = normalizeStreamEvent(fallbackType, message, seen);
      if (!event) return;
      setEvents((current) => [event, ...current].slice(0, MAX_EVENTS));
      if (REFRESH_EVENT_TYPES.has(event.type)) {
        void loadMission();
        void loadTraffic();
      }
    };

    const bound: Array<[string, EventListener]> = [];
    for (const type of STREAM_EVENT_TYPES) {
      const listener: EventListener = (rawEvent) => ingest(rawEvent as MessageEvent<string>, type);
      source.addEventListener(type, listener);
      bound.push([type, listener]);
    }
    // 兼容未命名事件（后端 Type 为空时以 message 帧下发）。
    const messageListener: EventListener = (rawEvent) => ingest(rawEvent as MessageEvent<string>, "message");
    source.addEventListener("message", messageListener);
    bound.push(["message", messageListener]);

    return () => {
      disposed = true;
      for (const [type, listener] of bound) source.removeEventListener(type, listener);
      source.close();
    };
  }, [missionId, token, reconnectKey, loadMission, loadTraffic]);

  const freeze = useCallback(
    async (item: TrafficExchange) => {
      setBusy(`freeze:${item.id}`);
      try {
        const snapshot = await freezeEvidence(token, item.id);
        setEvidenceByTraffic((current) => ({ ...current, [item.id]: snapshot.id }));
        setEvidenceState((current) => ({
          ...current,
          [`traffic-${item.id}`]: {
            ok: true,
            text: `已冻结为证据 #${snapshot.id}（签名 ${shortHash(snapshot.signature)}）。`,
          },
        }));
        await loadTraffic();
      } catch (error) {
        if (isAuthError(error)) onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
        setEvidenceState((current) => ({
          ...current,
          [`traffic-${item.id}`]: { ok: false, text: `冻结失败：${describeError(error)}` },
        }));
      } finally {
        setBusy(null);
      }
    },
    [token, loadTraffic, onAuthError],
  );

  const verify = useCallback(
    async (evidenceId: number) => {
      setBusy(`verify:${evidenceId}`);
      try {
        const result = await verifyEvidence(token, evidenceId);
        setEvidenceState((current) => ({
          ...current,
          [`evidence-${evidenceId}`]: {
            ok: result.verified,
            text: result.verified
              ? `证据 #${evidenceId} 校验通过：HMAC 签名与正文哈希一致。`
              : `证据 #${evidenceId} 校验未通过。`,
          },
        }));
      } catch (error) {
        if (isAuthError(error)) onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
        const tampered = error instanceof ApiError && error.status === 409;
        setEvidenceState((current) => ({
          ...current,
          [`evidence-${evidenceId}`]: {
            ok: false,
            text: tampered
              ? `证据 #${evidenceId} 校验失败（409 tampered）：签名或正文哈希不一致，疑似被篡改。`
              : `证据 #${evidenceId} 校验失败：${describeError(error)}`,
          },
        }));
      } finally {
        setBusy(null);
      }
    },
    [token, onAuthError],
  );

  async function handleManualVerify(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const text = manualEvidenceId.trim();
    if (!/^\d+$/.test(text) || Number(text) <= 0) {
      setManualResult({ ok: false, text: "请输入有效的证据 ID（正整数）。" });
      return;
    }
    const evidenceId = Number(text);
    setBusy(`verify:${evidenceId}`);
    setManualResult(null);
    try {
      const result = await verifyEvidence(token, evidenceId);
      setManualResult({
        ok: result.verified,
        text: result.verified
          ? `证据 #${evidenceId} 校验通过：HMAC 签名与正文哈希一致。`
          : `证据 #${evidenceId} 校验未通过。`,
      });
    } catch (error) {
      if (isAuthError(error)) onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
      const tampered = error instanceof ApiError && error.status === 409;
      setManualResult({
        ok: false,
        text: tampered
          ? `证据 #${evidenceId} 校验失败（409 tampered）：签名或正文哈希不一致，疑似被篡改。`
          : `证据 #${evidenceId} 校验失败：${describeError(error)}`,
      });
    } finally {
      setBusy(null);
    }
  }

  if (!missionId) {
    return (
      <SectionCard headingId="detail-heading" title="任务详情">
        <EmptyState title="未选择任务" hint="在左侧任务列表中点击任意任务，查看详情、事件流与流量证据。" />
      </SectionCard>
    );
  }

  if (!token) {
    return (
      <SectionCard headingId="detail-heading" title="任务详情">
        <EmptyState title="尚未保存管理令牌" hint="在页面顶部保存令牌后，才能读取任务详情与事件流。" />
      </SectionCard>
    );
  }

  const tokenPercent =
    mission && mission.max_tokens > 0 ? budgetPercent(mission.spent_tokens, mission.max_tokens) : null;
  const stepPercent = mission && mission.max_steps > 0 ? budgetPercent(mission.steps_used, mission.max_steps) : null;

  return (
    <div className="panel-stack">
      <SectionCard
        headingId="detail-heading"
        title={
          <>
            任务详情
            {mission ? <span className="mono small muted">#{mission.id}</span> : null}
          </>
        }
        description={mission?.name ?? "正在加载任务信息…"}
        actions={
          <button type="button" className="btn btn-sm" onClick={() => void loadMission()}>
            刷新
          </button>
        }
      >
        {missionError ? (
          <ErrorBanner message={missionError} onDismiss={() => setMissionError(null)} />
        ) : null}

        {mission ? (
          <>
            <div className="detail-head">
              <div className="detail-title-row">
                <span className="detail-title">{mission.name}</span>
                <Badge tone={missionStatusTone(mission.status)}>{missionStatusLabel(mission.status)}</Badge>
                {mission.queued ? <Badge tone="warning">已入队</Badge> : null}
              </div>
              <p className="muted">{mission.goal || "（未填写目标说明）"}</p>
              {mission.description ? <p className="small muted">{mission.description}</p> : null}
            </div>

            <div className="kv-grid">
              <div className="kv">
                <span className="kv-key">任务 ID</span>
                <span className="kv-value mono">#{mission.id}</span>
              </div>
              <div className="kv">
                <span className="kv-key">创建时间</span>
                <span className="kv-value">{formatTime(mission.created_at)}</span>
              </div>
              <div className="kv">
                <span className="kv-key">开始时间</span>
                <span className="kv-value">{formatTime(mission.started_at)}</span>
              </div>
              <div className="kv">
                <span className="kv-key">结束时间</span>
                <span className="kv-value">{formatTime(mission.finished_at)}</span>
              </div>
              <div className="kv">
                <span className="kv-key">Token 用量</span>
                <span className="kv-value">
                  {formatNumber(mission.spent_tokens)} / {mission.max_tokens > 0 ? formatNumber(mission.max_tokens) : "不限"}
                </span>
              </div>
              <div className="kv">
                <span className="kv-key">步骤用量</span>
                <span className="kv-value">
                  {formatNumber(mission.steps_used)} / {mission.max_steps > 0 ? formatNumber(mission.max_steps) : "不限"}
                </span>
              </div>
            </div>

            {tokenPercent !== null ? (
              <div className="budget-row">
                <span className="small muted">Token 预算使用 {tokenPercent.toFixed(1)}%</span>
                <ProgressBar percent={tokenPercent} />
              </div>
            ) : null}
            {stepPercent !== null ? (
              <div className="budget-row">
                <span className="small muted">步骤预算使用 {stepPercent.toFixed(1)}%</span>
                <ProgressBar percent={stepPercent} />
              </div>
            ) : null}

            {mission.reason ? <p className="error-text">结束原因：{mission.reason}</p> : null}
            <p className="hint">计划步骤 / 节点：后端当前未提供公开查询接口，本页面不展示该部分，避免用占位数据冒充真实进度。</p>
          </>
        ) : !missionError ? (
          <p className="loading-text">正在加载任务详情…</p>
        ) : null}
      </SectionCard>

      <SectionCard
        headingId="stream-heading"
        title="事件流"
        description="创建 / 准入 / 收尾 / 完成等任务事件，来自 SSE 实时推送（断线自动重连并补洞）。"
        actions={
          <>
            <span className="stream-status">
              <span className={`status-dot ${connected ? "live" : "down"}`} aria-hidden="true" />
              {connected ? "已连接" : "重连中"}
            </span>
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => {
                setConnected(false);
                setReconnectKey((value) => value + 1);
              }}
            >
              重新连接
            </button>
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              onClick={() => setEvents([])}
              disabled={events.length === 0}
            >
              清空显示
            </button>
          </>
        }
      >
        <p className="hint">
          已接收 {events.length} 条（页面最多保留最近 {MAX_EVENTS} 条）。令牌通过查询参数传给 SSE
          （EventSource 无法自定义请求头）。
        </p>
        {!connected ? (
          <NoticeBanner message="事件流未连接：浏览器正在自动重试；若持续失败，请检查令牌或后端是否可达。" />
        ) : null}
        {events.length === 0 ? (
          <EmptyState title="等待事件…" hint="任务状态变化时，事件会实时出现在这里。" />
        ) : (
          <div className="event-list">
            {events.map((event) => {
              const tone = eventTypeTone(event.type);
              return (
                <div key={event.key} className={`event-item tone-${tone}`}>
                  <div className="event-meta">
                    <span className="event-type">{eventTypeLabel(event.type)}</span>
                    <span className="event-time">{formatClock(event.createdAt ?? event.receivedAt)}</span>
                    {event.id !== undefined ? <span className="event-id">#{event.id}</span> : null}
                    {event.createdAt ? null : <span className="event-id">本地接收</span>}
                  </div>
                  {event.detail !== null && event.detail !== undefined ? (
                    <details>
                      <summary>查看负载</summary>
                      <JsonBlock value={event.detail} />
                    </details>
                  ) : null}
                </div>
              );
            })}
          </div>
        )}
      </SectionCard>

      <SectionCard
        headingId="traffic-heading"
        title="流量与证据"
        description="工具层两阶段采集（intent → result）。冻结后生成带 HMAC 签名的证据快照，可随时复核。"
        actions={
          <button type="button" className="btn btn-sm" onClick={() => void loadTraffic()} disabled={trafficLoading}>
            {trafficLoading ? "刷新中…" : "刷新"}
          </button>
        }
      >
        {trafficError ? <ErrorBanner message={trafficError} onDismiss={() => setTrafficError(null)} /> : null}

        {traffic.length === 0 ? (
          <EmptyState
            title="暂无流量记录"
            hint="任务执行工具调用后会写入采集记录；若任务尚未运行或未调用工具，这里是空的。"
          />
        ) : (
          <div className="table-wrap">
            <table className="data-table">
              <thead>
                <tr>
                  <th scope="col">ID</th>
                  <th scope="col">工具</th>
                  <th scope="col">阶段</th>
                  <th scope="col">状态码</th>
                  <th scope="col">大小（请求 → 响应）</th>
                  <th scope="col">采集时间</th>
                  <th scope="col">证据操作</th>
                </tr>
              </thead>
              <tbody>
                {traffic.map((item) => {
                  const evidenceId = evidenceByTraffic[item.id];
                  const freezeState = evidenceState[`traffic-${item.id}`];
                  const verifyState = evidenceId ? evidenceState[`evidence-${evidenceId}`] : undefined;
                  return (
                    <tr key={item.id}>
                      <td className="mono">#{item.id}</td>
                      <td>
                        <div className="mono">{item.tool}</div>
                        {item.error ? <div className="error-text">{item.error}</div> : null}
                      </td>
                      <td>
                        <Badge tone={trafficPhaseTone(item.phase)}>{trafficPhaseLabel(item.phase)}</Badge>
                        {item.redaction ? <div className="small muted">脱敏：{item.redaction}</div> : null}
                      </td>
                      <td className="small">{item.status_code ?? "—"}</td>
                      <td className="small nowrap">
                        {formatBytes(item.request_size)} → {item.phase === "result" ? formatBytes(item.response_size) : "—"}
                      </td>
                      <td className="small nowrap">{formatTime(item.created_at)}</td>
                      <td>
                        <div className="btn-row">
                          {item.phase === "result" ? (
                            <button
                              type="button"
                              className="btn btn-sm"
                              onClick={() => void freeze(item)}
                              disabled={busy !== null}
                            >
                              {busy === `freeze:${item.id}` ? "冻结中…" : evidenceId ? "重新冻结" : "冻结证据"}
                            </button>
                          ) : (
                            <span className="small muted">采集未完成</span>
                          )}
                          {evidenceId ? (
                            <>
                              <span className="small">证据 #{evidenceId}</span>
                              <button
                                type="button"
                                className="btn btn-sm"
                                onClick={() => void verify(evidenceId)}
                                disabled={busy !== null}
                              >
                                {busy === `verify:${evidenceId}` ? "校验中…" : "校验"}
                              </button>
                            </>
                          ) : null}
                        </div>
                        {freezeState ? (
                          <div className={freezeState.ok ? "small" : "error-text"}>{freezeState.text}</div>
                        ) : null}
                        {verifyState ? (
                          <div className={verifyState.ok ? "small" : "error-text"}>{verifyState.text}</div>
                        ) : null}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}

        <hr className="divider" />

        <form className="decision-row" onSubmit={handleManualVerify}>
          <div className="field">
            <label htmlFor="manual-evidence-id">按证据 ID 校验</label>
            <input
              id="manual-evidence-id"
              className="input"
              inputMode="numeric"
              value={manualEvidenceId}
              onChange={(event) => {
                setManualEvidenceId(event.target.value);
                setManualResult(null);
              }}
              placeholder="例如：12"
            />
          </div>
          <button type="submit" className="btn" disabled={busy !== null}>
            {busy?.startsWith("verify:") ? "校验中…" : "校验证据"}
          </button>
        </form>
        {manualResult ? (
          manualResult.ok ? (
            <NoticeBanner message={manualResult.text} />
          ) : (
            <ErrorBanner message={manualResult.text} onDismiss={() => setManualResult(null)} />
          )
        ) : null}

        <p className="hint">
          冻结接口要求采集已完成（phase=result），否则后端返回 409；校验通过返回 200，
          签名或正文哈希不一致时返回 409 tampered。本页面展示的是真实接口结果，没有任何模拟证据。
        </p>
      </SectionCard>
    </div>
  );
}

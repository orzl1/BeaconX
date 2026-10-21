"use client";

// 审批面板：待审批工具调用列表 + 批准/拒绝。
//
// 后端只有 pending 状态可被决策（CAS 更新），已处理或过期会返回 409。
// 审批等待有超时（服务端默认 60 秒，可由策略规则覆盖），超时按拒绝处理。

import { useCallback, useEffect, useState } from "react";

import { decideApproval, describeError, isAuthError, listApprovals } from "@/lib/api";
import { approvalStatusLabel, approvalStatusTone, formatTime } from "@/lib/format";
import { useInterval } from "@/lib/hooks";
import type { Approval } from "@/lib/types";

import { Badge, EmptyState, ErrorBanner, JsonBlock, NoticeBanner, SectionCard } from "@/components/ui";

const POLL_INTERVAL_MS = 5000;

interface ApprovalsPanelProps {
  token: string;
  onAuthError: (message: string) => void;
}

export function ApprovalsPanel({ token, onAuthError }: ApprovalsPanelProps) {
  const [approvals, setApprovals] = useState<Approval[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [notes, setNotes] = useState<Record<number, string>>({});
  const [busyId, setBusyId] = useState<number | null>(null);

  const load = useCallback(async () => {
    if (!token) {
      setApprovals([]);
      setLoadError(null);
      return;
    }
    setLoading(true);
    try {
      const data = await listApprovals(token, "pending");
      setApprovals(data.approvals ?? []);
      setLoadError(null);
    } catch (error) {
      if (isAuthError(error)) onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
      setLoadError(describeError(error));
    } finally {
      setLoading(false);
    }
  }, [token, onAuthError]);

  useEffect(() => {
    void load();
  }, [load]);

  useInterval(() => void load(), token ? POLL_INTERVAL_MS : null);

  async function decide(approval: Approval, approve: boolean) {
    const note = (notes[approval.id] ?? "").trim();
    setBusyId(approval.id);
    setActionError(null);
    setNotice(null);
    try {
      const updated = await decideApproval(token, approval.id, approve, note);
      setNotice(
        `审批 #${updated.id} 已${approve ? "批准" : "拒绝"}（状态：${approvalStatusLabel(updated.status)}）。`,
      );
      setNotes((current) => {
        const next = { ...current };
        delete next[approval.id];
        return next;
      });
      await load();
    } catch (error) {
      if (isAuthError(error)) onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
      setActionError(`审批 #${approval.id} 决策失败：${describeError(error)}`);
    } finally {
      setBusyId(null);
    }
  }

  return (
    <SectionCard
      headingId="approvals-heading"
      title="待审批"
      description={
        token
          ? `等待人工决策的工具调用 · 每 ${POLL_INTERVAL_MS / 1000} 秒自动刷新`
          : "保存管理令牌后加载审批列表。"
      }
      actions={
        <button type="button" className="btn btn-sm" onClick={() => void load()} disabled={!token || loading}>
          {loading ? "刷新中…" : "刷新"}
        </button>
      }
    >
      {!token ? (
        <EmptyState title="尚未保存管理令牌" hint="在页面顶部输入并保存 BEACONX_ADMIN_TOKEN 后，才能读取与决策审批。" />
      ) : null}

      {loadError ? <ErrorBanner message={loadError} onDismiss={() => setLoadError(null)} /> : null}
      {actionError ? <ErrorBanner message={actionError} onDismiss={() => setActionError(null)} /> : null}
      {notice ? <NoticeBanner message={notice} /> : null}

      {token && approvals.length === 0 && !loading && !loadError ? (
        <EmptyState
          title="当前没有待审批项"
          hint="工具调用命中 ask 策略时会进入这里；若任务未运行，列表为空是正常状态。"
        />
      ) : null}

      {token && approvals.length > 0 ? (
        <div className="approval-list">
          {approvals.map((approval) => (
            <article className="approval-item" key={approval.id}>
              <div className="approval-head">
                <div className="approval-title">
                  <span className="mono">审批 #{approval.id}</span>
                  <Badge tone={approvalStatusTone(approval.status)}>{approvalStatusLabel(approval.status)}</Badge>
                  <span className="badge badge-neutral mono">{approval.tool}</span>
                </div>
                <div className="approval-meta">
                  <span>任务 #{approval.mission_id}</span>
                  <span>节点 #{approval.node_id}</span>
                  <span>创建：{formatTime(approval.created_at)}</span>
                  <span>过期：{formatTime(approval.expires_at)}</span>
                </div>
              </div>

              <JsonBlock value={approval.args} label="工具参数" />

              <div className="decision-row">
                <div className="field">
                  <label htmlFor={`approval-note-${approval.id}`}>备注（可选）</label>
                  <input
                    id={`approval-note-${approval.id}`}
                    className="input"
                    value={notes[approval.id] ?? ""}
                    onChange={(event) => setNotes((current) => ({ ...current, [approval.id]: event.target.value }))}
                    placeholder="记录批准/拒绝理由，会写入审批记录"
                    maxLength={500}
                  />
                </div>
                <div className="btn-row">
                  <button
                    type="button"
                    className="btn btn-primary"
                    onClick={() => void decide(approval, true)}
                    disabled={busyId !== null}
                  >
                    {busyId === approval.id ? "处理中…" : "批准"}
                  </button>
                  <button
                    type="button"
                    className="btn btn-danger"
                    onClick={() => void decide(approval, false)}
                    disabled={busyId !== null}
                  >
                    拒绝
                  </button>
                </div>
              </div>
              <p className="approval-hint">
                审批有超时限制（服务端默认 60 秒，可由策略规则覆盖）；超时后按拒绝处理，状态变为“已超时”。
              </p>
            </article>
          ))}
        </div>
      ) : null}
    </SectionCard>
  );
}

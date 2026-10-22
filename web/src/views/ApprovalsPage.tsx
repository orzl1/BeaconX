"use client";

import { useState } from "react";

import { cancelApproval, decideApproval, getApproval, listApprovals } from "@/lib/api";
import { formatTime, label, missionStatusTone } from "@/lib/format";
import { Link, paths } from "@/lib/router";
import { useApi, useBusy } from "@/lib/use-api";
import type { PageProps } from "@/components/Console";
import { Badge, ErrorBanner, JsonBlock, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function ApprovalsPage({ token, onAuthError, missionId }: PageProps & { missionId?: number }) {
  const [status, setStatus] = useState(missionId ? "" : "pending");
  const [note, setNote] = useState("");
  const [detail, setDetail] = useState<unknown>(null);
  const list = useApi({
    token,
    interval: 5000,
    watch: [status],
    onAuthError,
    load: (current, signal) => listApprovals(current, { status: status || undefined, limit: 100 }, signal),
  });
  const busy = useBusy(onAuthError);
  const rows = (list.data?.approvals ?? []).filter((item) => !missionId || item.mission_id === missionId);

  return (
    <>
      {!missionId ? <PageHeader kicker="策略询问" title="审批" description="约 5 秒刷新。已处理或过期时后端返回 409，页面保留原文，不自动重试。" /> : null}
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} onDismiss={() => busy.setError(null)} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="filters">
        <select value={status} onChange={(event) => setStatus(event.target.value)} aria-label="审批状态">
          <option value="">全部</option>
          {["pending", "approved", "rejected", "timeout", "cancelled"].map((item) => <option key={item} value={item}>{label(item)}</option>)}
        </select>
        <input value={note} placeholder="决策备注，可空" onChange={(event) => setNote(event.target.value)} />
      </div>
      <Panel>
        <QueryBody token={token} loading={list.loading} error={list.error} empty={rows.length === 0} emptyTitle="没有审批">
          {rows.map((item) => (
            <article key={item.id} className="event" style={{ gridTemplateColumns: "1fr auto" }}>
              <div>
                <strong>{item.tool}</strong> <Badge tone={missionStatusTone(item.status)}><i />{label(item.status)}</Badge>
                <p className="muted">#{item.id} · <Link href={paths.mission(item.mission_id)}>任务 #{item.mission_id}</Link> · 节点 #{item.node_id} · 到期 {formatTime(item.expires_at)}</p>
                <JsonBlock value={item.args} />
                {item.note ? <p className="muted">备注：{item.note}</p> : null}
              </div>
              <div className="row-actions">
                <button type="button" className="btn btn-ghost btn-sm" disabled={Boolean(busy.busy)} onClick={() => void busy.run(`g${item.id}`, async () => { setDetail(await getApproval(token, item.id)); return ""; })}>详情</button>
              {item.status === "pending" ? (
                <>
                  <button type="button" className="btn btn-primary btn-sm" disabled={Boolean(busy.busy)} onClick={() => void busy.run(`a${item.id}`, async () => { await decideApproval(token, item.id, true, note); list.reload(); return "已批准"; })}>批准</button>
                  <button type="button" className="btn btn-danger btn-sm" disabled={Boolean(busy.busy)} onClick={() => void busy.run(`r${item.id}`, async () => { await decideApproval(token, item.id, false, note); list.reload(); return "已拒绝"; })}>拒绝</button>
                  <button type="button" className="btn btn-ghost btn-sm" disabled={Boolean(busy.busy)} onClick={() => void busy.run(`c${item.id}`, async () => { await cancelApproval(token, item.id); list.reload(); return "已取消"; })}>取消</button>
                </>
              ) : null}
              </div>
            </article>
          ))}
        </QueryBody>
      </Panel>
      {detail ? <Panel title="审批详情"><JsonBlock value={detail} /></Panel> : null}
    </>
  );
}

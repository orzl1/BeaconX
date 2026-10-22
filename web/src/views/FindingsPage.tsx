"use client";

import { useState } from "react";

import { bindFindingEvidence, downloadFile, freezeEvidence, getFinding, listEvidenceAudit, listFindings, listMissionTraffic, transitionFinding, unbindFindingEvidence, updateFinding, verifyEvidence } from "@/lib/api";
import { FINDING_NEXT, FINDING_STATUSES, formatTime, label, missionStatusTone, SEVERITIES, shortHash } from "@/lib/format";
import { Link, paths } from "@/lib/router";
import { useApi, useBusy, useTail } from "@/lib/use-api";
import type { Finding } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { Badge, ErrorBanner, Field, JsonBlock, LoadMore, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function FindingsPage({ token, onAuthError, findingId }: PageProps & { findingId?: number }) {
  if (findingId) return <FindingDetail token={token} onAuthError={onAuthError} id={findingId} />;
  return <FindingList token={token} onAuthError={onAuthError} />;
}

function FindingList({ token, onAuthError }: PageProps) {
  const [severity, setSeverity] = useState("");
  const [status, setStatus] = useState("");
  const [missionId, setMissionId] = useState("");
  const list = useApi({
    token,
    interval: 10000,
    watch: [severity, status, missionId],
    onAuthError,
    load: (current, signal) => listFindings(current, { severity: severity || undefined, status: status || undefined, mission_id: Number(missionId) || undefined, limit: 100 }, signal),
  });
  const tail = useTail<Finding>(`${severity}|${status}|${missionId}`);
  const busy = useBusy(onAuthError);
  const rows = [...(list.data?.findings ?? []), ...tail.rows];
  const nextId = tail.rows.length ? tail.next : (list.data?.next_after_id ?? 0);
  const exportQuery = new URLSearchParams({ format: "csv" });
  if (severity) exportQuery.set("severity", severity);
  if (status) exportQuery.set("status", status);
  if (Number(missionId)) exportQuery.set("mission_id", missionId);
  return (
    <>
      <PageHeader kicker="跨任务" title="发现" description="列表按创建时间倒序。导出是后端生成的 CSV，最多 10000 行，沿用当前筛选。" actions={<button type="button" className="btn btn-ghost" onClick={() => void downloadFile(`/api/v1/findings/export?${exportQuery.toString()}`, token, "findings.csv")}>导出 CSV</button>} />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <div className="filters">
        <input value={missionId} placeholder="任务 ID" onChange={(event) => setMissionId(event.target.value)} />
        <select value={severity} onChange={(event) => setSeverity(event.target.value)}><option value="">全部严重度</option>{SEVERITIES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select>
        <select value={status} onChange={(event) => setStatus(event.target.value)}><option value="">全部状态</option>{FINDING_STATUSES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select>
      </div>
      <Panel>
        <QueryBody token={token} loading={list.loading} error={list.error} empty={rows.length === 0} emptyTitle="没有发现">
          <table>
            <thead><tr><th>标题</th><th>任务</th><th>严重度</th><th>状态</th><th>证据</th></tr></thead>
            <tbody>
              {rows.map((finding) => (
                <tr key={finding.id}>
                  <td><Link href={paths.finding(finding.id)}>{finding.title}</Link></td>
                  <td><Link href={paths.mission(finding.mission_id, "findings")}>#{finding.mission_id}</Link></td>
                  <td><Badge tone={missionStatusTone(finding.severity)}>{label(finding.severity)}</Badge></td>
                  <td>{label(finding.status)}</td>
                  <td className="num">{finding.evidence_count}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <LoadMore nextId={nextId} busy={Boolean(busy.busy)} onLoad={() => void busy.run("more", async () => { const page = await listFindings(token, { severity: severity || undefined, status: status || undefined, mission_id: Number(missionId) || undefined, limit: 100, after_id: nextId }); tail.push(page.findings ?? [], page.next_after_id); })} />
        </QueryBody>
      </Panel>
    </>
  );
}

function FindingDetail({ token, onAuthError, id }: PageProps & { id: number }) {
  const finding = useApi({ token, watch: [id], onAuthError, load: (current, signal) => getFinding(current, id, signal) });
  const busy = useBusy(onAuthError);
  const [audit, setAudit] = useState<unknown>(null);
  const data = finding.data;
  const traffic = useApi({
    token,
    enabled: Boolean(data?.mission_id),
    watch: [data?.mission_id],
    onAuthError,
    load: (current, signal) => listMissionTraffic(current, data?.mission_id ?? 0, signal),
  });
  return (
    <>
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <QueryBody token={token} loading={finding.loading} error={finding.error} empty={!data} emptyTitle="发现不存在">
        {data ? (
          <>
            <PageHeader kicker={`发现 #${data.id} · 任务 #${data.mission_id}`} title={data.title} description={data.detail || "没有补充说明。"} actions={<Badge tone={missionStatusTone(data.severity)}>{label(data.severity)}</Badge>} />
            <p className="muted">状态 {label(data.status)} · 资产 {data.asset_id ?? "—"} · 节点 {data.node_id ?? "—"} · {formatTime(data.updated_at)}</p>
            <div className="row-actions" style={{ margin: "12px 0" }}>
              {(FINDING_NEXT[data.status] ?? []).map((next) => <button key={next} type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(next, async () => { await transitionFinding(token, id, next); finding.reload(); return `已改为${label(next)}`; })}>{label(next)}</button>)}
            </div>
            <form className="form-grid" onSubmit={(event) => { event.preventDefault(); const form = new FormData(event.currentTarget); const assetRaw = String(form.get("asset_id") ?? "").trim(); const nodeRaw = String(form.get("node_id") ?? "").trim(); void busy.run("edit", async () => { await updateFinding(token, id, { title: String(form.get("title") || data.title), severity: String(form.get("severity") || data.severity), detail: String(form.get("detail") ?? ""), asset_id: assetRaw === "" ? undefined : assetRaw === "null" ? null : Number(assetRaw), node_id: nodeRaw === "" ? undefined : nodeRaw === "null" ? null : Number(nodeRaw) }); finding.reload(); return "已更新发现"; }); }}>
              <Field label="标题"><input name="title" defaultValue={data.title} /></Field>
              <Field label="严重度"><select name="severity" defaultValue={data.severity}>{SEVERITIES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select></Field>
              <Field label="说明"><textarea name="detail" defaultValue={data.detail} /></Field>
              <Field label="资产 ID" hint="留空不变，null 清空"><input name="asset_id" placeholder={data.asset_id ? String(data.asset_id) : "null 清空"} /></Field>
              <Field label="节点 ID" hint="留空不变，null 清空"><input name="node_id" placeholder={data.node_id ? String(data.node_id) : "null 清空"} /></Field>
              <button className="btn btn-primary btn-sm" type="submit">保存发现</button>
            </form>
            <Panel title="已绑定证据">
              {(data.evidence ?? []).length === 0 ? <p className="muted">还没有绑定快照。</p> : null}
              {(data.evidence ?? []).map((item) => (
                <div key={item.snapshot_id} className="row-actions" style={{ marginBottom: 8 }}>
                  <span className="num">快照 #{item.snapshot_id}</span>
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`v${item.snapshot_id}`, async () => { const result = await verifyEvidence(token, item.snapshot_id); return result.verified ? `校验通过 ${shortHash(result.signature)}` : "未通过"; })}>校验</button>
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`a${item.snapshot_id}`, async () => { setAudit(await listEvidenceAudit(token, item.snapshot_id)); return `已读取快照 #${item.snapshot_id} 的审计`; })}>审计</button>
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`x${item.snapshot_id}`, () => downloadFile(`/api/v1/evidence/${item.snapshot_id}/export`, token, `evidence-${item.snapshot_id}.zip`))}>导出</button>
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`u${item.snapshot_id}`, async () => { await unbindFindingEvidence(token, id, item.snapshot_id); finding.reload(); })}>解绑</button>
                </div>
              ))}
            </Panel>
            {audit ? <Panel title="证据审计"><JsonBlock value={audit} /></Panel> : null}
            <Panel title="从本任务流量冻结并绑定">
              {(traffic.data?.traffic ?? []).filter((item) => item.phase === "result").map((item) => (
                <div key={item.id} className="row-actions" style={{ marginBottom: 8 }}>
                  <span>{item.tool} · 采集 #{item.id}</span>
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`b${item.id}`, async () => { const snap = await freezeEvidence(token, item.id); await bindFindingEvidence(token, id, snap.id); finding.reload(); return `已绑定快照 #${snap.id}`; })}>冻结并绑定</button>
                </div>
              ))}
              {(traffic.data?.traffic ?? []).filter((item) => item.phase === "result").length === 0 ? <p className="muted">没有已完成的采集可以冻结。</p> : null}
            </Panel>
          </>
        ) : null}
      </QueryBody>
    </>
  );
}

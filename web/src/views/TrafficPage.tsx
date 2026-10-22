"use client";

import { useState } from "react";

import { deleteTraffic, downloadFile, freezeEvidence, listEvidenceAudit, listTraffic, replayTraffic, trafficBody, verifyEvidence } from "@/lib/api";
import { formatBytes, formatTime, label, missionStatusTone, shortHash } from "@/lib/format";
import { Link, paths } from "@/lib/router";
import { useApi, useBusy, useTail } from "@/lib/use-api";
import type { TrafficExchange } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { Badge, ErrorBanner, JsonBlock, LoadMore, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function TrafficPage({ token, onAuthError }: PageProps) {
  const [missionId, setMissionId] = useState("");
  const [tool, setTool] = useState("");
  const [phase, setPhase] = useState("");
  const [body, setBody] = useState("");
  const [audit, setAudit] = useState<unknown>(null);
  const list = useApi({
    token,
    interval: 10000,
    watch: [missionId, tool, phase],
    onAuthError,
    load: (current, signal) => listTraffic(current, { mission_id: Number(missionId) || undefined, tool: tool || undefined, phase: phase || undefined, limit: 100 }, signal),
  });
  const tail = useTail<TrafficExchange>(`${missionId}|${tool}|${phase}`);
  const busy = useBusy(onAuthError);
  const rows = [...(list.data?.traffic ?? []), ...tail.rows];
  const nextId = tail.rows.length ? tail.next : (list.data?.next_after_id ?? 0);
  return (
    <>
      <PageHeader kicker="两阶段采集" title="流量" description="列表只有哈希和大小。点开才读取正文。重放会重新经过策略门，可能产生审批或再次执行。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="filters">
        <input value={missionId} placeholder="任务 ID" onChange={(event) => setMissionId(event.target.value)} />
        <input value={tool} placeholder="工具名" onChange={(event) => setTool(event.target.value)} />
        <select value={phase} onChange={(event) => setPhase(event.target.value)}><option value="">全部阶段</option><option value="intent">请求已采集</option><option value="result">已完成</option></select>
      </div>
      <Panel>
        <QueryBody token={token} loading={list.loading} error={list.error} empty={rows.length === 0} emptyTitle="没有流量">
          <table>
            <thead><tr><th>记录</th><th>阶段</th><th>大小</th><th>时间</th><th /></tr></thead>
            <tbody>
              {rows.map((item) => (
                <tr key={item.id}>
                  <td>{item.tool}<div className="muted"><Link href={paths.mission(item.mission_id, "traffic")}>任务 #{item.mission_id}</Link> · #{item.id} {item.error}</div></td>
                  <td><Badge tone={missionStatusTone(item.phase)}>{label(item.phase)}</Badge></td>
                  <td className="num">{formatBytes(item.request_size)} / {formatBytes(item.response_size)}</td>
                  <td className="num">{formatTime(item.created_at)}</td>
                  <td className="row-actions">
                    <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`q${item.id}`, async () => { setBody(await trafficBody(token, item.id, "request")); })}>请求</button>
                    <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`s${item.id}`, async () => { setBody(await trafficBody(token, item.id, "response")); })}>响应</button>
                    <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`h${item.id}`, () => downloadFile(`/api/v1/traffic/${item.id}/export?format=har`, token, `traffic-${item.id}.har`))}>HAR</button>
                    <button type="button" className="btn btn-ghost btn-sm" disabled={item.phase !== "result"} onClick={() => void busy.run(`f${item.id}`, async () => { const snap = await freezeEvidence(token, item.id); return `证据 #${snap.id} ${shortHash(snap.signature)}`; })}>冻结</button>
                    <button type="button" className="btn btn-ghost btn-sm" onClick={() => { if (!window.confirm("重放会再次经过策略门，可能执行工具或进入审批。继续？")) return; void busy.run(`p${item.id}`, async () => { const result = await replayTraffic(token, item.id); return `策略门 ${result.policy_gated ? "已经过" : "未标记"} · 状态 ${result.status} · 新采集 ${result.traffic_id || "无"}`; }); }}>重放</button>
                    <button type="button" className="btn btn-danger btn-sm" onClick={() => { if (!window.confirm("只删除采集行，已冻结证据仍保留。继续？")) return; void busy.run(`d${item.id}`, async () => { await deleteTraffic(token, item.id); list.reload(); }); }}>删除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <LoadMore nextId={nextId} busy={Boolean(busy.busy)} onLoad={() => void busy.run("more", async () => { const page = await listTraffic(token, { mission_id: Number(missionId) || undefined, tool: tool || undefined, phase: phase || undefined, limit: 100, after_id: nextId }); tail.push(page.traffic ?? [], page.next_after_id); })} />
        </QueryBody>
      </Panel>
      {body ? <Panel title="正文"><pre className="json">{body}</pre></Panel> : null}
      <form className="filters" onSubmit={(event) => { event.preventDefault(); const id = Number(new FormData(event.currentTarget).get("id")); const action = (event.nativeEvent as SubmitEvent).submitter?.getAttribute("value"); void busy.run(action || "verify", async () => { if (action === "audit") { setAudit(await listEvidenceAudit(token, id)); return `已读取证据 #${id} 的审计`; } if (action === "export") { await downloadFile(`/api/v1/evidence/${id}/export`, token, `evidence-${id}.zip`); return "已开始下载"; } const result = await verifyEvidence(token, id); return result.verified ? "校验通过" : "未通过"; }); }}>
        <input name="id" type="number" min={1} placeholder="证据 ID" required />
        <button className="btn btn-ghost btn-sm" type="submit" value="verify">校验证据</button>
        <button className="btn btn-ghost btn-sm" type="submit" value="audit">审计</button>
        <button className="btn btn-ghost btn-sm" type="submit" value="export">导出</button>
      </form>
      {audit ? <Panel title="证据审计"><JsonBlock value={audit} /></Panel> : null}
    </>
  );
}

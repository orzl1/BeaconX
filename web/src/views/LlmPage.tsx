"use client";

import { useState } from "react";

import { getLLMRecord, listLLMRecords, llmBody, llmSummary } from "@/lib/api";
import { formatBytes, formatNumber, formatTime } from "@/lib/format";
import { Link, paths } from "@/lib/router";
import { useApi, useBusy, useTail } from "@/lib/use-api";
import type { LLMExchangeDetail, LLMUsageRecord } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, LoadMore, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function LlmPage({ token, onAuthError }: PageProps) {
  const [groupBy, setGroupBy] = useState("model");
  const [role, setRole] = useState("");
  const [model, setModel] = useState("");
  const [detail, setDetail] = useState<LLMExchangeDetail | null>(null);
  const [body, setBody] = useState("");
  const records = useApi({
    token,
    interval: 15000,
    watch: [role, model],
    onAuthError,
    load: (current, signal) => listLLMRecords(current, { role: role || undefined, model: model || undefined, limit: 100 }, signal),
  });
  const summary = useApi({
    token,
    interval: 15000,
    watch: [groupBy],
    onAuthError,
    load: (current, signal) => llmSummary(current, { group_by: groupBy, days: 30 }, signal),
  });
  const tail = useTail<LLMUsageRecord>(`${role}|${model}`);
  const busy = useBusy(onAuthError);
  const rows = [...(records.data?.records ?? []), ...tail.rows];
  const nextId = tail.rows.length ? tail.next : (records.data?.next_after_id ?? 0);
  return (
    <>
      <PageHeader kicker="调用账本" title="模型记录" description="列表是用量。正文按点击加载，且已经脱敏。今日与累计口径以概览页说明为准。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <div className="grid-2">
        <Panel title="近 30 天聚合" extra={<select value={groupBy} onChange={(event) => setGroupBy(event.target.value)}><option value="model">按模型</option><option value="role">按角色</option><option value="day">按天</option></select>}>
          <QueryBody token={token} loading={summary.loading} error={summary.error} empty={(summary.data?.groups ?? []).length === 0} emptyTitle="没有用量">
            <table>
              <thead><tr><th>键</th><th>次数</th><th>token</th></tr></thead>
              <tbody>{(summary.data?.groups ?? []).map((group) => <tr key={group.key}><td className="num">{group.key}</td><td>{formatNumber(group.calls)}</td><td className="num">{formatNumber(group.total_tokens)}</td></tr>)}</tbody>
            </table>
          </QueryBody>
        </Panel>
        <Panel title="记录">
          <div className="filters">
            <input value={role} placeholder="角色" onChange={(event) => setRole(event.target.value)} />
            <input value={model} placeholder="模型" onChange={(event) => setModel(event.target.value)} />
          </div>
          <QueryBody token={token} loading={records.loading} error={records.error} empty={rows.length === 0} emptyTitle="没有记录">
            <table>
              <thead><tr><th>调用</th><th>token</th><th /></tr></thead>
              <tbody>
                {rows.map((row) => (
                  <tr key={row.id}>
                    <td>{row.role || "—"} · {row.model}<div className="muted">{row.mission_id ? <Link href={paths.mission(row.mission_id)}>任务 #{row.mission_id}</Link> : "无任务"} · {formatTime(row.created_at)}</div></td>
                    <td className="num">{formatNumber(row.total_tokens)}</td>
                    <td><button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`d${row.id}`, async () => { setDetail(await getLLMRecord(token, row.id)); setBody(""); })}>详情</button></td>
                  </tr>
                ))}
              </tbody>
            </table>
            <LoadMore nextId={nextId} busy={Boolean(busy.busy)} onLoad={() => void busy.run("more", async () => { const page = await listLLMRecords(token, { role: role || undefined, model: model || undefined, limit: 100, after_id: nextId }); tail.push(page.records ?? [], page.next_after_id); })} />
          </QueryBody>
        </Panel>
      </div>
      {detail ? (
        <Panel title={`调用 #${detail.id}`}>
          <p>{detail.role} · {detail.model} · 请求 {formatBytes(detail.request_bytes)} · 响应 {formatBytes(detail.response_bytes)} {detail.truncated ? "· 已截断" : ""} {detail.redacted ? "· 已脱敏" : ""}</p>
          {detail.error ? <p className="muted">{detail.error}</p> : null}
          <div className="row-actions">
            <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run("req", async () => { setBody(await llmBody(token, detail.id, "request")); })}>请求</button>
            <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run("res", async () => { setBody(await llmBody(token, detail.id, "response")); })}>响应</button>
          </div>
          {body ? <pre className="json">{body}</pre> : null}
        </Panel>
      ) : null}
    </>
  );
}

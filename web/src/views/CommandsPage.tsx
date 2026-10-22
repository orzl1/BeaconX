"use client";

import { useState } from "react";

import { getCommand, listCommands } from "@/lib/api";
import { formatTime, label, missionStatusTone, shortHash } from "@/lib/format";
import { Link, paths } from "@/lib/router";
import { useApi, useBusy, useTail } from "@/lib/use-api";
import type { AuditEvent } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { Badge, ErrorBanner, LoadMore, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

const OUTCOMES = ["executed", "tool_error", "denied_by_rule", "rejected_by_user", "approval_unavailable"];

export function CommandsPage({ token, onAuthError }: PageProps) {
  const [missionId, setMissionId] = useState("");
  const [tool, setTool] = useState("");
  const [outcome, setOutcome] = useState("");
  const [selected, setSelected] = useState<AuditEvent | null>(null);
  const list = useApi({
    token,
    interval: 10000,
    watch: [missionId, tool, outcome],
    onAuthError,
    load: (current, signal) => listCommands(current, { mission_id: Number(missionId) || undefined, tool: tool || undefined, outcome: outcome || undefined, limit: 100 }, signal),
  });
  const tail = useTail<AuditEvent>(`${missionId}|${tool}|${outcome}`);
  const busy = useBusy(onAuthError);
  const rows = [...(list.data?.commands ?? []), ...tail.rows];
  const nextId = tail.rows.length ? tail.next : (list.data?.next_after_id ?? 0);
  return (
    <>
      <PageHeader kicker="审计视图" title="命令" description="只读。每一行是一次工具调用的策略判定，没有取消或重放。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <div className="filters">
        <input value={missionId} placeholder="任务 ID" onChange={(event) => setMissionId(event.target.value)} />
        <input value={tool} placeholder="工具" onChange={(event) => setTool(event.target.value)} />
        <select value={outcome} onChange={(event) => setOutcome(event.target.value)}><option value="">全部结果</option>{OUTCOMES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select>
      </div>
      <div className="grid-2">
        <Panel>
          <QueryBody token={token} loading={list.loading} error={list.error} empty={rows.length === 0} emptyTitle="没有命令记录">
            <table>
              <thead><tr><th>工具</th><th>结果</th><th>时间</th></tr></thead>
              <tbody>
                {rows.map((item) => (
                  <tr key={item.id} className="link" onClick={() => void busy.run(`c${item.id}`, async () => { setSelected(await getCommand(token, item.id)); })}>
                    <td>{item.tool}<div className="muted">{label(item.action)} · {item.rule_name || "—"}</div></td>
                    <td><Badge tone={missionStatusTone(item.outcome)}>{label(item.outcome)}</Badge></td>
                    <td className="num">{formatTime(item.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <LoadMore nextId={nextId} busy={Boolean(busy.busy)} onLoad={() => void busy.run("more", async () => { const page = await listCommands(token, { mission_id: Number(missionId) || undefined, tool: tool || undefined, outcome: outcome || undefined, limit: 100, after_id: nextId }); tail.push(page.commands ?? [], page.next_after_id); })} />
          </QueryBody>
        </Panel>
        <Panel title={selected ? `#${selected.id}` : "详情"}>
          {selected ? (
            <>
              <p><Link href={paths.mission(selected.mission_id)}>任务 #{selected.mission_id}</Link> · 节点 {selected.node_id ?? "—"}</p>
              <p>参数摘要 <span className="num">{shortHash(selected.args_digest)}</span></p>
              <p className="muted">{selected.detail || "没有详情。"}</p>
              <p className="muted">操作者 {selected.actor || "—"}</p>
            </>
          ) : <p className="muted">点一行查看。</p>}
        </Panel>
      </div>
    </>
  );
}

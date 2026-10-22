"use client";

import { useEffect, useState } from "react";

import { listLogs, sseUrl } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { useApi } from "@/lib/use-api";
import type { LogEntry } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function LogsPage({ token, onAuthError }: PageProps) {
  const [level, setLevel] = useState("");
  const [live, setLive] = useState<LogEntry[]>([]);
  const [connected, setConnected] = useState(false);
  const history = useApi({
    token,
    watch: [level],
    onAuthError,
    load: (current, signal) => listLogs(current, { level: level || undefined, limit: 200 }, signal),
  });
  useEffect(() => {
    if (!token) return;
    let disposed = false;
    const source = new EventSource(sseUrl(`/api/v1/logs/stream${level ? `?level=${encodeURIComponent(level)}` : ""}`, token));
    source.onopen = () => { if (!disposed) setConnected(true); };
    source.onerror = () => { if (!disposed) setConnected(false); };
    const listener = (event: Event) => {
      if (disposed) return;
      const message = event as MessageEvent<string>;
      try {
        const entry = JSON.parse(message.data) as LogEntry;
        setLive((current) => [entry, ...current].slice(0, 200));
      } catch { /* 心跳或坏帧忽略 */ }
    };
    source.addEventListener("log", listener);
    return () => { disposed = true; source.removeEventListener("log", listener); source.close(); };
  }, [token, level]);
  const rows = live.length > 0 ? live : history.data?.logs ?? [];
  return (
    <>
      <PageHeader kicker={connected ? "实时流已连接" : "实时流重连中"} title="日志" description="进程内环形缓冲，刷新页面后的更早日志不在这里。实时帧与历史列表用同一套字段。" />
      <TokenNotice token={token} />
      <div className="filters">
        <select value={level} onChange={(event) => { setLevel(event.target.value); setLive([]); }}>
          <option value="">全部级别</option>
          {["debug", "info", "warn", "error"].map((item) => <option key={item}>{item}</option>)}
        </select>
      </div>
      <Panel>
        <QueryBody token={token} loading={history.loading && rows.length === 0} error={history.error} empty={rows.length === 0} emptyTitle="没有日志">
          <table>
            <thead><tr><th>时间</th><th>级别</th><th>消息</th></tr></thead>
            <tbody>
              {rows.map((entry) => (
                <tr key={`${entry.id}-${entry.time}`}>
                  <td className="num">{formatTime(entry.time)}</td>
                  <td>{entry.level}</td>
                  <td>{entry.msg}<div className="muted">{Object.keys(entry.attrs ?? {}).length ? JSON.stringify(entry.attrs) : ""}</div></td>
                </tr>
              ))}
            </tbody>
          </table>
        </QueryBody>
      </Panel>
    </>
  );
}

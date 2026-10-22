"use client";

import { useState } from "react";

import { createMcpServer, deleteMcpServer, listMcpServers, listMcpTools, refreshMcpServer, updateMcpServer } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { useApi, useBusy } from "@/lib/use-api";
import type { McpTool } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, Field, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function McpPage({ token, onAuthError }: PageProps) {
  const [serverId, setServerId] = useState<number | undefined>(undefined);
  const [tools, setTools] = useState<McpTool[] | null>(null);
  const servers = useApi({ token, onAuthError, load: (current, signal) => listMcpServers(current, signal) });
  const catalog = useApi({ token, watch: [serverId], onAuthError, load: (current, signal) => listMcpTools(current, serverId, signal) });
  const busy = useBusy(onAuthError);
  const shown = tools ?? catalog.data?.tools ?? [];
  return (
    <>
      <PageHeader kicker="外部工具" title="MCP" description="刷新失败时后端保留上一份工具清单。stdio 在未放开的环境会返回 400。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="端点">
          <form onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); const transport = String(data.get("transport")); void busy.run("new", async () => { await createMcpServer(token, { name: String(data.get("name")), transport, url: transport === "http" ? String(data.get("url") || "") : "", command: transport === "stdio" ? String(data.get("command") || "") : "", enabled: true }); servers.reload(); return "已创建"; }); }}>
            <Field label="名称"><input name="name" required /></Field>
            <Field label="传输"><select name="transport"><option value="http">http</option><option value="stdio">stdio</option></select></Field>
            <Field label="URL"><input name="url" placeholder="https://..." /></Field>
            <Field label="命令"><input name="command" placeholder="stdio 命令" /></Field>
            <button className="btn btn-primary btn-sm" type="submit">创建</button>
          </form>
          <QueryBody token={token} loading={servers.loading} error={servers.error} empty={(servers.data?.servers ?? []).length === 0} emptyTitle="没有端点">
            {(servers.data?.servers ?? []).map((server) => (
              <div key={server.id} style={{ borderTop: "1px solid var(--line)", padding: "8px 0" }}>
                <strong>{server.name}</strong> <span className="muted">{server.transport} · {server.last_status || "未刷新"} {server.last_error}</span>
                <div className="muted">{server.url || server.command} · {formatTime(server.last_refreshed_at)}</div>
                <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run(`e${server.id}`, async () => { await updateMcpServer(token, server.id, { name: String(data.get("name") || server.name), url: String(data.get("url") || ""), command: String(data.get("command") || ""), enabled: data.get("enabled") === "1" }); servers.reload(); return "已更新端点"; }); }}>
                  <input name="name" defaultValue={server.name} aria-label="名称" />
                  <input name="url" defaultValue={server.url} placeholder="URL" aria-label="URL" />
                  <input name="command" defaultValue={server.command} placeholder="命令" aria-label="命令" />
                  <select name="enabled" defaultValue={server.enabled ? "1" : "0"} aria-label="启用"><option value="1">启用</option><option value="0">停用</option></select>
                  <button className="btn btn-ghost btn-sm" type="submit">保存</button>
                </form>
                <div className="row-actions">
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => setServerId(server.id)}>看工具</button>
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`rf${server.id}`, async () => { const result = await refreshMcpServer(token, server.id); setServerId(server.id); setTools(result.tools ?? []); servers.reload(); return `刷新到 ${(result.tools ?? []).length} 个工具`; })}>刷新</button>
                  <button type="button" className="btn btn-danger btn-sm" onClick={() => void busy.run(`del${server.id}`, async () => { await deleteMcpServer(token, server.id); servers.reload(); })}>删除</button>
                </div>
              </div>
            ))}
          </QueryBody>
        </Panel>
        <Panel title="工具">
          {catalog.error ? <p className="muted">{catalog.error}</p> : null}
          {shown.length === 0 ? <p className="muted">没有工具。先刷新端点。</p> : shown.map((tool) => <p key={`${tool.server_id}-${tool.name}`}><strong>{tool.name}</strong> <span className="muted">端点 #{tool.server_id} · {tool.description}</span></p>)}
        </Panel>
      </div>
    </>
  );
}

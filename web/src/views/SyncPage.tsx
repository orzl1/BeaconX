"use client";

import { createSyncProject, createSyncSource, deleteSyncSource, listSyncProjects, listSyncRuns, listSyncSources, runSync, updateSyncSource } from "@/lib/api";
import { formatTime, label, parseObject, pretty } from "@/lib/format";
import { useApi, useBusy } from "@/lib/use-api";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, Field, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function SyncPage({ token, onAuthError }: PageProps) {
  const sources = useApi({ token, interval: 10000, onAuthError, load: (current, signal) => listSyncSources(current, signal) });
  const projects = useApi({ token, interval: 10000, onAuthError, load: (current, signal) => listSyncProjects(current, undefined, signal) });
  const runs = useApi({ token, interval: 10000, onAuthError, load: (current, signal) => listSyncRuns(current, signal) });
  const busy = useBusy(onAuthError);
  return (
    <>
      <PageHeader kicker="外部数据" title="同步" description="http_json 需要 config.url。默认拒绝私网地址。运行记录里的 pending 在进程重启后不会自动恢复。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="数据源">
          <form onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("src", async () => { try { await createSyncSource(token, { name: String(data.get("name")), type: String(data.get("type")), config: parseObject(String(data.get("config") || "{}")), enabled: true }); } catch (error) { if (error instanceof SyntaxError) throw new Error("配置必须是 JSON 对象"); throw error; } sources.reload(); return "已创建数据源"; }); }}>
            <Field label="名称"><input name="name" required /></Field>
            <Field label="类型"><select name="type"><option value="manual">manual</option><option value="http_json">http_json</option></select></Field>
            <Field label="配置" hint='http_json 示例 {"url":"https://example.test/feed.json"}'><textarea name="config" defaultValue="{}" /></Field>
            <button className="btn btn-primary btn-sm" type="submit">创建</button>
          </form>
          <QueryBody token={token} loading={sources.loading} error={sources.error} empty={(sources.data?.sources ?? []).length === 0} emptyTitle="没有数据源">
            {(sources.data?.sources ?? []).map((source) => (
              <div key={source.id} className="row-actions" style={{ marginTop: 8 }}>
                <span>#{source.id} {source.name} · {source.type} {source.enabled ? "" : "· 停用"}</span>
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`run${source.id}`, async () => { const run = await runSync(token, { source_id: source.id }); runs.reload(); return `已触发运行 #${run.id}，状态 ${label(run.status)}`; })}>运行</button>
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`en${source.id}`, async () => { await updateSyncSource(token, source.id, { enabled: !source.enabled }); sources.reload(); return source.enabled ? "已停用" : "已启用"; })}>{source.enabled ? "停用" : "启用"}</button>
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`ed${source.id}`, async () => { const name = window.prompt("名称", source.name); if (name === null) return; const configText = window.prompt("配置 JSON，留空不变", pretty(source.config) === "（无数据）" ? "{}" : pretty(source.config)); if (configText === null) return; await updateSyncSource(token, source.id, { name, ...(configText.trim() ? { config: parseObject(configText) } : {}) }); sources.reload(); return "已更新数据源"; })}>编辑</button>
                <button type="button" className="btn btn-danger btn-sm" onClick={() => void busy.run(`del${source.id}`, async () => { await deleteSyncSource(token, source.id); sources.reload(); })}>删除</button>
              </div>
            ))}
          </QueryBody>
        </Panel>
        <Panel title="外部项目">
          <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("proj", async () => { await createSyncProject(token, { source_id: Number(data.get("source_id")), external_id: String(data.get("external_id")), name: String(data.get("name")) }); projects.reload(); return "已写入项目"; }); }}>
            <input name="source_id" type="number" min={1} required placeholder="数据源 ID" />
            <input name="external_id" required placeholder="外部 ID" />
            <input name="name" required placeholder="名称" />
            <button className="btn btn-primary btn-sm" type="submit">添加</button>
          </form>
          {(projects.data?.projects ?? []).map((project) => <p key={project.id}>#{project.id} {project.name} · {project.external_id}<span className="muted"> {pretty(project.data)}</span> <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`pr${project.id}`, async () => { const run = await runSync(token, { source_id: project.source_id, project_id: project.id }); runs.reload(); return `已触发运行 #${run.id}`; })}>运行此项目</button></p>)}
          {(projects.data?.projects ?? []).length === 0 ? <p className="muted">没有项目。</p> : null}
          {projects.error ? <p className="muted">{projects.error}</p> : null}
        </Panel>
      </div>
      <Panel title="运行">
        <QueryBody token={token} loading={runs.loading} error={runs.error} empty={(runs.data?.runs ?? []).length === 0} emptyTitle="还没有运行">
          <table>
            <thead><tr><th>ID</th><th>状态</th><th>说明</th><th>时间</th></tr></thead>
            <tbody>{(runs.data?.runs ?? []).map((run) => <tr key={run.id}><td className="num">#{run.id} / 源 {run.source_id}</td><td>{label(run.status)}</td><td>{run.error || pretty(run.stats) || "—"}</td><td className="num">{formatTime(run.created_at)}</td></tr>)}</tbody>
          </table>
        </QueryBody>
      </Panel>
    </>
  );
}

"use client";

import { useEffect, useState } from "react";

import { createAgent, deleteAgent, getAgent, listAgents, testAgent, updateAgent } from "@/lib/api";
import { useApi, useBusy } from "@/lib/use-api";
import { Link, paths } from "@/lib/router";
import type { Agent, AgentTestResult } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, Field, JsonBlock, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

const ROLES = ["planner", "worker", "retester", "main"];

export function AgentsPage({ token, onAuthError, agentKey }: PageProps & { agentKey?: string }) {
  const list = useApi({ token, onAuthError, load: (current, signal) => listAgents(current, signal) });
  const busy = useBusy(onAuthError);
  return (
    <>
      <PageHeader kicker="运行时定义" title="Agents" description="同一角色取 ID 最小的定义接入规划器和执行器。改完后正在跑的任务不会自动换定义。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="列表">
          <QueryBody token={token} loading={list.loading} error={list.error} empty={(list.data?.agents ?? []).length === 0} emptyTitle="还没有 Agent">
            {(list.data?.agents ?? []).map((agent) => (
              <p key={agent.key}><Link href={paths.agent(agent.key)} current={agent.key === agentKey}>{agent.name || agent.key}</Link> <span className="muted">{agent.role} · #{agent.id} {agent.enabled ? "" : "· 停用"}</span></p>
            ))}
          </QueryBody>
          <form style={{ marginTop: 12 }} onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("new", async () => { const created = await createAgent(token, { key: String(data.get("key")), name: String(data.get("name")), role: String(data.get("role")), model: String(data.get("model") || ""), system_prompt: String(data.get("system_prompt") || ""), tools: String(data.get("tools") || "").split(",").map((item) => item.trim()).filter(Boolean), enabled: true }); list.reload(); return `已创建 ${created.key}`; }); }}>
            <Field label="key" hint="小写字母或数字开头"><input name="key" required pattern="[a-z0-9][a-z0-9_-]{0,63}" /></Field>
            <Field label="名称"><input name="name" required /></Field>
            <Field label="角色"><select name="role">{ROLES.map((role) => <option key={role}>{role}</option>)}</select></Field>
            <Field label="模型"><input name="model" /></Field>
            <Field label="工具" hint="逗号分隔"><input name="tools" /></Field>
            <Field label="系统提示"><textarea name="system_prompt" /></Field>
            <button className="btn btn-primary btn-sm" type="submit">创建</button>
          </form>
        </Panel>
        <Editor token={token} agentKey={agentKey} onAuthError={onAuthError} onChanged={list.reload} />
      </div>
    </>
  );
}

function Editor({ token, agentKey, onAuthError, onChanged }: { token: string; agentKey?: string; onAuthError: (m: string) => void; onChanged: () => void }) {
  const detail = useApi({ token, enabled: Boolean(agentKey), watch: [agentKey], onAuthError, load: (current, signal) => getAgent(current, agentKey ?? "", signal) });
  const [draft, setDraft] = useState<Agent | null>(null);
  const [test, setTest] = useState<AgentTestResult | null>(null);
  const busy = useBusy(onAuthError);
  useEffect(() => setDraft(detail.data), [detail.data]);
  if (!agentKey) return <Panel title="编辑"><p className="muted">从左侧点开一个 Agent。</p></Panel>;
  if (!draft) return <Panel title="编辑"><QueryBody token={token} loading={detail.loading} error={detail.error} empty> </QueryBody></Panel>;
  return (
    <Panel title={draft.key}>
      <ErrorBanner message={busy.error ?? ""} />
      <Field label="名称"><input value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} /></Field>
      <Field label="角色"><select value={draft.role} onChange={(event) => setDraft({ ...draft, role: event.target.value })}>{ROLES.map((role) => <option key={role}>{role}</option>)}</select></Field>
      <Field label="模型"><input value={draft.model} onChange={(event) => setDraft({ ...draft, model: event.target.value })} /></Field>
      <Field label="工具"><input value={(draft.tools ?? []).join(", ")} onChange={(event) => setDraft({ ...draft, tools: event.target.value.split(",").map((item) => item.trim()).filter(Boolean) })} /></Field>
      <Field label="系统提示"><textarea value={draft.system_prompt} onChange={(event) => setDraft({ ...draft, system_prompt: event.target.value })} /></Field>
      <label className="field"><span>启用</span><input type="checkbox" checked={draft.enabled} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} /></label>
      <div className="row-actions">
        <button type="button" className="btn btn-primary btn-sm" onClick={() => void busy.run("save", async () => { await updateAgent(token, draft.key, { name: draft.name, role: draft.role, model: draft.model, tools: draft.tools, system_prompt: draft.system_prompt, enabled: draft.enabled }); onChanged(); return "已保存"; })}>保存</button>
        <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run("test", async () => { setTest(await testAgent(token, draft.key)); return "试跑完成"; })}>试跑</button>
        <button type="button" className="btn btn-danger btn-sm" onClick={() => { if (!window.confirm(`删除 ${draft.key}？`)) return; void busy.run("del", async () => { await deleteAgent(token, draft.key); onChanged(); }); }}>删除</button>
      </div>
      {test ? <JsonBlock value={test} /> : null}
    </Panel>
  );
}

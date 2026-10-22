"use client";

import { useState } from "react";

import { createPolicyRule, deletePolicyRule, listPolicyRules, simulatePolicy, updatePolicyRule } from "@/lib/api";
import { label, missionStatusTone, parseObject } from "@/lib/format";
import { useApi, useBusy } from "@/lib/use-api";
import type { PageProps } from "@/components/Console";
import { Badge, ErrorBanner, Field, JsonBlock, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function PolicyPage({ token, onAuthError }: PageProps) {
  const rules = useApi({ token, onAuthError, load: (current, signal) => listPolicyRules(current, signal) });
  const busy = useBusy(onAuthError);
  const [simulation, setSimulation] = useState<unknown>(null);
  return (
    <>
      <PageHeader kicker="执行前判定" title="策略" description="内置规则除启用开关外不能改。模拟只看当前启用规则，不产生审批。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="规则">
          <QueryBody token={token} loading={rules.loading} error={rules.error} empty={(rules.data?.rules ?? []).length === 0} emptyTitle="没有规则">
            <table>
              <thead><tr><th>名称</th><th>裁决</th><th /></tr></thead>
              <tbody>
                {(rules.data?.rules ?? []).map((rule) => (
                  <tr key={rule.id}>
                    <td>{rule.name}<div className="muted">{rule.tool || "*"} · 优先级 {rule.priority} {rule.immutable ? "· 内置" : ""}</div><div className="num">{rule.arg_pattern || "匹配全部参数"}</div></td>
                    <td><Badge tone={missionStatusTone(rule.action)}>{label(rule.action)}</Badge></td>
                    <td className="row-actions">
                      <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`en${rule.id}`, async () => { await updatePolicyRule(token, rule.id, { enabled: !rule.enabled }); rules.reload(); })}>{rule.enabled ? "停用" : "启用"}</button>
                      <button type="button" className="btn btn-ghost btn-sm" disabled={rule.immutable} onClick={() => { const name = window.prompt("规则名称", rule.name); if (name === null) return; const pattern = window.prompt("参数正则，留空表示匹配全部", rule.arg_pattern); if (pattern === null) return; void busy.run(`ed${rule.id}`, async () => { await updatePolicyRule(token, rule.id, { name, arg_pattern: pattern, tool: rule.tool, action: rule.action, priority: rule.priority, timeout_seconds: rule.timeout_seconds }); rules.reload(); return "已更新规则"; }); }}>编辑</button>
                      <button type="button" className="btn btn-danger btn-sm" disabled={rule.immutable} onClick={() => void busy.run(`del${rule.id}`, async () => { await deletePolicyRule(token, rule.id); rules.reload(); })}>删除</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </QueryBody>
        </Panel>
        <div className="stack">
          <Panel title="新建">
            <form onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("new", async () => { await createPolicyRule(token, { name: String(data.get("name")), tool: String(data.get("tool")), arg_pattern: String(data.get("arg_pattern") || ""), action: String(data.get("action")), priority: Number(data.get("priority") || 0), timeout_seconds: Number(data.get("timeout_seconds") || 0), enabled: true }); rules.reload(); return "已创建"; }); }}>
              <Field label="名称"><input name="name" required /></Field>
              <Field label="工具" hint="空或 * 表示全部"><input name="tool" placeholder="http_probe" /></Field>
              <Field label="参数正则"><input name="arg_pattern" /></Field>
              <div className="form-grid">
                <Field label="裁决"><select name="action"><option value="allow">放行</option><option value="ask">询问</option><option value="deny">拒绝</option></select></Field>
                <Field label="优先级"><input name="priority" type="number" defaultValue={10} /></Field>
              </div>
              <Field label="审批超时（秒）"><input name="timeout_seconds" type="number" defaultValue={60} /></Field>
              <button className="btn btn-primary btn-sm" type="submit">创建</button>
            </form>
          </Panel>
          <Panel title="模拟">
            <form onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("sim", async () => { let args: Record<string, unknown>; try { args = parseObject(String(data.get("args") || "{}")); } catch { throw new Error("参数必须是 JSON 对象"); } setSimulation(await simulatePolicy(token, { tool: String(data.get("tool")), args })); return ""; }); }}>
              <Field label="工具"><input name="tool" required /></Field>
              <Field label="参数"><textarea name="args" defaultValue="{}" /></Field>
              <button className="btn btn-ghost btn-sm" type="submit">试算</button>
            </form>
            {simulation ? <JsonBlock value={simulation} /> : null}
          </Panel>
        </div>
      </div>
    </>
  );
}

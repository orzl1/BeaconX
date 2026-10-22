"use client";

import { useEffect, useState } from "react";

import { applyUpdate, checkUpdate, getHealth, getReadiness, getSettings, getSystemInfo, patchSettings, rollbackUpdate, rotateEvidence } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { useApi, useBusy } from "@/lib/use-api";
import type { SettingsView } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, Field, JsonBlock, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function SettingsPage({ token, onAuthError }: PageProps) {
  const settings = useApi({ token, onAuthError, load: (current, signal) => getSettings(current, signal) });
  const info = useApi({ token: "public", interval: 10000, onAuthError: () => undefined, load: () => getSystemInfo() });
  const health = useApi({ token: "public", interval: 10000, onAuthError: () => undefined, load: () => getHealth() });
  const ready = useApi({ token: "public", interval: 10000, onAuthError: () => undefined, load: () => getReadiness() });
  const busy = useBusy(onAuthError);
  const [draft, setDraft] = useState<SettingsView | null>(null);
  const [update, setUpdate] = useState<unknown>(null);
  const [keyHex, setKeyHex] = useState("");
  useEffect(() => setDraft(settings.data?.settings ?? null), [settings.data]);
  return (
    <>
      <PageHeader kicker="运行参数" title="设置" description="保存后立即作用于新的判定和之后启动的任务。已经在跑的任务预算不会回溯，调低并发也不会抢走正在运行的名额。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="参数">
          <QueryBody token={token} loading={settings.loading} error={settings.error} empty={!draft}>
            {draft ? (
              <form onSubmit={(event) => { event.preventDefault(); void busy.run("save", async () => { const saved = await patchSettings(token, draft); setDraft(saved.settings); return "已保存并热生效"; }); }}>
                <Field label="最大并发" hint="1 到 64"><input type="number" min={1} max={64} value={draft.max_concurrent} onChange={(event) => setDraft({ ...draft, max_concurrent: Number(event.target.value) })} /></Field>
                <Field label="默认 Token 上限" hint="0 表示不限"><input type="number" min={0} value={draft.default_budget.max_tokens} onChange={(event) => setDraft({ ...draft, default_budget: { ...draft.default_budget, max_tokens: Number(event.target.value) } })} /></Field>
                <Field label="默认步骤上限"><input type="number" min={0} value={draft.default_budget.max_steps} onChange={(event) => setDraft({ ...draft, default_budget: { ...draft.default_budget, max_steps: Number(event.target.value) } })} /></Field>
                <Field label="默认裁决"><select value={draft.default_action} onChange={(event) => setDraft({ ...draft, default_action: event.target.value })}><option value="allow">放行</option><option value="ask">询问</option><option value="deny">拒绝</option></select></Field>
                <Field label="审批超时"><input value={draft.approval_timeout} onChange={(event) => setDraft({ ...draft, approval_timeout: event.target.value })} /></Field>
                <button className="btn btn-primary" type="submit" disabled={Boolean(busy.busy)}>保存</button>
              </form>
            ) : null}
          </QueryBody>
        </Panel>
        <div className="stack">
          <Panel title="探针">
            <p>健康 {health.data?.status || health.error || "读取中"}</p>
            <p>就绪 {ready.data ? (ready.data.ready ? "是" : "否") : ready.error || "读取中"}</p>
            {(ready.data?.checks ?? []).map((check) => <p key={check.name} className="muted">{check.name} · {check.ok ? "正常" : check.error || "失败"}</p>)}
            {info.data ? <p className="muted">{info.data.service} {info.data.version} · {info.data.commit} · 启动 {formatTime(info.data.started_at)} · 运行 {info.data.uptime} · schema {info.data.schema_version}</p> : <p className="muted">{info.error}</p>}
          </Panel>
          <Panel title="自更新">
            <div className="row-actions">
              <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run("check", async () => { const result = await checkUpdate(token); setUpdate(result); return result.has_update ? `有更新 ${result.release?.version || ""}` : "没有更新"; })}>检查</button>
              <button type="button" className="btn btn-primary btn-sm" onClick={() => { if (!window.confirm("应用更新会替换当前二进制，之后需要重启。继续？")) return; void busy.run("apply", async () => { const result = await applyUpdate(token); setUpdate(result); return result.message || (result.updated ? `已更新到 ${result.version}` : "未更新"); }); }}>应用</button>
              <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run("rollback", async () => { const result = await rollbackUpdate(token); setUpdate(result); return result.message; })}>回滚</button>
            </div>
            {update ? <JsonBlock value={update} /> : null}
          </Panel>
          <Panel title="证据密钥轮换">
            <p className="muted">新密钥只用于这一次进程，不会写入磁盘。完成后要自己更新 BEACONX_EVIDENCE_KEY 或 evidence.key 并重启。</p>
            <Field label="新密钥（64 位十六进制）"><input value={keyHex} onChange={(event) => setKeyHex(event.target.value.trim())} /></Field>
            <button type="button" className="btn btn-danger btn-sm" onClick={() => { if (!/^[0-9a-fA-F]{64}$/.test(keyHex)) { busy.setError("密钥必须是 64 位十六进制"); return; } if (!window.confirm("轮换会重加密全部证据并重签快照。继续？")) return; void busy.run("rotate", async () => { const result = await rotateEvidence(token, keyHex); setKeyHex(""); return result.notice || `已轮换 ${result.rotated}，重签 ${result.resigned}`; }); }}>轮换</button>
          </Panel>
        </div>
      </div>
    </>
  );
}

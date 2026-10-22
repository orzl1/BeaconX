"use client";

import { useState } from "react";

import { createMission, deleteMission, listMissions, missionAction } from "@/lib/api";
import { formatTime, label, missionActions, missionBudgetText, missionStatusTone, MISSION_STATUSES } from "@/lib/format";
import { Link, paths } from "@/lib/router";
import { useApi, useBusy } from "@/lib/use-api";
import type { PageProps } from "@/components/Console";
import { Badge, Drawer, ErrorBanner, Field, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function MissionsPage({ token, onAuthError }: PageProps) {
  const [status, setStatus] = useState("");
  const [open, setOpen] = useState(false);
  const list = useApi({
    token,
    interval: 5000,
    watch: [status],
    onAuthError,
    load: (current, signal) => listMissions(current, { status: status || undefined, limit: 100 }, signal),
  });
  const busy = useBusy(onAuthError);
  const missions = list.data?.missions ?? [];

  return (
    <>
      <PageHeader
        kicker="授权测试"
        title="任务"
        description="列表约 5 秒刷新。暂停只对运行中，恢复只对已暂停，取消对排队、运行、暂停和收尾有效。删除会级联清掉任务数据。"
        actions={<button type="button" className="btn btn-primary" onClick={() => setOpen(true)}>新建任务</button>}
      />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} onDismiss={() => busy.setError(null)} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="filters">
        <select value={status} onChange={(event) => setStatus(event.target.value)} aria-label="按状态筛选">
          <option value="">全部状态</option>
          {MISSION_STATUSES.map((item) => <option key={item} value={item}>{label(item)}</option>)}
        </select>
      </div>
      <Panel>
        <QueryBody token={token} loading={list.loading} error={list.error} empty={missions.length === 0} emptyTitle="还没有任务">
          <table>
            <thead><tr><th>名称</th><th>状态</th><th>预算</th><th>更新</th><th /></tr></thead>
            <tbody>
              {missions.map((mission) => {
                const actions = missionActions(mission.status);
                return (
                  <tr key={mission.id}>
                    <td><Link href={paths.mission(mission.id)}><strong>{mission.name}</strong></Link><div className="muted">#{mission.id}</div></td>
                    <td><Badge tone={missionStatusTone(mission.status)}><i />{label(mission.status)}</Badge></td>
                    <td className="num">{missionBudgetText(mission)}</td>
                    <td className="num">{formatTime(mission.updated_at)}</td>
                    <td>
                      <div className="row-actions">
                        <button type="button" className="btn btn-ghost btn-sm" disabled={!actions.pause || Boolean(busy.busy)} onClick={() => void busy.run(`p${mission.id}`, async () => { await missionAction(token, mission.id, "pause"); list.reload(); })}>暂停</button>
                        <button type="button" className="btn btn-ghost btn-sm" disabled={!actions.resume || Boolean(busy.busy)} onClick={() => void busy.run(`r${mission.id}`, async () => { await missionAction(token, mission.id, "resume"); list.reload(); })}>恢复</button>
                        <button type="button" className="btn btn-ghost btn-sm" disabled={!actions.cancel || Boolean(busy.busy)} onClick={() => void busy.run(`c${mission.id}`, async () => { await missionAction(token, mission.id, "cancel"); list.reload(); })}>取消</button>
                        <button type="button" className="btn btn-danger btn-sm" disabled={Boolean(busy.busy)} onClick={() => { if (!window.confirm(`删除任务「${mission.name}」？关联数据会一并删除。`)) return; void busy.run(`d${mission.id}`, async () => { await deleteMission(token, mission.id); list.reload(); return "已删除"; }); }}>删除</button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </QueryBody>
      </Panel>
      <Drawer open={open} title="新建任务" onClose={() => setOpen(false)}>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            const form = new FormData(event.currentTarget);
            const maxTokens = Number(form.get("max_tokens") || 0);
            const maxSteps = Number(form.get("max_steps") || 0);
            void busy.run("create", async () => {
              const created = await createMission(token, {
                name: String(form.get("name") || ""),
                goal: String(form.get("goal") || ""),
                description: String(form.get("description") || ""),
                max_tokens: Number.isFinite(maxTokens) ? maxTokens : 0,
                max_steps: Number.isFinite(maxSteps) ? maxSteps : 0,
              });
              setOpen(false);
              list.reload();
              return `已创建任务 #${created.id}，状态 ${label(created.status)}`;
            });
          }}
        >
          <Field label="名称"><input name="name" required /></Field>
          <Field label="目标"><textarea name="goal" /></Field>
          <Field label="描述"><textarea name="description" /></Field>
          <div className="form-grid">
            <Field label="Token 上限" hint="0 表示不限"><input name="max_tokens" type="number" min={0} defaultValue={0} /></Field>
            <Field label="步骤上限" hint="0 表示不限"><input name="max_steps" type="number" min={0} defaultValue={0} /></Field>
          </div>
          <button type="submit" className="btn btn-primary" disabled={Boolean(busy.busy)}>创建</button>
        </form>
      </Drawer>
    </>
  );
}

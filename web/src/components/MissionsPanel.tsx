"use client";

// 任务面板：列表 / 创建 / 暂停 / 恢复 / 取消 / 删除。
//
// 操作可用性严格按后端状态机（internal/app/runner/engine.go）：
//   pause  仅 running；resume 仅 paused；cancel 排队/运行/暂停/收尾中；delete 任意状态。

import { useCallback, useEffect, useMemo, useState } from "react";
import type { FormEvent } from "react";

import {
  createMission,
  deleteMission,
  describeError,
  isAuthError,
  listMissions,
  missionAction,
} from "@/lib/api";
import { formatNumber, formatTime, missionBudgetText, missionStatusLabel, missionStatusTone } from "@/lib/format";
import { useInterval } from "@/lib/hooks";
import type { Mission } from "@/lib/types";

import { Badge, EmptyState, ErrorBanner, NoticeBanner, SectionCard } from "@/components/ui";

const POLL_INTERVAL_MS = 5000;

const ACTION_LABEL: Record<"pause" | "resume" | "cancel" | "delete", string> = {
  pause: "暂停",
  resume: "恢复",
  cancel: "取消",
  delete: "删除",
};

type ActionKind = keyof typeof ACTION_LABEL;

interface CreateForm {
  name: string;
  goal: string;
  maxTokens: string;
  maxSteps: string;
}

const EMPTY_FORM: CreateForm = { name: "", goal: "", maxTokens: "", maxSteps: "" };

/** 空串 → 0（不限）；非法/负数 → null。 */
function parseOptionalCount(raw: string): number | null {
  const text = raw.trim();
  if (!text) return 0;
  if (!/^\d+$/.test(text)) return null;
  const value = Number(text);
  return Number.isSafeInteger(value) ? value : null;
}

function canPause(mission: Mission): boolean {
  return mission.status === "running";
}

function canResume(mission: Mission): boolean {
  return mission.status === "paused";
}

function canCancel(mission: Mission): boolean {
  return ["queued", "running", "paused", "finalizing"].includes(mission.status);
}

interface MissionsPanelProps {
  token: string;
  selectedId: number | null;
  onSelect: (id: number | null) => void;
  onAuthError: (message: string) => void;
}

export function MissionsPanel({ token, selectedId, onSelect, onAuthError }: MissionsPanelProps) {
  const [missions, setMissions] = useState<Mission[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [lastLoadedAt, setLastLoadedAt] = useState<string | null>(null);
  const [form, setForm] = useState<CreateForm>(EMPTY_FORM);
  const [createOpen, setCreateOpen] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [busy, setBusy] = useState<{ id: number; action: ActionKind } | null>(null);

  const load = useCallback(async () => {
    if (!token) {
      setMissions([]);
      setLoadError(null);
      return;
    }
    setLoading(true);
    try {
      const data = await listMissions(token);
      setMissions(data.missions ?? []);
      setLoadError(null);
      setLastLoadedAt(new Date().toLocaleTimeString("zh-CN", { hour12: false }));
    } catch (error) {
      if (isAuthError(error)) {
        onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
      }
      setLoadError(describeError(error));
    } finally {
      setLoading(false);
    }
  }, [token, onAuthError]);

  useEffect(() => {
    void load();
  }, [load]);

  useInterval(() => void load(), token ? POLL_INTERVAL_MS : null);

  const counts = useMemo(() => {
    const summary: Record<string, number> = {};
    for (const mission of missions) {
      summary[mission.status] = (summary[mission.status] ?? 0) + 1;
    }
    return summary;
  }, [missions]);

  function updateForm(patch: Partial<CreateForm>) {
    setForm((current) => ({ ...current, ...patch }));
    setFormError(null);
  }

  async function handleCreate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const name = form.name.trim();
    if (!name) {
      setFormError("任务名称不能为空。");
      return;
    }
    const maxTokens = parseOptionalCount(form.maxTokens);
    const maxSteps = parseOptionalCount(form.maxSteps);
    if (maxTokens === null || maxSteps === null) {
      setFormError("预算必须是大于等于 0 的整数（留空表示不限）。");
      return;
    }

    setSubmitting(true);
    setFormError(null);
    setNotice(null);
    setActionError(null);
    try {
      const mission = await createMission(token, {
        name,
        goal: form.goal,
        max_tokens: maxTokens,
        max_steps: maxSteps,
      });
      setNotice(`任务 #${mission.id} 已创建，已自动选中。`);
      setForm(EMPTY_FORM);
      setCreateOpen(false);
      await load();
      onSelect(mission.id);
    } catch (error) {
      if (isAuthError(error)) onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
      setFormError(describeError(error));
    } finally {
      setSubmitting(false);
    }
  }

  const runAction = useCallback(
    async (mission: Mission, action: ActionKind) => {
      if (action === "cancel" && !window.confirm(`确定取消任务 #${mission.id}？取消后不可恢复。`)) {
        return;
      }
      if (
        action === "delete" &&
        !window.confirm(`确定删除任务 #${mission.id}？该操作会级联删除任务数据且不可恢复。`)
      ) {
        return;
      }

      setBusy({ id: mission.id, action });
      setActionError(null);
      setNotice(null);
      try {
        if (action === "delete") {
          await deleteMission(token, mission.id);
          setNotice(`任务 #${mission.id} 已删除。`);
          if (selectedId === mission.id) onSelect(null);
        } else {
          const updated = await missionAction(token, mission.id, action);
          setNotice(`任务 #${updated.id} 已${ACTION_LABEL[action]}，当前状态：${missionStatusLabel(updated.status)}。`);
        }
        await load();
      } catch (error) {
        if (isAuthError(error)) onAuthError("管理令牌无效或已失效，请重新保存令牌后再试。");
        setActionError(`任务 #${mission.id} ${ACTION_LABEL[action]}失败：${describeError(error)}`);
      } finally {
        setBusy(null);
      }
    },
    [token, load, onAuthError, onSelect, selectedId],
  );

  const description = token
    ? `共 ${missions.length} 个任务 · 每 ${POLL_INTERVAL_MS / 1000} 秒自动刷新${
        lastLoadedAt ? ` · 上次刷新 ${lastLoadedAt}` : ""
      }`
    : "保存管理令牌后加载任务列表。";

  return (
    <SectionCard
      headingId="missions-heading"
      title="任务"
      description={description}
      actions={
        <>
          <button type="button" className="btn btn-sm" onClick={() => void load()} disabled={!token || loading}>
            {loading ? "刷新中…" : "刷新"}
          </button>
          <button
            type="button"
            className="btn btn-primary btn-sm"
            onClick={() => {
              setCreateOpen((open) => !open);
              setFormError(null);
            }}
            disabled={!token}
          >
            {createOpen ? "收起新建" : "新建任务"}
          </button>
        </>
      }
    >
      {!token ? (
        <EmptyState
          title="尚未保存管理令牌"
          hint="在页面顶部输入 BEACONX_ADMIN_TOKEN 并保存后，即可读取、创建与操作任务。"
        />
      ) : null}

      {loadError ? <ErrorBanner message={loadError} onDismiss={() => setLoadError(null)} /> : null}
      {actionError ? <ErrorBanner message={actionError} onDismiss={() => setActionError(null)} /> : null}
      {notice ? <NoticeBanner message={notice} /> : null}

      {token && createOpen ? (
        <form className="form-grid" onSubmit={handleCreate}>
          <div className="field">
            <label htmlFor="mission-name">任务名称（必填）</label>
            <input
              id="mission-name"
              className="input"
              value={form.name}
              onChange={(event) => updateForm({ name: event.target.value })}
              placeholder="例如：example.com 外网资产测试"
              maxLength={200}
            />
          </div>
          <div className="field">
            <label htmlFor="mission-max-tokens">Token 预算</label>
            <input
              id="mission-max-tokens"
              className="input"
              type="number"
              min={0}
              step={1}
              value={form.maxTokens}
              onChange={(event) => updateForm({ maxTokens: event.target.value })}
              placeholder="留空 = 不限"
            />
          </div>
          <div className="field span-2">
            <label htmlFor="mission-goal">目标说明</label>
            <textarea
              id="mission-goal"
              className="textarea"
              value={form.goal}
              onChange={(event) => updateForm({ goal: event.target.value })}
              placeholder="用自然语言描述测试目标与范围，例如：对 example.com 及其子域做被动信息收集，不执行破坏性操作。"
            />
          </div>
          <div className="field">
            <label htmlFor="mission-max-steps">步骤预算</label>
            <input
              id="mission-max-steps"
              className="input"
              type="number"
              min={0}
              step={1}
              value={form.maxSteps}
              onChange={(event) => updateForm({ maxSteps: event.target.value })}
              placeholder="留空 = 不限"
            />
          </div>
          <div className="form-actions span-2">
            <button type="submit" className="btn btn-primary" disabled={submitting}>
              {submitting ? "创建中…" : "创建任务"}
            </button>
            <button
              type="button"
              className="btn btn-ghost"
              onClick={() => {
                setCreateOpen(false);
                setFormError(null);
              }}
              disabled={submitting}
            >
              取消
            </button>
            <span className="hint">创建后任务进入排队，由后端调度器按并发上限准入。</span>
          </div>
          {formError ? (
            <div className="span-2">
              <ErrorBanner message={formError} onDismiss={() => setFormError(null)} />
            </div>
          ) : null}
        </form>
      ) : null}

      {token && missions.length > 0 ? (
        <div className="btn-row">
          {Object.entries(counts).map(([status, count]) => (
            <span key={status} className="small muted">
              {missionStatusLabel(status)} {formatNumber(count)}
            </span>
          ))}
        </div>
      ) : null}

      {token && missions.length === 0 && !loading && !loadError ? (
        <EmptyState title="暂无任务" hint="点击「新建任务」创建第一个任务；列表每 5 秒自动刷新。" />
      ) : null}

      {token && missions.length > 0 ? (
        <div className="table-wrap">
          <table className="data-table">
            <thead>
              <tr>
                <th scope="col">任务</th>
                <th scope="col">状态</th>
                <th scope="col">预算用量</th>
                <th scope="col">创建时间</th>
                <th scope="col">操作</th>
              </tr>
            </thead>
            <tbody>
              {missions.map((mission) => {
                const isSelected = selectedId === mission.id;
                const isBusy = busy?.id === mission.id;
                return (
                  <tr
                    key={mission.id}
                    className={`clickable${isSelected ? " selected" : ""}`}
                    onClick={() => onSelect(mission.id)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault();
                        onSelect(mission.id);
                      }
                    }}
                    tabIndex={0}
                    aria-selected={isSelected}
                  >
                    <td>
                      <div className="strong cell-truncate" title={mission.name}>
                        #{mission.id} {mission.name}
                      </div>
                      <div className="small muted cell-truncate" title={mission.goal || "（未填写目标说明）"}>
                        {mission.goal || "（未填写目标说明）"}
                      </div>
                    </td>
                    <td>
                      <Badge tone={missionStatusTone(mission.status)}>{missionStatusLabel(mission.status)}</Badge>
                    </td>
                    <td className="small nowrap">{missionBudgetText(mission)}</td>
                    <td className="small nowrap">{formatTime(mission.created_at)}</td>
                    <td>
                      <div className="btn-row" onClick={(event) => event.stopPropagation()}>
                        <button
                          type="button"
                          className="btn btn-sm"
                          onClick={() => onSelect(mission.id)}
                          aria-label={`查看任务 ${mission.id} 详情`}
                        >
                          查看
                        </button>
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={!canPause(mission) || isBusy}
                          onClick={() => void runAction(mission, "pause")}
                          title={canPause(mission) ? "暂停运行中的任务" : "仅运行中的任务可暂停"}
                        >
                          {isBusy && busy?.action === "pause" ? "暂停中…" : "暂停"}
                        </button>
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={!canResume(mission) || isBusy}
                          onClick={() => void runAction(mission, "resume")}
                          title={canResume(mission) ? "把暂停的任务重新排队" : "仅已暂停的任务可恢复"}
                        >
                          {isBusy && busy?.action === "resume" ? "恢复中…" : "恢复"}
                        </button>
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={!canCancel(mission) || isBusy}
                          onClick={() => void runAction(mission, "cancel")}
                          title={canCancel(mission) ? "取消任务" : "终态任务无需取消"}
                        >
                          {isBusy && busy?.action === "cancel" ? "取消中…" : "取消"}
                        </button>
                        <button
                          type="button"
                          className="btn btn-danger btn-sm"
                          disabled={isBusy}
                          onClick={() => void runAction(mission, "delete")}
                          title="级联删除任务数据，不可恢复"
                        >
                          {isBusy && busy?.action === "delete" ? "删除中…" : "删除"}
                        </button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      {token ? <p className="hint">列表最多展示最近 100 个任务；操作失败的原因来自后端错误响应（如 409 状态冲突）。</p> : null}
    </SectionCard>
  );
}

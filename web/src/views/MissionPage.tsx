"use client";

import { useEffect, useRef, useState } from "react";

import {
  addMissionAsset,
  addMissionScope,
  clearMissionTraffic,
  compileScope,
  createFinding,
  createFindingFromNode,
  createRetest,
  deleteMission,
  deleteMissionScope,
  downloadFile,
  freezeEvidence,
  getCoverage,
  getGraph,
  getMission,
  getNode,
  getPlan,
  listMissionAssets,
  listMissionFindings,
  listMissionScope,
  listEvidenceAudit,
  listMissionTraffic,
  listRetestEvidence,
  listRetests,
  missionAction,
  missionAudit,
  missionReport,
  missionUsage,
  patchNodeState,
  patchRetest,
  removeMissionAsset,
  runRetest,
  sseUrl,
  trafficBody,
  transitionFinding,
  verifyEvidence,
} from "@/lib/api";
import {
  allowedNodeStates,
  budgetText,
  FINDING_NEXT,
  formatBytes,
  formatClock,
  formatTime,
  label,
  missionActions,
  missionStatusTone,
  MISSION_SCOPE_KINDS,
  NODE_KINDS,
  nodeStateText,
  pretty,
  REFRESH_EVENT_TYPES,
  RETEST_NEXT,
  SEVERITIES,
  shortHash,
  stepStatusText,
  STREAM_EVENT_TYPES,
} from "@/lib/format";
import { Link, MISSION_TABS, paths, useRouter } from "@/lib/router";
import { useApi, useBusy, useTail } from "@/lib/use-api";
import type { GraphView, Mission, OperationNode, StreamEvent, TrafficExchange } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { ApprovalsPage } from "@/views/ApprovalsPage";
import { Badge, BudgetMeter, ErrorBanner, Field, JsonBlock, LoadMore, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

function normalizeEvent(fallback: string, message: MessageEvent<string>, seen: Set<string>): StreamEvent | null {
  let raw: unknown = message.data;
  try { raw = JSON.parse(message.data); } catch { raw = message.data; }
  let type = fallback;
  let detail: unknown = raw;
  let createdAt: string | undefined;
  let id: number | undefined;
  if (message.lastEventId) {
    const parsed = Number(message.lastEventId);
    if (Number.isSafeInteger(parsed) && parsed > 0) id = parsed;
  }
  if (raw && typeof raw === "object") {
    const record = raw as Record<string, unknown>;
    if (typeof record.type === "string" && record.type) type = record.type;
    if (typeof record.created_at === "string") createdAt = record.created_at;
    if (id === undefined && typeof record.id === "number") id = record.id;
    if (typeof record.mission_id === "number") detail = "payload" in record ? record.payload ?? null : null;
  }
  const key = id !== undefined ? `id-${id}` : `${type}-${message.timeStamp}`;
  if (id !== undefined) {
    if (seen.has(key)) return null;
    seen.add(key);
  }
  return { key, id, type, createdAt, receivedAt: new Date().toISOString(), detail };
}

export function MissionPage({ token, onAuthError, id, tab }: PageProps & { id: number; tab: string }) {
  const { navigate } = useRouter();
  const mission = useApi({ token, interval: 5000, watch: [id], onAuthError, load: (current, signal) => getMission(current, id, signal) });
  const busy = useBusy(onAuthError);
  const data = mission.data;
  const actions = missionActions(data?.status ?? "");

  return (
    <>
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} onDismiss={() => busy.setError(null)} />
      <NoticeBanner message={busy.notice ?? ""} />
      <QueryBody token={token} loading={mission.loading} error={mission.error} empty={!data} emptyTitle="任务不存在或正在读取">
        {data ? (
          <>
            <PageHeader
              kicker={`任务 #${data.id} · ${formatTime(data.created_at)}`}
              title={data.name}
              description={<>{data.goal || "没有填写目标。"} {data.reason ? `原因：${data.reason}` : ""}</>}
              actions={
                <div className="row-actions">
                  <Badge tone={missionStatusTone(data.status)}><i />{label(data.status)}</Badge>
                  <button type="button" className="btn btn-ghost btn-sm" disabled={!actions.pause || Boolean(busy.busy)} onClick={() => void busy.run("pause", async () => { await missionAction(token, id, "pause"); mission.reload(); })}>暂停</button>
                  <button type="button" className="btn btn-ghost btn-sm" disabled={!actions.resume || Boolean(busy.busy)} onClick={() => void busy.run("resume", async () => { await missionAction(token, id, "resume"); mission.reload(); })}>恢复</button>
                  <button type="button" className="btn btn-ghost btn-sm" disabled={!actions.cancel || Boolean(busy.busy)} onClick={() => void busy.run("cancel", async () => { await missionAction(token, id, "cancel"); mission.reload(); })}>取消</button>
                  <button type="button" className="btn btn-danger btn-sm" onClick={() => { if (!window.confirm(`删除任务「${data.name}」？`)) return; void busy.run("delete", async () => { await deleteMission(token, id); navigate(paths.missions); }); }}>删除</button>
                </div>
              }
            />
            <div className="grid-2" style={{ marginBottom: 12 }}>
              <Panel><BudgetMeter used={data.spent_tokens} max={data.max_tokens} unit="Token" /></Panel>
              <Panel><BudgetMeter used={data.steps_used} max={data.max_steps} unit="步骤" /></Panel>
            </div>
            <div className="tabs">
              {MISSION_TABS.map(([key, name]) => (
                <Link key={key} href={paths.mission(id, key)} className={tab === key ? "on" : undefined} current={tab === key}>{name}</Link>
              ))}
            </div>
            {tab === "overview" ? <Overview token={token} mission={data} onAuthError={onAuthError} /> : null}
            {tab === "events" ? <Events token={token} missionId={id} onAuthError={onAuthError} onRefresh={mission.reload} /> : null}
            {tab === "plan" ? <PlanTab token={token} missionId={id} onAuthError={onAuthError} /> : null}
            {tab === "graph" ? <GraphTab token={token} missionId={id} onAuthError={onAuthError} /> : null}
            {tab === "assets" ? <AssetsTab token={token} missionId={id} onAuthError={onAuthError} /> : null}
            {tab === "coverage" ? <CoverageTab token={token} missionId={id} onAuthError={onAuthError} /> : null}
            {tab === "findings" ? <FindingsTab token={token} missionId={id} onAuthError={onAuthError} /> : null}
            {tab === "retests" ? <RetestsTab token={token} missionId={id} onAuthError={onAuthError} /> : null}
            {tab === "approvals" ? <ApprovalsPage token={token} onAuthError={onAuthError} missionId={id} /> : null}
            {tab === "traffic" ? <TrafficTab token={token} missionId={id} onAuthError={onAuthError} /> : null}
            {tab === "report" ? <ReportTab token={token} missionId={id} onAuthError={onAuthError} /> : null}
          </>
        ) : null}
      </QueryBody>
    </>
  );
}

function Overview({ token, mission, onAuthError }: { token: string; mission: Mission; onAuthError: (m: string) => void }) {
  const scope = useApi({ token, interval: 10000, watch: [mission.id], onAuthError, load: (current, signal) => listMissionScope(current, mission.id, signal) });
  const usage = useApi({ token, interval: 10000, watch: [mission.id], onAuthError, load: (current, signal) => missionUsage(current, mission.id, signal) });
  const audit = useApi({ token, interval: 10000, watch: [mission.id], onAuthError, load: (current, signal) => missionAudit(current, mission.id, signal) });
  const busy = useBusy(onAuthError);
  const [compiled, setCompiled] = useState<unknown>(null);
  return (
    <div className="stack">
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="范围" extra={<button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run("compile", async () => { setCompiled(await compileScope(token, mission.id)); return "已生成预览，未写入规则"; })}>编译预览</button>}>
          <div className="chips">{(scope.data?.scope ?? []).map((item) => <span key={item.id} className="chip">{label(item.kind)} · {item.value} <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`s${item.id}`, async () => { await deleteMissionScope(token, mission.id, item.id); scope.reload(); })}>移除</button></span>)}</div>
          {(scope.data?.scope ?? []).length === 0 ? <p className="muted">还没有范围。</p> : null}
          <form className="form-grid" style={{ marginTop: 12 }} onSubmit={(event) => { event.preventDefault(); const form = event.currentTarget; const data = new FormData(form); void busy.run("scope", async () => { await addMissionScope(token, mission.id, { kind: String(data.get("kind")), value: String(data.get("value")), note: String(data.get("note") || "") }); form.reset(); scope.reload(); }); }}>
            <Field label="类型"><select name="kind">{MISSION_SCOPE_KINDS.map((kind) => <option key={kind} value={kind}>{label(kind)}</option>)}</select></Field>
            <Field label="值"><input name="value" required /></Field>
            <button type="submit" className="btn btn-primary btn-sm">添加</button>
          </form>
          {compiled ? <JsonBlock value={compiled} /> : null}
          {scope.error ? <p className="muted">{scope.error}</p> : null}
        </Panel>
        <Panel title="用量">
          <p className="muted">token {budgetText(usage.data?.spent_tokens ?? mission.spent_tokens, usage.data?.max_tokens ?? mission.max_tokens)} · 步 {budgetText(usage.data?.steps_used ?? mission.steps_used, usage.data?.max_steps ?? mission.max_steps)}</p>
          {(usage.data?.by_role ?? []).map((row) => <p key={row.role}>{row.role || "未标角色"} · {row.calls} 次 · {row.total_tokens} token</p>)}
          {(usage.data?.by_model ?? []).map((row) => <p key={row.model} className="muted">{row.model} · {row.calls} 次 · {row.total_tokens}</p>)}
          {usage.error ? <p className="muted">{usage.error}</p> : null}
        </Panel>
      </div>
      <Panel title="最近审计">
        {(audit.data?.audit ?? []).slice(0, 8).map((item) => (
          <div className="event" key={item.id}><time>{formatClock(item.created_at)}</time><Badge tone={missionStatusTone(item.outcome)}>{label(item.outcome)}</Badge><span>{item.tool} · {label(item.action)} · {item.rule_name || "无规则名"}</span></div>
        ))}
        {(audit.data?.audit ?? []).length === 0 ? <p className="muted">没有审计记录。</p> : null}
      </Panel>
    </div>
  );
}

function Events({ token, missionId, onRefresh }: { token: string; missionId: number; onAuthError: (m: string) => void; onRefresh: () => void }) {
  const [events, setEvents] = useState<StreamEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const [generation, setGeneration] = useState(0);
  const seen = useRef(new Set<string>());
  useEffect(() => { seen.current = new Set(); setEvents([]); }, [missionId]);
  useEffect(() => {
    if (!token) return;
    let disposed = false;
    const source = new EventSource(sseUrl(`/api/v1/missions/${missionId}/events?since=0`, token));
    source.onopen = () => { if (!disposed) setConnected(true); };
    source.onerror = () => { if (!disposed) setConnected(false); };
    const ingest = (message: Event, fallback: string) => {
      if (disposed) return;
      const event = normalizeEvent(fallback, message as MessageEvent<string>, seen.current);
      if (!event) return;
      setEvents((current) => [event, ...current].slice(0, 200));
      if (REFRESH_EVENT_TYPES.has(event.type)) onRefresh();
    };
    const bound: Array<[string, EventListener]> = [];
    for (const type of STREAM_EVENT_TYPES) {
      const listener: EventListener = (raw) => ingest(raw, type);
      source.addEventListener(type, listener);
      bound.push([type, listener]);
    }
    const messageListener: EventListener = (raw) => ingest(raw, "message");
    source.addEventListener("message", messageListener);
    return () => { disposed = true; source.removeEventListener("message", messageListener); for (const [type, listener] of bound) source.removeEventListener(type, listener); source.close(); };
  }, [token, missionId, generation, onRefresh]);
  return (
    <Panel title="事件流" extra={<><span className="muted">{connected ? "已连接" : "重连中"}</span><button type="button" className="btn btn-ghost btn-sm" onClick={() => { seen.current = new Set(); setEvents([]); setGeneration((value) => value + 1); }}>重新连接</button></>}>
      {events.length === 0 ? <p className="muted">还没有事件。连接会按 Last-Event-ID 补洞，页面只留最近 200 条。</p> : null}
      {events.map((event) => (
        <div className="event" key={event.key}><time>{formatClock(event.createdAt || event.receivedAt)}</time><Badge tone={missionStatusTone(event.type)}>{label(event.type)}</Badge><span>{pretty(event.detail) || "—"}</span></div>
      ))}
    </Panel>
  );
}

function PlanTab({ token, missionId, onAuthError }: PageProps & { missionId: number }) {
  const plan = useApi({ token, interval: 10000, watch: [missionId], onAuthError, load: (current, signal) => getPlan(current, missionId, signal) });
  const steps = plan.data?.steps ?? [];
  return (
    <Panel title="计划" extra={<span className="muted">只读，由规划器写入</span>}>
      <QueryBody token={token} loading={plan.loading} error={plan.error} empty={steps.length === 0} emptyTitle="还没有计划步骤">
        <table>
          <thead><tr><th>步骤</th><th>状态</th><th>依赖</th></tr></thead>
          <tbody>{steps.map((step) => <tr key={step.id}><td><strong>{step.title || step.key}</strong><div className="muted">{step.detail}</div></td><td><Badge tone={missionStatusTone(step.status)}>{stepStatusText(step.status)}</Badge></td><td>{(step.depends_on ?? []).join(", ") || "—"}</td></tr>)}</tbody>
        </table>
      </QueryBody>
    </Panel>
  );
}

function GraphTab({ token, missionId, onAuthError }: PageProps & { missionId: number }) {
  const [kind, setKind] = useState("");
  const [state, setState] = useState("");
  const graph = useApi({ token, interval: 10000, watch: [missionId, kind, state], onAuthError, load: (current, signal) => getGraph(current, missionId, { kind: kind || undefined, state: state || undefined, limit: 500 }, signal) });
  const tail = useTail<OperationNode>(`${missionId}|${kind}|${state}`);
  const busy = useBusy(onAuthError);
  const [nodeDetail, setNodeDetail] = useState<OperationNode | null>(null);
  const view: GraphView = graph.data ?? { nodes: [], edges: [], anchors: [] };
  const nodes = [...view.nodes, ...tail.rows];
  const nextId = tail.rows.length ? tail.next : (view.next_after_id ?? 0);
  return (
    <div className="stack">
      <div className="filters">
        <select value={kind} onChange={(event) => setKind(event.target.value)}><option value="">全部类型</option>{NODE_KINDS.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select>
        <select value={state} onChange={(event) => setState(event.target.value)}><option value="">全部状态</option>{["open", "running", "done", "exhausted", "blocked"].map((item) => <option key={item} value={item}>{nodeStateText(item)}</option>)}</select>
      </div>
      <ErrorBanner message={graph.error || busy.error || ""} />
      <Panel title="节点">
        <div className="graph">
          {NODE_KINDS.filter((item) => !kind || item === kind).map((item) => (
            <div key={item}>
              <h3>{label(item)}</h3>
              {nodes.filter((node) => node.kind === item).map((node) => (
                <div key={node.id} className="node">
                  <strong>{node.title || `#${node.id}`}</strong>
                  <span className="muted">{nodeStateText(node.state)}</span>
                  <div className="row-actions" style={{ marginTop: 6 }}>
                    <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`g${node.id}`, async () => { setNodeDetail(await getNode(token, node.id)); })}>读取</button>
                    {allowedNodeStates(node.kind).filter((next) => next !== node.state).map((next) => (
                      <button key={next} type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`n${node.id}`, async () => { await patchNodeState(token, node.id, next); graph.reload(); })}>{nodeStateText(next)}</button>
                    ))}
                  </div>
                </div>
              ))}
            </div>
          ))}
        </div>
        <LoadMore nextId={nextId} busy={Boolean(busy.busy)} onLoad={() => void busy.run("more", async () => { const page = await getGraph(token, missionId, { kind: kind || undefined, state: state || undefined, limit: 500, after_id: nextId }); tail.push(page.nodes ?? [], page.next_after_id); })} />
        {nodeDetail ? <JsonBlock value={nodeDetail} /> : null}
      </Panel>
      <Panel title="边与锚点">
        {(view.edges ?? []).slice(0, 40).map((edge) => <p key={edge.id}>#{edge.src_id} {label(edge.rel)} #{edge.dst_id}</p>)}
        {(view.anchors ?? []).slice(0, 40).map((anchor) => <p key={`${anchor.node_id}-${anchor.asset_id}-${anchor.role}`} className="muted">节点 #{anchor.node_id} · 资产 #{anchor.asset_id} · {label(anchor.role)}</p>)}
        {(view.edges ?? []).length === 0 && (view.anchors ?? []).length === 0 ? <p className="muted">没有边或锚点。</p> : null}
      </Panel>
    </div>
  );
}

function AssetsTab({ token, missionId, onAuthError }: PageProps & { missionId: number }) {
  const assets = useApi({ token, interval: 10000, watch: [missionId], onAuthError, load: (current, signal) => listMissionAssets(current, missionId, signal) });
  const busy = useBusy(onAuthError);
  return (
    <Panel title="任务资产">
      <ErrorBanner message={busy.error ?? ""} />
      <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("add", async () => { const result = await addMissionAsset(token, missionId, { asset_id: Number(data.get("asset_id")), source: String(data.get("source") || "manual") }); assets.reload(); return `已登记资产 #${result.asset_id}`; }); }}>
        <input name="asset_id" type="number" min={1} required placeholder="资产 ID" />
        <select name="source"><option value="manual">manual</option><option value="scope">scope</option><option value="discovered">discovered</option></select>
        <button type="submit" className="btn btn-primary btn-sm">登记</button>
      </form>
      <QueryBody token={token} loading={assets.loading} error={assets.error} empty={(assets.data?.assets ?? []).length === 0} emptyTitle="还没有任务资产">
        <table>
          <thead><tr><th>资产</th><th>类型</th><th /></tr></thead>
          <tbody>{(assets.data?.assets ?? []).map((asset) => <tr key={asset.id}><td>{asset.label} <span className="muted">#{asset.id}</span></td><td>{label(asset.type)}</td><td><button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`rm${asset.id}`, async () => { await removeMissionAsset(token, missionId, asset.id); assets.reload(); })}>移除</button></td></tr>)}</tbody>
        </table>
      </QueryBody>
    </Panel>
  );
}

function CoverageTab({ token, missionId, onAuthError }: PageProps & { missionId: number }) {
  const coverage = useApi({ token, interval: 10000, watch: [missionId], onAuthError, load: (current, signal) => getCoverage(current, missionId, signal) });
  const data = coverage.data;
  return (
    <Panel title="覆盖度" extra={<span className="muted">分母是任务资产和 target 锚点，分子是 fact 上的 tested 锚点</span>}>
      <QueryBody token={token} loading={coverage.loading} error={coverage.error} empty={!data}>
        {data ? (
          <>
            <p>{data.tested} / {data.total} · {data.percent.toFixed(1)}%</p>
            <ul className="dist">{(data.by_type ?? []).map((row) => <li key={row.type}><span>{label(row.type)}</span><span className="track"><i className="fill" style={{ width: `${row.percent}%` }} /></span><span className="num">{row.tested}/{row.total}</span></li>)}</ul>
            {(data.by_type ?? []).length === 0 ? <p className="muted">按类型的覆盖为空。</p> : null}
          </>
        ) : null}
      </QueryBody>
    </Panel>
  );
}

function FindingsTab({ token, missionId, onAuthError }: PageProps & { missionId: number }) {
  const list = useApi({ token, interval: 5000, watch: [missionId], onAuthError, load: (current, signal) => listMissionFindings(current, missionId, { limit: 100 }, signal) });
  const busy = useBusy(onAuthError);
  return (
    <div className="stack">
      <ErrorBanner message={busy.error ?? ""} />
      <Panel title="新建">
        <form onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("new", async () => { const created = await createFinding(token, missionId, { title: String(data.get("title")), severity: String(data.get("severity")), detail: String(data.get("detail") || ""), asset_id: Number(data.get("asset_id")) || undefined, node_id: Number(data.get("node_id")) || undefined }); list.reload(); return `已创建发现 #${created.id}`; }); }}>
          <Field label="标题"><input name="title" required /></Field>
          <div className="form-grid">
            <Field label="严重度"><select name="severity">{SEVERITIES.map((item) => <option key={item} value={item}>{label(item)}</option>)}</select></Field>
            <Field label="说明"><input name="detail" /></Field>
          </div>
          <button type="submit" className="btn btn-primary btn-sm">创建</button>
        </form>
        <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("node", async () => { await createFindingFromNode(token, missionId, { node_id: Number(data.get("node_id")), title: String(data.get("title") || "") }); list.reload(); return "已从节点创建"; }); }}>
          <input name="node_id" type="number" min={1} required placeholder="节点 ID" />
          <input name="title" placeholder="标题，可空" />
          <button type="submit" className="btn btn-ghost btn-sm">从节点创建</button>
        </form>
      </Panel>
      <Panel title="发现">
        <QueryBody token={token} loading={list.loading} error={list.error} empty={(list.data?.findings ?? []).length === 0} emptyTitle="还没有发现">
          <table>
            <thead><tr><th>标题</th><th>严重度</th><th>状态</th><th /></tr></thead>
            <tbody>
              {(list.data?.findings ?? []).map((finding) => (
                <tr key={finding.id}>
                  <td><Link href={paths.finding(finding.id)}>{finding.title}</Link><div className="muted">证据 {finding.evidence_count}</div></td>
                  <td><Badge tone={missionStatusTone(finding.severity)}>{label(finding.severity)}</Badge></td>
                  <td><Badge>{label(finding.status)}</Badge></td>
                  <td className="row-actions">{(FINDING_NEXT[finding.status] ?? []).map((next) => <button key={next} type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`f${finding.id}${next}`, async () => { await transitionFinding(token, finding.id, next); list.reload(); })}>{label(next)}</button>)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </QueryBody>
      </Panel>
    </div>
  );
}

function RetestsTab({ token, missionId, onAuthError }: PageProps & { missionId: number }) {
  const list = useApi({ token, interval: 5000, watch: [missionId], onAuthError, load: (current, signal) => listRetests(current, missionId, signal) });
  const busy = useBusy(onAuthError);
  return (
    <Panel title="复测">
      <ErrorBanner message={busy.error ?? ""} />
      <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("new", async () => { await createRetest(token, missionId, { finding_id: Number(data.get("finding_id")), note: String(data.get("note") || "") }); list.reload(); return "已发起复测"; }); }}>
        <input name="finding_id" type="number" min={1} required placeholder="发现 ID" />
        <input name="note" placeholder="备注" />
        <button type="submit" className="btn btn-primary btn-sm">发起</button>
      </form>
      <QueryBody token={token} loading={list.loading} error={list.error} empty={(list.data?.retests ?? []).length === 0} emptyTitle="还没有复测">
        {(list.data?.retests ?? []).map((retest) => (
          <div className="event" key={retest.id} style={{ gridTemplateColumns: "1fr auto" }}>
            <div><strong>#{retest.id}</strong> 发现 #{retest.finding_id} <Badge tone={missionStatusTone(retest.status)}>{label(retest.status)}</Badge><div className="muted">{retest.note || "无备注"} · {formatTime(retest.created_at)}</div></div>
            <div className="row-actions">
              <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`run${retest.id}`, async () => { const next = await runRetest(token, retest.id); list.reload(); return `执行结果：${label(next.status)}`; })}>执行校验</button>
              <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`ev${retest.id}`, async () => { const result = await listRetestEvidence(token, retest.id); const ids = (result.evidence ?? []).map((item) => item.snapshot_id).join(", "); return ids ? `绑定快照 ${ids}` : "没有绑定证据"; })}>查看证据</button>
              {(RETEST_NEXT[retest.status] ?? []).map((next) => <button key={next} type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`st${retest.id}${next}`, async () => { await patchRetest(token, retest.id, { status: next }); list.reload(); })}>{label(next)}</button>)}
            </div>
          </div>
        ))}
      </QueryBody>
    </Panel>
  );
}

function TrafficTab({ token, missionId, onAuthError }: PageProps & { missionId: number }) {
  const list = useApi({ token, interval: 5000, watch: [missionId], onAuthError, load: (current, signal) => listMissionTraffic(current, missionId, signal) });
  const busy = useBusy(onAuthError);
  const [body, setBody] = useState("");
  async function show(item: TrafficExchange, part: "request" | "response") {
    await busy.run(`body${item.id}${part}`, async () => { setBody(await trafficBody(token, item.id, part)); return ""; });
  }
  return (
    <div className="stack">
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <Panel title="采集" extra={<button type="button" className="btn btn-danger btn-sm" onClick={() => { if (!window.confirm("清空本任务采集行？已冻结的证据仍保留。")) return; void busy.run("clear", async () => { await clearMissionTraffic(token, missionId); list.reload(); return "已清空采集"; }); }}>清空采集</button>}>
        <QueryBody token={token} loading={list.loading} error={list.error} empty={(list.data?.traffic ?? []).length === 0} emptyTitle="还没有流量">
          <table>
            <thead><tr><th>工具</th><th>阶段</th><th>大小</th><th /></tr></thead>
            <tbody>
              {(list.data?.traffic ?? []).map((item) => (
                <tr key={item.id}>
                  <td>{item.tool}<div className="muted">#{item.id} {item.error}</div></td>
                  <td><Badge tone={missionStatusTone(item.phase)}>{label(item.phase)}</Badge></td>
                  <td className="num">{formatBytes(item.request_size)} / {formatBytes(item.response_size)}</td>
                  <td className="row-actions">
                    <button type="button" className="btn btn-ghost btn-sm" onClick={() => void show(item, "request")}>请求</button>
                    <button type="button" className="btn btn-ghost btn-sm" onClick={() => void show(item, "response")}>响应</button>
                    <button type="button" className="btn btn-ghost btn-sm" disabled={item.phase !== "result"} onClick={() => void busy.run(`fr${item.id}`, async () => { const snap = await freezeEvidence(token, item.id); return `已冻结为证据 #${snap.id}（${shortHash(snap.signature)}）`; })}>冻结</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </QueryBody>
      </Panel>
      {body ? <Panel title="正文"><pre className="json">{body}</pre></Panel> : null}
      <form className="filters" onSubmit={(event) => { event.preventDefault(); const id = Number(new FormData(event.currentTarget).get("id")); const action = (event.nativeEvent as SubmitEvent).submitter?.getAttribute("value"); void busy.run(action || "verify", async () => { if (action === "audit") { const result = await listEvidenceAudit(token, id); return `审计 ${result.audit.length} 条`; } if (action === "export") { await downloadFile(`/api/v1/evidence/${id}/export`, token, `evidence-${id}.zip`); return "已开始下载"; } const result = await verifyEvidence(token, id); return result.verified ? `证据 #${result.evidence_id} 校验通过` : "校验未通过"; }); }}>
        <input name="id" type="number" min={1} required placeholder="证据 ID" />
        <button type="submit" className="btn btn-ghost btn-sm" value="verify">校验证据</button>
        <button type="submit" className="btn btn-ghost btn-sm" value="audit">审计</button>
        <button type="submit" className="btn btn-ghost btn-sm" value="export">导出</button>
      </form>
    </div>
  );
}

function ReportTab({ token, missionId, onAuthError }: PageProps & { missionId: number }) {
  const report = useApi({ token, watch: [missionId], onAuthError, load: (current) => missionReport(current, missionId, "markdown") });
  const busy = useBusy(onAuthError);
  return (
    <Panel title="报告" extra={<div className="row-actions">{["markdown", "html", "docx", "pdf"].map((format) => <button key={format} type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(format, async () => { await downloadFile(`/api/v1/missions/${missionId}/report?format=${format}`, token, `mission-${missionId}.${format === "markdown" ? "md" : format}`); })}>{format}</button>)}</div>}>
      <ErrorBanner message={busy.error || report.error || ""} />
      {report.loading ? <p className="loading-text">正在生成…</p> : <pre className="json">{report.data || "没有报告正文。"}</pre>}
    </Panel>
  );
}

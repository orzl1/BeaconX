"use client";

import { getDashboard, listActivity } from "@/lib/api";
import { formatClock, formatNumber, formatTime, label, missionStatusTone, MISSION_STATUSES, SEVERITIES } from "@/lib/format";
import { paths } from "@/lib/router";
import { Link } from "@/lib/router";
import { useApi, useBusy, useTail } from "@/lib/use-api";
import type { ActivityItem } from "@/lib/types";
import type { PageProps } from "@/components/Console";
import { Badge, ErrorBanner, LoadMore, Metric, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function DashboardPage({ token, onAuthError }: PageProps) {
  const stats = useApi({ token, interval: 15000, onAuthError, load: (current, signal) => getDashboard(current, signal) });
  const activity = useApi({ token, interval: 15000, onAuthError, load: (current, signal) => listActivity(current, { limit: 12 }, signal) });
  const tail = useTail<ActivityItem>("activity");
  const busy = useBusy(onAuthError);
  const activityRows = [...(activity.data?.activity ?? []), ...tail.rows];
  const activityNext = tail.rows.length ? tail.next : (activity.data?.next_before_id ?? 0);
  const data = stats.data;
  const maxStatus = Math.max(1, ...MISSION_STATUSES.map((key) => data?.missions.by_status?.[key] ?? 0));
  const maxSeverity = Math.max(1, ...SEVERITIES.map((key) => data?.findings.by_severity?.[key] ?? 0));
  const hours = data?.hourly ?? [];
  const maxTokens = Math.max(1, ...hours.map((hour) => hour.tokens));

  return (
    <>
      <PageHeader kicker="今日态势" title="概览" description="任务、预算、待审批和最近 24 小时用量。进入页面拉一次，之后约 15 秒刷新。累计 token 是任务账本，今日 token 是当天调用记录，两者口径不同。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <QueryBody token={token} loading={stats.loading} error={stats.error} empty={!data}>
        {data ? (
          <>
            <div className="metrics">
              <Metric label="任务" value={formatNumber(data.missions.total)} hint={`运行中 ${formatNumber(data.missions.by_status?.running)}`} />
              <Metric label="待审批" value={formatNumber(data.approvals.pending)} hint="策略询问" />
              <Metric label="发现" value={formatNumber(data.findings.total)} hint="按严重度见下" />
              <Metric label="资产" value={formatNumber(data.assets.total)} />
              <Metric label="累计 token" value={formatNumber(data.tokens.spent_total)} hint="任务账本" />
              <Metric label="今日 token" value={formatNumber(data.tokens.today)} hint={`${formatNumber(data.tokens.calls)} 次调用`} />
            </div>
            <div className="grid-2">
              <Panel title="任务状态" extra={<span className="muted">固定 7 个状态</span>}>
                <ul className="dist">
                  {MISSION_STATUSES.map((status) => {
                    const value = data.missions.by_status?.[status] ?? 0;
                    return (
                      <li key={status}>
                        <span>{label(status)}</span>
                        <span className="track"><i className={`fill tone-${missionStatusTone(status)}`} style={{ width: `${(value / maxStatus) * 100}%` }} /></span>
                        <span className="num">{formatNumber(value)}</span>
                      </li>
                    );
                  })}
                </ul>
              </Panel>
              <Panel title="最近 24 小时" extra={<span className="muted">token · UTC 整点</span>}>
                {hours.length === 0 ? <p className="muted">接口没有返回小时桶。</p> : (
                  <div className="bars" aria-label="最近 24 小时 token">
                    {hours.map((hour) => <i key={hour.hour} title={`${hour.hour} · ${hour.tokens}`} style={{ height: `${(hour.tokens / maxTokens) * 100}%` }} />)}
                  </div>
                )}
              </Panel>
            </div>
            <div className="grid-3">
              <Panel title="最近任务">
                <table>
                  <thead><tr><th>名称</th><th>状态</th><th>token</th></tr></thead>
                  <tbody>
                    {(data.recent_missions ?? []).map((mission) => (
                      <tr key={mission.id} className="link" onClick={() => undefined}>
                        <td><Link href={paths.mission(mission.id)}>{mission.name || `任务 #${mission.id}`}</Link></td>
                        <td><Badge tone={missionStatusTone(mission.status)}><i />{label(mission.status)}</Badge></td>
                        <td className="num">{formatNumber(mission.spent_tokens)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </Panel>
              <Panel title="发现严重度">
                <ul className="dist">
                  {SEVERITIES.map((severity) => {
                    const value = data.findings.by_severity?.[severity] ?? 0;
                    return (
                      <li key={severity}>
                        <span>{label(severity)}</span>
                        <span className="track"><i className={`fill tone-${missionStatusTone(severity)}`} style={{ width: `${(value / maxSeverity) * 100}%` }} /></span>
                        <span className="num">{formatNumber(value)}</span>
                      </li>
                    );
                  })}
                </ul>
              </Panel>
              <Panel title="活动" extra={activity.error ? <span className="muted">{activity.error}</span> : null}>
                {activityRows.map((item) => (
                  <div className="event" key={item.id}>
                    <time>{formatClock(item.created_at)}</time>
                    <Badge tone={missionStatusTone(item.type)}>{label(item.type)}</Badge>
                    <Link href={paths.mission(item.mission_id)}>任务 #{item.mission_id}</Link>
                  </div>
                ))}
                {activityRows.length === 0 ? <p className="muted">还没有活动。</p> : null}
                <LoadMore nextId={activityNext} busy={Boolean(busy.busy)} onLoad={() => void busy.run("more", async () => { const page = await listActivity(token, { limit: 12, before_id: activityNext }); tail.push(page.activity ?? [], page.next_before_id); })} />
                <p className="muted">时间 {formatTime(hours[hours.length - 1]?.hour)}</p>
              </Panel>
            </div>
          </>
        ) : null}
      </QueryBody>
    </>
  );
}

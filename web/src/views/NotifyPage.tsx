"use client";

import { createChannel, deleteChannel, listChannels, listDeliveries, testChannel, updateChannel } from "@/lib/api";
import { formatTime, label, parseObject, pretty } from "@/lib/format";
import { useApi, useBusy } from "@/lib/use-api";
import type { PageProps } from "@/components/Console";
import { ErrorBanner, Field, NoticeBanner, PageHeader, Panel, QueryBody, TokenNotice } from "@/components/ui";

export function NotifyPage({ token, onAuthError }: PageProps) {
  const channels = useApi({ token, onAuthError, load: (current, signal) => listChannels(current, signal) });
  const deliveries = useApi({ token, interval: 10000, onAuthError, load: (current, signal) => listDeliveries(current, signal) });
  const busy = useBusy(onAuthError);
  return (
    <>
      <PageHeader kicker="出站" title="通知" description="测试一律返回 200 和投递记录。webhook 会真实发出；email 与 generic 记为 skipped。敏感配置在响应里是掩码。推送是至少一次尝试，停机不补。" />
      <TokenNotice token={token} />
      <ErrorBanner message={busy.error ?? ""} />
      <NoticeBanner message={busy.notice ?? ""} />
      <div className="grid-2">
        <Panel title="渠道">
          <form onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run("new", async () => { let config: Record<string, unknown>; try { config = parseObject(String(data.get("config") || "{}")); } catch { throw new Error("配置必须是 JSON 对象"); } await createChannel(token, { name: String(data.get("name")), type: String(data.get("type")), config, enabled: true }); channels.reload(); return "已创建"; }); }}>
            <Field label="名称"><input name="name" required /></Field>
            <Field label="类型"><select name="type"><option value="webhook">webhook</option><option value="email">email</option><option value="generic">generic</option></select></Field>
            <Field label="配置" hint='webhook 需要 {"url":"https://..."}'><textarea name="config" defaultValue='{"url":""}' /></Field>
            <button className="btn btn-primary btn-sm" type="submit">创建</button>
          </form>
          <QueryBody token={token} loading={channels.loading} error={channels.error} empty={(channels.data?.channels ?? []).length === 0} emptyTitle="没有渠道">
            {(channels.data?.channels ?? []).map((channel) => (
              <div key={channel.id} style={{ borderTop: "1px solid var(--line)", padding: "8px 0" }}>
                <strong>{channel.name}</strong> <span className="muted">{channel.type} {channel.enabled ? "" : "· 停用"}</span>
                <pre className="json">{pretty(channel.config)}</pre>
                <form className="filters" onSubmit={(event) => { event.preventDefault(); const data = new FormData(event.currentTarget); void busy.run(`e${channel.id}`, async () => { const raw = String(data.get("config") || "").trim(); await updateChannel(token, channel.id, { name: String(data.get("name") || channel.name), enabled: data.get("enabled") === "1", ...(raw ? { config: raw === "null" ? null : parseObject(raw) } : {}) }); channels.reload(); return "已更新渠道"; }); }}>
                  <input name="name" defaultValue={channel.name} aria-label="渠道名称" />
                  <select name="enabled" defaultValue={channel.enabled ? "1" : "0"} aria-label="启用"><option value="1">启用</option><option value="0">停用</option></select>
                  <input name="config" placeholder="配置 JSON，留空不变，null 清空" aria-label="配置" />
                  <button className="btn btn-ghost btn-sm" type="submit">保存</button>
                </form>
                <div className="row-actions">
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => void busy.run(`t${channel.id}`, async () => { const result = await testChannel(token, channel.id); deliveries.reload(); return `${result.sent ? "已发送" : "未发出"} · ${label(result.delivery.status)} ${result.delivery.detail || ""}`; })}>测试</button>
                  <button type="button" className="btn btn-danger btn-sm" onClick={() => void busy.run(`d${channel.id}`, async () => { await deleteChannel(token, channel.id); channels.reload(); })}>删除</button>
                </div>
              </div>
            ))}
          </QueryBody>
        </Panel>
        <Panel title="投递">
          <QueryBody token={token} loading={deliveries.loading} error={deliveries.error} empty={(deliveries.data?.deliveries ?? []).length === 0} emptyTitle="没有投递记录">
            <table>
              <thead><tr><th>渠道</th><th>状态</th><th>时间</th></tr></thead>
              <tbody>{(deliveries.data?.deliveries ?? []).map((item) => <tr key={item.id}><td>#{item.channel_id} · {item.event}<div className="muted">{item.detail}</div></td><td>{label(item.status)}</td><td className="num">{formatTime(item.created_at)}</td></tr>)}</tbody>
            </table>
          </QueryBody>
        </Panel>
      </div>
    </>
  );
}

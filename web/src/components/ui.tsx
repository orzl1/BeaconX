"use client";

import type { ReactNode } from "react";

import { budgetPercent } from "@/lib/format";

export function Badge({ children, tone = "neutral" }: { children: ReactNode; tone?: string }) {
  return <span className={`badge badge-${tone}`}>{children}</span>;
}

export function EmptyState({ title, hint }: { title: string; hint?: ReactNode }) {
  return (
    <div className="empty">
      <p className="empty-title">{title}</p>
      {hint ? <p className="empty-hint">{hint}</p> : null}
    </div>
  );
}

export function ErrorBanner({ message, onDismiss }: { message: string; onDismiss?: () => void }) {
  if (!message) return null;
  return (
    <div className="banner banner-error" role="alert">
      <span>{message}</span>
      {onDismiss ? <button type="button" className="btn btn-ghost btn-sm" onClick={onDismiss}>关闭</button> : null}
    </div>
  );
}

export function NoticeBanner({ message }: { message: string }) {
  if (!message) return null;
  return <div className="banner banner-info" role="status">{message}</div>;
}

export function JsonBlock({ value }: { value: unknown }) {
  let text = "（无数据）";
  if (typeof value === "string") text = value || "（空）";
  else if (value !== undefined && value !== null) {
    try { text = JSON.stringify(value, null, 2); } catch { text = String(value); }
  }
  return <pre className="json">{text}</pre>;
}

export function PageHeader({ kicker, title, description, actions }: { kicker?: string; title: string; description?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="page-head">
      <div>
        {kicker ? <p className="kicker">{kicker}</p> : null}
        <h1>{title}</h1>
        {description ? <p className="lede">{description}</p> : null}
      </div>
      {actions ? <div className="page-actions">{actions}</div> : null}
    </header>
  );
}

export function Panel({ title, extra, children }: { title?: ReactNode; extra?: ReactNode; children: ReactNode }) {
  return (
    <section className="panel">
      {title || extra ? <header className="panel-head"><h2>{title}</h2>{extra}</header> : null}
      <div className="panel-body">{children}</div>
    </section>
  );
}

export function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      {hint ? <small>{hint}</small> : null}
    </label>
  );
}

export function Drawer({ open, title, onClose, children }: { open: boolean; title: string; onClose: () => void; children: ReactNode }) {
  if (!open) return null;
  return (
    <div className="drawer-root">
      <button type="button" className="drawer-backdrop" aria-label="关闭" onClick={onClose} />
      <aside className="drawer" role="dialog" aria-label={title}>
        <header><h2>{title}</h2><button type="button" className="btn btn-ghost btn-sm" onClick={onClose}>关闭</button></header>
        <div className="drawer-body">{children}</div>
      </aside>
    </div>
  );
}

export function TokenNotice({ token }: { token: string }) {
  if (token) return null;
  return <NoticeBanner message="先在右上角保存管理令牌。令牌只留在这台浏览器里。" />;
}

export function QueryBody({ loading, error, empty, emptyTitle, children, token }: { loading: boolean; error: string | null; empty?: boolean; emptyTitle?: string; children: ReactNode; token?: string }) {
  if (token === "") return null;
  if (error && empty) return <ErrorBanner message={error} />;
  if (loading && empty) return <p className="loading-text">正在读取…</p>;
  if (empty) return <EmptyState title={emptyTitle ?? "没有数据"} />;
  return (
    <>
      {error ? <ErrorBanner message={error} /> : null}
      {children}
    </>
  );
}

export function BudgetMeter({ used, max, unit }: { used: number; max: number; unit: string }) {
  const percent = budgetPercent(used, max);
  return (
    <div className="budget">
      <div className="budget-text"><span>{unit}</span><span className="num">{used.toLocaleString("zh-CN")} / {max > 0 ? max.toLocaleString("zh-CN") : "不限"}</span></div>
      {percent === null ? <p className="muted">未设上限</p> : <div className="progress"><span style={{ width: `${percent}%` }} /></div>}
    </div>
  );
}

export function LoadMore({ nextId, busy, onLoad }: { nextId?: number; busy?: boolean; onLoad: () => void }) {
  if (!nextId) return null;
  return <div className="row-actions" style={{ marginTop: 12 }}><button type="button" className="btn btn-ghost btn-sm" disabled={busy} onClick={onLoad}>加载更多</button></div>;
}

export function Metric({ label, value, hint }: { label: string; value: ReactNode; hint?: ReactNode }) {
  return <article className="metric"><p className="metric-label">{label}</p><p className="metric-value">{value}</p>{hint ? <p className="metric-hint">{hint}</p> : null}</article>;
}

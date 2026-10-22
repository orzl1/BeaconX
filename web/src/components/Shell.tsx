"use client";

import { useEffect, useState } from "react";
import type { ReactNode } from "react";

import { listApprovals, THEME_STORAGE_KEY, TOKEN_STORAGE_KEY } from "@/lib/api";
import { Link, paths, routeTitle, useRouter } from "@/lib/router";
import { useApi } from "@/lib/use-api";

import { ErrorBanner } from "@/components/ui";

const NAV = [
  { group: "功能", items: [
    { href: paths.dashboard, label: "概览", key: "dashboard" },
    { href: paths.missions, label: "任务", key: "missions" },
    { href: paths.findings, label: "发现", key: "findings" },
    { href: paths.traffic, label: "流量", key: "traffic" },
    { href: paths.commands, label: "命令", key: "commands" },
    { href: paths.llm, label: "模型记录", key: "llm" },
    { href: paths.assets, label: "资产", key: "assets" },
    { href: paths.workspace, label: "工作区", key: "workspace" },
    { href: paths.sync, label: "同步", key: "sync" },
    { href: paths.chat, label: "对话", key: "chat", note: "未开放" },
  ] },
  { group: "系统", items: [
    { href: paths.llmConfig, label: "模型配置", key: "llm-config", note: "未开放" },
    { href: paths.agents, label: "Agents", key: "agents" },
    { href: paths.mcp, label: "MCP", key: "mcp" },
    { href: paths.skills, label: "技能", key: "skills" },
    { href: paths.tools, label: "工具", key: "tools" },
    { href: paths.notify, label: "通知", key: "notify" },
    { href: paths.policy, label: "策略", key: "policy" },
    { href: paths.approvals, label: "审批", key: "approvals" },
    { href: paths.logs, label: "日志", key: "logs" },
    { href: paths.settings, label: "设置", key: "settings" },
  ] },
];

function navKey(page: string): string {
  if (page === "mission") return "missions";
  if (page === "finding") return "findings";
  if (page === "agent") return "agents";
  if (page === "unavailable") return "";
  return page;
}

export function Shell({
  token,
  setToken,
  authError,
  clearAuthError,
  children,
}: {
  token: string;
  setToken: (value: string) => void;
  authError: string | null;
  clearAuthError: () => void;
  children: ReactNode;
}) {
  const { route, navigate } = useRouter();
  const [draft, setDraft] = useState(token);
  const [open, setOpen] = useState(false);
  const [dark, setDark] = useState(false);
  const pending = useApi({
    token,
    enabled: Boolean(token),
    interval: 15000,
    onAuthError: () => undefined,
    load: (current, signal) => listApprovals(current, { status: "pending", limit: 100 }, signal),
  });

  useEffect(() => setDraft(token), [token]);
  useEffect(() => {
    setDark(document.documentElement.classList.contains("dark"));
    document.title = `${routeTitle(route)} · BeaconX`;
    setOpen(false);
  }, [route]);

  const active = navKey(route.page);
  const pendingCount = pending.data?.approvals.length ?? 0;

  return (
    <div className="shell">
      <aside className={open ? "sidebar open" : "sidebar"}>
        <Link href={paths.dashboard} className="brand">
          <span className="mark" aria-hidden="true"><i /></span>
          <span><strong>BEACONX</strong><small>授权测试控制台</small></span>
        </Link>
        <nav className="nav" aria-label="主导航">
          {NAV.map((group) => (
            <div key={group.group} className="nav-group">
              <p>{group.group}</p>
              {group.items.map((item) => (
                <Link key={item.href} href={item.href} className={active === item.key ? "nav-link current" : "nav-link"} current={active === item.key}>
                  <span>{item.label}</span>
                  {item.key === "approvals" && pendingCount > 0 ? <em>{pendingCount > 99 ? "99+" : pendingCount}</em> : null}
                  {item.note ? <em className="nav-note">{item.note}</em> : null}
                </Link>
              ))}
            </div>
          ))}
        </nav>
      </aside>
      {open ? <button type="button" className="nav-scrim" aria-label="关闭导航" onClick={() => setOpen(false)} /> : null}
      <div className="workspace">
        <header className="topbar">
          <button type="button" className="btn btn-ghost btn-sm menu-btn" onClick={() => setOpen(true)}>菜单</button>
          <p className="top-title">{routeTitle(route)}</p>
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            onClick={() => {
              const next = !dark;
              setDark(next);
              document.documentElement.classList.toggle("dark", next);
              try { window.localStorage.setItem(THEME_STORAGE_KEY, next ? "dark" : "light"); } catch { /* 忽略存储失败 */ }
            }}
          >
            {dark ? "浅色" : "深色"}
          </button>
          <form
            className="token-form"
            onSubmit={(event) => {
              event.preventDefault();
              const next = draft.trim();
              setToken(next);
              try {
                if (next) window.localStorage.setItem(TOKEN_STORAGE_KEY, next);
                else window.localStorage.removeItem(TOKEN_STORAGE_KEY);
              } catch { /* 隐私模式可能拒绝写入 */ }
              clearAuthError();
            }}
          >
            <label>
              管理令牌
              <input type="password" autoComplete="off" value={draft} placeholder="启动日志中的临时令牌" onChange={(event) => setDraft(event.target.value)} />
            </label>
            <button type="submit" className="btn btn-primary btn-sm">保存</button>
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              onClick={() => {
                setDraft("");
                setToken("");
                try { window.localStorage.removeItem(TOKEN_STORAGE_KEY); } catch { /* 同上 */ }
                navigate(paths.dashboard);
              }}
            >
              清除
            </button>
          </form>
        </header>
        <main className="content">
          {authError ? <ErrorBanner message={authError} onDismiss={clearAuthError} /> : null}
          {children}
        </main>
      </div>
    </div>
  );
}

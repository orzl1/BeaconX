"use client";

import { createContext, useContext, useEffect, useMemo, useState } from "react";
import type { MouseEvent, ReactNode } from "react";

export type Route =
  | { page: "dashboard" }
  | { page: "missions" }
  | { page: "mission"; id: number; tab: string }
  | { page: "findings" }
  | { page: "finding"; id: number }
  | { page: "traffic" }
  | { page: "commands" }
  | { page: "llm" }
  | { page: "assets" }
  | { page: "workspace" }
  | { page: "sync" }
  | { page: "agents" }
  | { page: "agent"; key: string }
  | { page: "mcp" }
  | { page: "skills" }
  | { page: "notify" }
  | { page: "policy" }
  | { page: "approvals" }
  | { page: "logs" }
  | { page: "settings" }
  | { page: "tools" }
  | { page: "unavailable"; id: "chat" | "llm" }
  | { page: "notfound"; path: string };

export const MISSION_TABS = [
  ["overview", "概览"],
  ["events", "事件"],
  ["plan", "计划"],
  ["graph", "作战图"],
  ["assets", "资产"],
  ["coverage", "覆盖度"],
  ["findings", "发现"],
  ["retests", "复测"],
  ["approvals", "审批"],
  ["traffic", "流量"],
  ["report", "报告"],
] as const;

function normalize(pathname: string): string {
  if (!pathname) return "/";
  return pathname.length > 1 && pathname.endsWith("/") ? pathname.slice(0, -1) : pathname;
}

export function parseRoute(pathname: string, search: string): Route {
  const path = normalize(pathname);
  const query = new URLSearchParams(search);
  const parts = path.split("/").filter(Boolean);
  const idOf = (raw: string | undefined) => {
    const id = Number(raw);
    return Number.isSafeInteger(id) && id > 0 ? id : 0;
  };
  if (parts.length === 0 || parts[0] === "dashboard") return { page: "dashboard" };
  if (parts[0] === "missions" && parts.length === 1) return { page: "missions" };
  if (parts[0] === "missions" && parts.length === 2) {
    const id = idOf(parts[1]);
    if (!id) return { page: "notfound", path };
    const tab = query.get("tab") || "overview";
    return { page: "mission", id, tab: MISSION_TABS.some((item) => item[0] === tab) ? tab : "overview" };
  }
  if (parts[0] === "findings" && parts.length === 1) return { page: "findings" };
  if (parts[0] === "findings" && parts.length === 2) {
    const id = idOf(parts[1]);
    return id ? { page: "finding", id } : { page: "notfound", path };
  }
  if (parts[0] === "traffic") return { page: "traffic" };
  if (parts[0] === "commands") return { page: "commands" };
  if (parts[0] === "llm-records") return { page: "llm" };
  if (parts[0] === "assets") return { page: "assets" };
  if (parts[0] === "workspace") return { page: "workspace" };
  if (parts[0] === "sync") return { page: "sync" };
  if (parts[0] === "chat") return { page: "unavailable", id: "chat" };
  if (parts[0] === "system" && parts[1] === "llm") return { page: "unavailable", id: "llm" };
  if (parts[0] === "system" && parts[1] === "agents" && parts.length === 2) return { page: "agents" };
  if (parts[0] === "system" && parts[1] === "agents" && parts[2]) return { page: "agent", key: decodeURIComponent(parts[2]) };
  if (parts[0] === "system" && parts[1] === "mcp") return { page: "mcp" };
  if (parts[0] === "system" && parts[1] === "skills") return { page: "skills" };
  if (parts[0] === "system" && parts[1] === "tools") return { page: "tools" };
  if (parts[0] === "system" && parts[1] === "notify") return { page: "notify" };
  if (parts[0] === "system" && parts[1] === "policy") return { page: "policy" };
  if (parts[0] === "system" && parts[1] === "approvals") return { page: "approvals" };
  if (parts[0] === "system" && parts[1] === "logs") return { page: "logs" };
  if (parts[0] === "system" && parts[1] === "settings") return { page: "settings" };
  return { page: "notfound", path };
}

export function routeTitle(route: Route): string {
  switch (route.page) {
    case "dashboard": return "概览";
    case "missions": return "任务";
    case "mission": return `任务 #${route.id}`;
    case "findings": return "发现";
    case "finding": return `发现 #${route.id}`;
    case "traffic": return "流量";
    case "commands": return "命令";
    case "llm": return "模型记录";
    case "assets": return "资产";
    case "workspace": return "工作区";
    case "sync": return "同步";
    case "agents":
    case "agent": return "Agents";
    case "mcp": return "MCP";
    case "skills": return "技能";
    case "notify": return "通知";
    case "policy": return "策略";
    case "approvals": return "审批";
    case "logs": return "日志";
    case "settings": return "设置";
    case "tools": return "工具";
    case "unavailable": return route.id === "chat" ? "对话" : "模型配置";
    default: return "未找到";
  }
}

export const paths = {
  dashboard: "/dashboard",
  missions: "/missions",
  mission: (id: number, tab = "overview") => (tab === "overview" ? `/missions/${id}` : `/missions/${id}?tab=${tab}`),
  findings: "/findings",
  finding: (id: number) => `/findings/${id}`,
  traffic: "/traffic",
  commands: "/commands",
  llm: "/llm-records",
  assets: "/assets",
  workspace: "/workspace",
  sync: "/sync",
  agents: "/system/agents",
  agent: (key: string) => `/system/agents/${encodeURIComponent(key)}`,
  mcp: "/system/mcp",
  skills: "/system/skills",
  tools: "/system/tools",
  notify: "/system/notify",
  policy: "/system/policy",
  approvals: "/system/approvals",
  logs: "/system/logs",
  settings: "/system/settings",
  chat: "/chat",
  llmConfig: "/system/llm",
};

interface RouterValue {
  route: Route;
  navigate: (href: string) => void;
}

const RouterContext = createContext<RouterValue | null>(null);

export function RouterProvider({ children }: { children: ReactNode }) {
  const [href, setHref] = useState("/");
  useEffect(() => {
    const read = () => setHref(`${window.location.pathname}${window.location.search}`);
    read();
    window.addEventListener("popstate", read);
    return () => window.removeEventListener("popstate", read);
  }, []);
  const value = useMemo<RouterValue>(() => {
    const url = new URL(href, "http://beaconx.local");
    return {
      route: parseRoute(url.pathname, url.search),
      navigate: (next) => {
        const current = `${window.location.pathname}${window.location.search}`;
        if (next === current) return;
        window.history.pushState(null, "", next);
        setHref(next);
      },
    };
  }, [href]);
  return <RouterContext.Provider value={value}>{children}</RouterContext.Provider>;
}

export function useRouter(): RouterValue {
  const value = useContext(RouterContext);
  if (!value) throw new Error("useRouter 必须在 RouterProvider 内使用");
  return value;
}

export function Link({ href, className, children, current }: { href: string; className?: string; children: ReactNode; current?: boolean }) {
  const { navigate } = useRouter();
  const onClick = (event: MouseEvent<HTMLAnchorElement>) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    navigate(href);
  };
  return (
    <a href={href} className={className} aria-current={current ? "page" : undefined} onClick={onClick}>
      {children}
    </a>
  );
}

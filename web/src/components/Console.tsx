"use client";

import { useCallback, useEffect, useState } from "react";

import { TOKEN_STORAGE_KEY } from "@/lib/api";
import { Link, RouterProvider, paths, useRouter } from "@/lib/router";

import { Shell } from "@/components/Shell";
import { PageHeader } from "@/components/ui";
import { AgentsPage } from "@/views/AgentsPage";
import { ApprovalsPage } from "@/views/ApprovalsPage";
import { AssetsPage } from "@/views/AssetsPage";
import { CommandsPage } from "@/views/CommandsPage";
import { DashboardPage } from "@/views/DashboardPage";
import { FindingsPage } from "@/views/FindingsPage";
import { LlmPage } from "@/views/LlmPage";
import { LogsPage } from "@/views/LogsPage";
import { McpPage } from "@/views/McpPage";
import { MissionPage } from "@/views/MissionPage";
import { MissionsPage } from "@/views/MissionsPage";
import { NotifyPage } from "@/views/NotifyPage";
import { PolicyPage } from "@/views/PolicyPage";
import { SettingsPage } from "@/views/SettingsPage";
import { SkillsPage } from "@/views/SkillsPage";
import { SyncPage } from "@/views/SyncPage";
import { ToolsPage } from "@/views/ToolsPage";
import { TrafficPage } from "@/views/TrafficPage";
import { WorkspacePage } from "@/views/WorkspacePage";

export interface PageProps {
  token: string;
  onAuthError: (message: string) => void;
}

function Routes({ token, onAuthError }: PageProps) {
  const { route } = useRouter();
  switch (route.page) {
    case "dashboard": return <DashboardPage token={token} onAuthError={onAuthError} />;
    case "missions": return <MissionsPage token={token} onAuthError={onAuthError} />;
    case "mission": return <MissionPage token={token} onAuthError={onAuthError} id={route.id} tab={route.tab} />;
    case "findings": return <FindingsPage token={token} onAuthError={onAuthError} />;
    case "finding": return <FindingsPage token={token} onAuthError={onAuthError} findingId={route.id} />;
    case "traffic": return <TrafficPage token={token} onAuthError={onAuthError} />;
    case "commands": return <CommandsPage token={token} onAuthError={onAuthError} />;
    case "llm": return <LlmPage token={token} onAuthError={onAuthError} />;
    case "assets": return <AssetsPage token={token} onAuthError={onAuthError} />;
    case "workspace": return <WorkspacePage token={token} onAuthError={onAuthError} />;
    case "sync": return <SyncPage token={token} onAuthError={onAuthError} />;
    case "agents": return <AgentsPage token={token} onAuthError={onAuthError} />;
    case "agent": return <AgentsPage token={token} onAuthError={onAuthError} agentKey={route.key} />;
    case "mcp": return <McpPage token={token} onAuthError={onAuthError} />;
    case "skills": return <SkillsPage token={token} onAuthError={onAuthError} />;
    case "notify": return <NotifyPage token={token} onAuthError={onAuthError} />;
    case "policy": return <PolicyPage token={token} onAuthError={onAuthError} />;
    case "approvals": return <ApprovalsPage token={token} onAuthError={onAuthError} />;
    case "logs": return <LogsPage token={token} onAuthError={onAuthError} />;
    case "settings": return <SettingsPage token={token} onAuthError={onAuthError} />;
    case "tools":
      return <ToolsPage />;
    case "unavailable":
      return route.id === "chat" ? (
        <PageHeader kicker="尚未提供接口" title="对话" description="后端还没有会话与消息接口。这个入口只说明缺口，不展示假对话。" />
      ) : (
        <>
          <PageHeader kicker="尚未提供接口" title="模型配置" description="模型 profile 仍在配置文件里，没有增删接口。改模型需要改配置后重启。运行参数里能改的是并发、默认预算和审批超时。" actions={<Link href={paths.settings} className="btn btn-primary btn-sm">打开设置</Link>} />
        </>
      );
    default:
      return <PageHeader title="没有这个页面" description={route.page === "notfound" ? route.path : ""} />;
  }
}

export function Console() {
  const [token, setToken] = useState("");
  const [ready, setReady] = useState(false);
  const [authError, setAuthError] = useState<string | null>(null);
  const onAuthError = useCallback((message: string) => setAuthError(message), []);

  useEffect(() => {
    try {
      const saved = window.localStorage.getItem(TOKEN_STORAGE_KEY);
      if (saved) setToken(saved);
    } catch { /* 隐私模式按未保存处理 */ }
    setReady(true);
  }, []);

  if (!ready) return <div className="boot">BeaconX</div>;

  return (
    <RouterProvider>
      <Shell token={token} setToken={setToken} authError={authError} clearAuthError={() => setAuthError(null)}>
        <Routes token={token} onAuthError={onAuthError} />
      </Shell>
    </RouterProvider>
  );
}

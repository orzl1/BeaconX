"use client";

// BeaconX M6 控制台入口：顶部令牌 + 三个功能区（任务 / 审批 / 系统）。
//
// 静态导出约束：
//   - 只有一个页面，不依赖动态路由段；任务详情通过页面内选中状态展示。
//   - 不使用 next/headers、Server Actions、useSearchParams 等运行时能力。

import { useCallback, useEffect, useState } from "react";

import { ApprovalsPanel } from "@/components/ApprovalsPanel";
import { MissionDetail } from "@/components/MissionDetail";
import { MissionsPanel } from "@/components/MissionsPanel";
import { SystemPanel } from "@/components/SystemPanel";
import { TokenBar } from "@/components/TokenBar";
import { ErrorBanner } from "@/components/ui";
import { TOKEN_STORAGE_KEY } from "@/lib/api";

type Tab = "missions" | "approvals" | "system";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "missions", label: "任务" },
  { id: "approvals", label: "审批" },
  { id: "system", label: "系统" },
];

export default function Page() {
  const [token, setToken] = useState("");
  const [tokenLoaded, setTokenLoaded] = useState(false);
  const [tab, setTab] = useState<Tab>("missions");
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [authError, setAuthError] = useState<string | null>(null);

  // localStorage 只能在浏览器读取，放到 effect 里避免 hydration 不一致。
  useEffect(() => {
    try {
      const saved = window.localStorage.getItem(TOKEN_STORAGE_KEY);
      if (saved) setToken(saved);
    } catch {
      // 隐私模式等场景下 localStorage 可能不可用；此时按未保存令牌处理。
    }
    setTokenLoaded(true);
  }, []);

  const saveToken = useCallback((next: string) => {
    setToken(next);
    try {
      window.localStorage.setItem(TOKEN_STORAGE_KEY, next);
    } catch {
      // 存储失败不影响本次会话使用。
    }
    setAuthError(null);
  }, []);

  const clearToken = useCallback(() => {
    setToken("");
    try {
      window.localStorage.removeItem(TOKEN_STORAGE_KEY);
    } catch {
      // 同上。
    }
    setSelectedId(null);
  }, []);

  const handleAuthError = useCallback((message: string) => {
    setAuthError(message);
  }, []);

  return (
    <div className="app">
      <header className="topbar">
        <div className="topbar-inner">
          <div className="brand">
            <span className="brand-mark" aria-hidden="true">
              B
            </span>
            <span className="brand-name">BeaconX</span>
            <span className="brand-tag">自主安全测试控制台</span>
          </div>
          <TokenBar token={token} onSave={saveToken} onClear={clearToken} />
        </div>
      </header>

      <main className="page">
        {authError ? (
          <ErrorBanner id="auth-error" message={authError} onDismiss={() => setAuthError(null)} />
        ) : null}

        <nav className="tabs" aria-label="功能面板">
          {TABS.map((item) => (
            <button
              key={item.id}
              type="button"
              className="tab"
              aria-current={tab === item.id ? "page" : undefined}
              onClick={() => setTab(item.id)}
            >
              {item.label}
            </button>
          ))}
        </nav>

        {!tokenLoaded ? <p className="loading-text">正在读取本地令牌…</p> : null}

        {tokenLoaded && tab === "missions" ? (
          <div className="missions-layout">
            <MissionsPanel
              token={token}
              selectedId={selectedId}
              onSelect={setSelectedId}
              onAuthError={handleAuthError}
            />
            <MissionDetail token={token} missionId={selectedId} onAuthError={handleAuthError} />
          </div>
        ) : null}

        {tokenLoaded && tab === "approvals" ? (
          <ApprovalsPanel token={token} onAuthError={handleAuthError} />
        ) : null}

        {tokenLoaded && tab === "system" ? <SystemPanel token={token} onAuthError={handleAuthError} /> : null}
      </main>

      <footer className="footer">
        BeaconX M6 控制台 · 静态导出（web/out，由 Go 服务托管）· API 同源 /api/v1 ·
        任务、审批、事件与证据均来自真实后端接口，页面不内置任何演示数据。
      </footer>
    </div>
  );
}

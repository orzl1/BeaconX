"use client";

// 顶部令牌输入：管理令牌只保存在本机 localStorage。

import { useEffect, useState } from "react";
import type { FormEvent } from "react";

interface TokenBarProps {
  token: string;
  onSave: (next: string) => void;
  onClear: () => void;
}

/** 令牌脱敏展示：保留首尾少量字符，便于确认"保存的是哪一个"。 */
function maskToken(token: string): string {
  if (token.length <= 8) return `${token.slice(0, 2)}***`;
  return `${token.slice(0, 4)}…${token.slice(-4)}`;
}

export function TokenBar({ token, onSave, onClear }: TokenBarProps) {
  const [draft, setDraft] = useState(token);
  const [reveal, setReveal] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // 外部清除（例如页面初始化读取 localStorage）时同步输入框。
  useEffect(() => {
    setDraft(token);
  }, [token]);

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next = draft.trim();
    if (!next) {
      setError("请输入管理令牌后再保存。");
      return;
    }
    setError(null);
    onSave(next);
  }

  function handleClear() {
    setDraft("");
    setError(null);
    onClear();
  }

  return (
    <form className="token-bar" onSubmit={handleSubmit} aria-label="管理令牌">
      <div className="token-row">
        <label className="sr-only" htmlFor="admin-token">
          管理令牌
        </label>
        <input
          id="admin-token"
          className="input"
          type={reveal ? "text" : "password"}
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          placeholder="粘贴 BEACONX_ADMIN_TOKEN"
          autoComplete="off"
          spellCheck={false}
        />
        <button type="button" className="btn btn-sm" onClick={() => setReveal((value) => !value)}>
          {reveal ? "隐藏" : "显示"}
        </button>
        <button type="submit" className="btn btn-primary btn-sm">
          保存令牌
        </button>
        <button type="button" className="btn btn-ghost btn-sm" onClick={handleClear} disabled={!token && !draft}>
          清除
        </button>
      </div>
      <p className="token-status">
        {token ? (
          <>
            已保存令牌 <span className="ok mono">{maskToken(token)}</span>
            <span className="muted">（仅存于本机 localStorage）</span>
          </>
        ) : (
          <span className="missing">未保存令牌：任务与审批接口需要令牌，健康检查/系统信息可直接查看。</span>
        )}
        {error ? <span className="missing"> {error}</span> : null}
      </p>
    </form>
  );
}

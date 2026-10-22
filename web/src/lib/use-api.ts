"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { describeError, isAuthError } from "@/lib/api";

const AUTH_MESSAGE = "管理令牌无效或已失效，请重新保存令牌后再试。";

export function useApi<T>({
  token,
  enabled = true,
  interval = null,
  watch = [],
  onAuthError,
  load,
}: {
  token: string;
  enabled?: boolean;
  interval?: number | null;
  watch?: unknown[];
  onAuthError: (message: string) => void;
  load: (token: string, signal: AbortSignal) => Promise<T>;
}) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [tick, setTick] = useState(0);
  const loadRef = useRef(load);
  const authRef = useRef(onAuthError);
  loadRef.current = load;
  authRef.current = onAuthError;
  const watchKey = JSON.stringify(watch);

  useEffect(() => {
    if (!enabled || !token) {
      setLoading(false);
      setData(null);
      setError(null);
      return;
    }
    const controller = new AbortController();
    let stop = false;
    const exec = async (initial: boolean) => {
      if (initial) setLoading(true);
      try {
        const next = await loadRef.current(token, controller.signal);
        if (stop) return;
        setData(next);
        setError(null);
      } catch (caught) {
        if (stop || (caught instanceof DOMException && caught.name === "AbortError")) return;
        if (isAuthError(caught)) authRef.current(AUTH_MESSAGE);
        if (!stop) setError(describeError(caught));
      } finally {
        if (!stop) setLoading(false);
      }
    };
    void exec(true);
    const timer = interval ? window.setInterval(() => void exec(false), interval) : undefined;
    return () => {
      stop = true;
      controller.abort();
      if (timer) window.clearInterval(timer);
    };
  }, [token, enabled, interval, watchKey, tick]);

  return { data, error, loading, reload: useCallback(() => setTick((value) => value + 1), []) };
}

export function useBusy(onAuthError: (message: string) => void) {
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  async function run(key: string, action: () => Promise<string | void>) {
    setBusy(key);
    setError(null);
    try {
      const message = await action();
      setNotice(message ?? null);
      return true;
    } catch (caught) {
      if (isAuthError(caught)) onAuthError(AUTH_MESSAGE);
      setError(describeError(caught));
      return false;
    } finally {
      setBusy(null);
    }
  }
  return { busy, error, notice, setNotice, setError, run };
}

/** 把游标下一页接到首页后面。筛选变化时清空。 */
export function useTail<T>(resetKey: string) {
  const [rows, setRows] = useState<T[]>([]);
  const [next, setNext] = useState(0);
  useEffect(() => {
    setRows([]);
    setNext(0);
  }, [resetKey]);
  const push = useCallback((pageRows: T[], pageNext?: number) => {
    setRows((current) => [...current, ...pageRows]);
    setNext(pageNext && pageNext > 0 ? pageNext : 0);
  }, []);
  return { rows, next, push };
}

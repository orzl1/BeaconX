"use client";

// 少量通用 hooks，避免每个面板重复写"挂载中"与轮询逻辑。

import { useEffect, useRef } from "react";
import type { RefObject } from "react";

/**
 * 以固定间隔调用回调；delay 为 null 时暂停。
 *
 * 通过 ref 保存最新回调，因此调用方不必用 useCallback 包一层，
 * 也不会因为内联函数变化而反复重建定时器。
 */
export function useInterval(callback: () => void, delay: number | null): void {
  const saved = useRef(callback);

  useEffect(() => {
    saved.current = callback;
  }, [callback]);

  useEffect(() => {
    if (delay === null) return;
    const timer = window.setInterval(() => saved.current(), delay);
    return () => window.clearInterval(timer);
  }, [delay]);
}

/** 组件是否仍挂载：异步请求回来时用来跳过已卸载组件的 setState。 */
export function useMountedRef(): RefObject<boolean> {
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  return mounted;
}

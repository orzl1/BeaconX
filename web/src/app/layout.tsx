import type { Metadata } from "next";
import type { ReactNode } from "react";

import "../styles.css";

export const metadata: Metadata = {
  title: "BeaconX 控制台",
  description: "BeaconX 自主安全测试平台的管理控制台：任务、事件流、审批、证据与系统状态。",
};

export default function RootLayout({ children }: Readonly<{ children: ReactNode }>) {
  return (
    <html lang="zh-CN">
      <body>{children}</body>
    </html>
  );
}

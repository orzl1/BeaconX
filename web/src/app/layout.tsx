import type { Metadata } from "next";
import type { ReactNode } from "react";
import Script from "next/script";

import "../styles.css";

export const metadata: Metadata = {
  title: "BeaconX 控制台",
  description: "BeaconX 授权测试控制台：任务、发现、流量、审批与系统配置。",
  icons: { icon: "/favicon.svg" },
};

export default function RootLayout({ children }: Readonly<{ children: ReactNode }>) {
  return (
    <html lang="zh-CN">
      <body>
        <Script id="beaconx-theme" strategy="beforeInteractive">
          {`try{if(localStorage.getItem("beaconx.theme")==="dark")document.documentElement.classList.add("dark")}catch(e){}`}
        </Script>
        {children}
      </body>
    </html>
  );
}

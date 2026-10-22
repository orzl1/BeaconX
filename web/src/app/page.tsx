"use client";

// 静态导出只有这一页。地址由浏览器 location 决定，Go 对未知路径回退 index.html。

import { Console } from "@/components/Console";

export default function Page() {
  return <Console />;
}

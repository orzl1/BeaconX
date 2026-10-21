import { fileURLToPath } from "node:url";

// 构建模式说明：
//   - 生产/交付：NEXT_EXPORT=1 时 `next build` 产出纯静态目录 web/out，
//     随后由 Go 服务 go:embed 托管（无 Node 运行时，也无 rewrites / Server Actions）。
//   - 本地开发：不设 NEXT_EXPORT，`next dev` 保留 /api 反代到本地后端，
//     让页面与 API/SSE 保持同源，避免浏览器跨域（后端当前未提供 CORS）。
const isExport = process.env.NEXT_EXPORT === "1";
const backend = process.env.BEACONX_API_PROXY ?? "http://127.0.0.1:6969";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 避免父目录的 lockfile / 工作区推断影响构建根目录。
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactStrictMode: true,
  ...(isExport
    ? {
        // 纯静态导出：每个路由产出 <route>/index.html，图片不做运行时优化。
        output: "export",
        trailingSlash: true,
        images: { unoptimized: true },
      }
    : {
        // 开发模式：把 /api/* 反代到本地 Go 后端（默认 :6969）。
        async rewrites() {
          return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
        },
      }),
};

export default nextConfig;

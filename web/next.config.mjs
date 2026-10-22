import { fileURLToPath } from "node:url";

// 构建模式说明：
//   - 生产/交付：NEXT_EXPORT=1 时 `next build` 产出纯静态目录 web/out，
//     随后由 Go 服务 go:embed 托管。静态导出只有首页 HTML，深链靠 Go 回退 index.html，
//     页面自己读 window.location。
//   - 本地开发：不设 NEXT_EXPORT。/api、/healthz、/readyz 反代到 Go；
//     其余未匹配路径回退到 /，让刷新深链仍落到同一页。
const isExport = process.env.NEXT_EXPORT === "1";
const backend = process.env.BEACONX_API_PROXY ?? "http://127.0.0.1:6969";

/** @type {import('next').NextConfig} */
const nextConfig = {
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactStrictMode: true,
  agentRules: false,
  ...(isExport
    ? {
        output: "export",
        trailingSlash: true,
        images: { unoptimized: true },
      }
    : {
        async rewrites() {
          return {
            beforeFiles: [
              { source: "/api/:path*", destination: `${backend}/api/:path*` },
              { source: "/healthz", destination: `${backend}/healthz` },
              { source: "/readyz", destination: `${backend}/readyz` },
            ],
            fallback: [{ source: "/:path*", destination: "/" }],
          };
        },
      }),
};

export default nextConfig;

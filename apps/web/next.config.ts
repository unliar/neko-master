import type { NextConfig } from "next";
import createNextIntlPlugin from "next-intl/plugin";
import { readFileSync } from "fs";
import { join } from "path";

const withNextIntl = createNextIntlPlugin();

// Read version from root package.json
const rootPkg = JSON.parse(
  readFileSync(join(__dirname, "../../package.json"), "utf-8"),
);

// API URL configuration（仅开发模式用于 rewrites 代理）
const apiUrl = process.env.API_URL || "http://localhost:3001";
const API_DESTINATION = apiUrl.endsWith("/api")
  ? `${apiUrl}/:path*`
  : `${apiUrl}/api/:path*`;

// SPA 静态导出模式
// ---------------------------------------------------------------
// 打包成单二进制时，前端产物由 Go 后端以同源方式托管：
//   - 页面/静态资源：Go embed 的静态文件
//   - /api/*        ：Go 的 HTTP handler
//   - /ws           ：Go 的 WebSocket handler（同源同端口）
// 因此前端所有请求都用相对路径（/api、/ws），无需跨域配置。
//
// 注意 output: 'export' 的硬性约束：
//   - rewrites / headers 不生效（仅在 dev 下保留 rewrites 方便联调）
//   - next/image 的服务端优化不可用 → 必须 unoptimized
//   - 每个动态路由段必须有 generateStaticParams
const nextConfig: NextConfig = {
  reactStrictMode: true,
  output: "export",
  // 导出为 index.html 目录结构，便于 Go 静态文件服务做目录匹配
  trailingSlash: true,
  // 静态导出无法运行 Next 的图片优化服务，必须关闭
  images: { unoptimized: true },
  env: {
    NEXT_PUBLIC_APP_VERSION: rootPkg.version || "0.0.0",
  },
  turbopack: {},
  // rewrites 只在 dev 生效（生产是静态导出，由 Go 同源托管，不需要代理）
  async rewrites() {
    if (process.env.NODE_ENV === "production") {
      return [];
    }
    return [
      {
        source: "/manifest.json",
        destination: "/manifest.webmanifest",
      },
      {
        source: "/api/:path*",
        destination: API_DESTINATION,
      },
    ];
  },
};

// Apply PWA
// ---------------------------------------------------------------
// 用 DISABLE_PWA=1 显式关闭，而不是靠 NODE_ENV 判断——
// 之前用 NODE_ENV=development 跑生产构建会让 React 的 dev/prod 版本混用，
// 引发 "Cannot read properties of null (reading 'useContext')" 这类诡异错误。
let finalConfig = withNextIntl(nextConfig);

if (process.env.DISABLE_PWA !== "1") {
  try {
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const withPWA = require("@ducanh2912/next-pwa");
    finalConfig = withPWA({
      dest: "public",
      register: true,
      skipWaiting: true,
      disable: false,
      buildExcludes: [/middleware-manifest\.json$/, /runtime-config\.js$/],
    })(finalConfig);
  } catch {
    // PWA 插件不可用时忽略
  }
}

export default finalConfig;

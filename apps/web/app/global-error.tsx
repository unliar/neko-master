"use client";

// 静态导出时这个页面会参与预渲染，但它是 "use client" 且渲染完整
// <html>/<body>，React 19 在 SSR 阶段拿不到 context 会抛
// "Cannot read properties of null (reading 'useContext')"。
// 标记 dynamic = "error" 让 Next.js 跳过它的静态预渲染（降级为 500 页）。
// 运行时它照常工作：只有在 locale layout 崩溃时才会被触发。
export const dynamic = "error";

// Root-level error boundary — replaces the entire document when the locale
// layout itself throws, so it must render its own <html>/<body> and use no
// app providers or theme context.
export default function GlobalError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <html lang="en">
      <body
        style={{
          margin: 0,
          minHeight: "100vh",
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          fontFamily:
            "-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif",
          background: "#0b0f19",
          color: "#e2e8f0",
        }}>
        <div style={{ textAlign: "center", maxWidth: 420, padding: 24 }}>
          <h2 style={{ fontSize: 18, marginBottom: 8 }}>Something went wrong</h2>
          <p style={{ fontSize: 14, color: "#94a3b8", marginBottom: 4 }}>
            应用出现了未处理的错误，请重试。
          </p>
          {error.digest && (
            <p style={{ fontSize: 12, color: "#64748b", fontFamily: "monospace" }}>
              digest: {error.digest}
            </p>
          )}
          <button
            onClick={reset}
            style={{
              marginTop: 16,
              padding: "8px 20px",
              borderRadius: 9999,
              border: "1px solid #334155",
              background: "transparent",
              color: "#e2e8f0",
              fontSize: 13,
              cursor: "pointer",
            }}>
            Retry / 重试
          </button>
        </div>
      </body>
    </html>
  );
}

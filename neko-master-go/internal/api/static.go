package api

import (
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed all:web/out
var embeddedWebFS embed.FS

// webFS 返回嵌入了前端静态产物的文件系统，根目录对齐 web/out 内部。
//
// 用 embed 而非 http.Dir 的原因：产物被编译进二进制，
// 单文件分发时不需要在部署目录另放前端文件。
// 但 embed 要求被嵌入的目录在构建时存在且非空，
// 否则编译失败——所以仓库里放了一个 .gitkeep 占位。
func webFS() (fs.FS, bool) {
	sub, err := fs.Sub(embeddedWebFS, "web/out")
	if err != nil {
		return nil, false
	}
	// 空产物时（只有占位文件）视为未构建前端
	if entries, err := fs.ReadDir(sub, "."); err != nil || len(entries) == 0 {
		return nil, false
	}
	return sub, true
}

// staticFileServer 提供嵌入了 embed.FS 的静态文件服务，带 SPA fallback。
//
// 前端是 output: 'export' 的多页静态站点（/zh/、/en/ 各自 index.html），
// 路由都在前端完成，所以：
//  1. 命中真实文件 → 直接返回
//  2. 未命中但路径像页面（无扩展名）→ 回落到对应 locale 的 index.html
//  3. / 根路径 → 落到默认 locale（zh）
//  4. 带扩展名但不存在的资源 → 真 404，避免把缺失的 JS 当 HTML 返回
func staticFileServer() (http.Handler, bool) {
	sub, ok := webFS()
	if !ok {
		return nil, false
	}
	// serveFile 直接读出并返回指定文件内容。
	//
	// 不用 http.FileServer 处理 index.html，是因为它对 embed.FS 的
	// 目录请求会触发自身的 redirect 逻辑，把 /zh/ 301 到它自己，成环。
	// embed.FS 的文件句柄不支持 Seek，所以也不能用 http.ServeContent，
	// 这里直接 io.Copy 并手动设置 Content-Type / Content-Length。
	serveFile := func(w http.ResponseWriter, r *http.Request, name string) {
		f, err := sub.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			// mime 标准库不认 .webmanifest 等较新的扩展名，补一份映射，
			// 否则 Content-Type 为空，浏览器会按纯文本解析导致 PWA 装不上。
			ct = fallbackMIME[strings.ToLower(path.Ext(name))]
		}
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		// API / WS / 健康检查绝不应落到静态服务
		if strings.HasPrefix(p, "/api/") || p == "/ws" || p == "/health" {
			http.NotFound(w, r)
			return
		}

		// 根路径落到默认 locale（与 next-intl defaultLocale: zh 一致）
		if p == "/" {
			http.Redirect(w, r, "/zh/", http.StatusFound)
			return
		}

		clean := strings.TrimPrefix(path.Clean(p), "/")

		// locale 根路径（/zh/、/en/）→ 直接返回对应 index.html
		if idx := spaIndexPath(p); idx != "" {
			if clean == "zh" || clean == "en" || spaIsLocaleRoot(p) {
				serveFile(w, r, idx)
				return
			}
		}

		// 尝试真实文件（_next/static、icons、fonts 等）
		if clean != "" && clean != "." {
			if f, err := sub.Open(clean); err == nil {
				f.Close()
				if st, err := f.Stat(); err == nil && !st.IsDir() {
					serveFile(w, r, clean)
					return
				}
			}
		}

		// SPA fallback：无扩展名的路径按页面处理
		if !strings.Contains(pathBase(p), ".") {
			if idx := spaIndexPath(p); idx != "" {
				serveFile(w, r, idx)
				return
			}
		}

		http.NotFound(w, r)
	}), true
}

// spaIsLocaleRoot 判断是否是 locale 根路径（/zh 或 /zh/）。
func spaIsLocaleRoot(p string) bool {
	c := strings.Trim(p, "/")
	return c == "zh" || c == "en"
}

// fallbackMIME 补 mime 标准库缺失的扩展名。
var fallbackMIME = map[string]string{
	".webmanifest": "application/manifest+json",
	".json":        "application/json",
	".mjs":         "text/javascript; charset=utf-8",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".ttf":         "font/ttf",
	".svg":         "image/svg+xml",
	".webp":        "image/webp",
	".avif":        "image/avif",
}

// pathBase 取路径最后一段。
func pathBase(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// spaIndexPath 把请求路径映射到它所属 locale 的 index.html。
//
// 只认 /zh/ 与 /en/ 两个前缀（对应 routing.locales）。
// 返回空表示该路径不属于任何已知 locale 段。
func spaIndexPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	for _, loc := range []string{"zh", "en"} {
		if p == loc || strings.HasPrefix(p, loc+"/") {
			return loc + "/index.html"
		}
	}
	return ""
}

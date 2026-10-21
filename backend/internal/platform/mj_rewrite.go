package platform

import (
	"net/http"
	"strings"
)

// 本文件实现 `/<seg>/mj/<rest>` → `/mj/<rest>` 的 mux 前置重写（R-MJ-07）。
//
// 【为什么必须重写而不是注册别名模式】relay-05 §0.2 要求 `/mj/*` 与
// `/{mode}/mj/*` 语义等价（基线同时注册两组）。Go 1.22+ ServeMux 无法同时
// 表达「任意第二段为 mj」与 `/v1/` 兜底：`POST /{mode}/mj/submit/imagine` 与
// `POST /v1/{path...}` 对 `/v1/mj/submit/imagine` 同样具体，注册期 panic
// （R-MJ-07 已实测）。前置重写把别名收敛到唯一族根，mux 只需要认识 `/mj/*`。
//
// 【排除的首段】`api` / `v1` / `pg` / `healthz` / `readyz` 是既有处理面：
//   - `/v1/mj/...` 留给插件原生路由（R-09 允许插件声明 /v1 路径）；
//   - `/api/mj/...` 是管理面，不是 MJ 客户端协议；
//   - `/pg/mj/...` 是 Playground 会话面；
//   - 探针段不应被业务重写吞掉。
//
// 重写必须发生在 SecurityHeaders 之内（`SecurityHeaders(mjPathRewrite(mux))`）：
// 头注入覆盖 404 与重写后的请求，避免「重写失败时漏安全头」。

// mjPathRewrite 返回包住 mux 的重写 handler。
//
// 只改 URL.Path，不动 query/body/方法；请求对象 Clone 后再改，避免与调用方
// 共享的 *http.Request 产生意外写入（中间件链可能复用）。
func mjPathRewrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rewritten, ok := rewriteMJPath(r.URL.Path); ok {
			clone := r.Clone(r.Context())
			urlCopy := *r.URL
			urlCopy.Path = rewritten
			// RawPath 必须清空：它若保留原编码形状会与新的 Path 不一致，
			// EscapedPath 会退化到 Path，但显式清空避免上游中间件读到旧值。
			urlCopy.RawPath = ""
			clone.URL = &urlCopy
			next.ServeHTTP(w, clone)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rewriteMJPath 计算重写后的路径。
//
// 输入必须是 `/<seg>/mj` 或 `/<seg>/mj/<rest>`（恰好一个前导段），且 seg 不在
// 保留集合内；否则 ok=false（原样放行）。`/mj/...` 自身不需要重写。
func rewriteMJPath(path string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" || strings.HasPrefix(trimmed, "mj/") || trimmed == "mj" {
		return "", false
	}
	segments := strings.Split(trimmed, "/")
	if len(segments) < 2 || segments[1] != "mj" {
		return "", false
	}
	// 只接受恰好一个前导段：`/a/b/mj/...` 不是契约里的别名形态，重写它
	// 会把无关路径意外映射进 MJ 族。
	if len(segments) > 2 && segments[2] == "" {
		return "", false
	}
	switch segments[0] {
	case "api", "v1", "pg", "healthz", "readyz":
		return "", false
	}
	if len(segments) == 2 {
		return "/mj", true
	}
	return "/mj/" + strings.Join(segments[2:], "/"), true
}

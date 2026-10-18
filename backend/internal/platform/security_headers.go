package platform

import "net/http"

// SecurityHeaders 在 HTTP 处理链最外层统一注入 security-08 §5.2 的最小响应头集。
//
// 为什么放在 mux 外层：安全头必须覆盖「每一个」响应——探针、API、SSE、错误页
// 与 404 一视同仁；若放进各 handler 或按路由分组，新增端点时极易漏头。
// 为什么 CSP 只声明 frame-ancestors：本服务只产出 JSON/事件流，不渲染 HTML，
// 完整 script-src/default-src 策略没有承载对象；点击劫持防护仍需每响应下发。
// X-Frame-Options 是现代浏览器已废弃头的兼容兜底，规范取值仍以 CSP 为准。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

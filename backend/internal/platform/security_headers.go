package platform

import "net/http"

// SecurityHeaders 在 HTTP 处理链最外层统一注入 security-08 §2.1 的最小响应头集。
//
// 为什么放在 mux 外层：安全头必须覆盖「每一个」响应——探针、API、SSE、错误页
// 与 404 一视同仁；若放进各 handler 或按路由分组，新增端点时极易漏头。
//
// M0 的响应全部属于 §2.1 的 B（JSON API）/ C（SSE）/ E（运维端点）类，
// 因此按矩阵冻结为：nosniff + Referrer-Policy: no-referrer + CSP frame-ancestors。
//   - Referrer-Policy：B/C/E 类用 no-referrer（A 类 HTML 才用
//     strict-origin-when-cross-origin）；M0 不产出 HTML，故统一 no-referrer。
//   - X-Frame-Options：§2.1 明确「不发」（obsolete）；点击劫持防护由 CSP
//     frame-ancestors 'none' 承担（TC-H-07 断言不得出现 XFO）。
//   - CSP 只声明 frame-ancestors：本服务只产出 JSON/事件流，不渲染 HTML，
//     完整 script-src/default-src 策略没有承载对象；防嵌入仍需每响应下发。
//   - HSTS 不在本中间件：它只在 TLS 终结处（edge）注入，gateway 是明文内网。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

package metrics

import (
	"net/http"
	"time"
)

// httpMethodWhitelist 是 method 标签的白名单。
//
// 方法名在 HTTP 语法上是任意 token，未认证客户端可以用自定义方法（如
// FOO_<随机串>）制造无界序列。指标只认标准方法，其余统一归 other。
var httpMethodWhitelist = map[string]struct{}{
	http.MethodGet: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodHead: {},
	http.MethodOptions: {},
}

func methodLabel(method string) string {
	if _, ok := httpMethodWhitelist[method]; ok {
		return method
	}
	return "other"
}

// HTTPMiddleware 在 handler 外层记录 HTTP 指标。
//
// 关键点：route 标签取 r.Pattern（Go 1.22+ ServeMux 在调用 handler 前写入
// 模板化模式），绝不取 r.URL.Path——原始 path 含用户/令牌/id，是基数与
// PII 双红线（platform-05 §3.3）。未匹配时 Pattern 为空 → unmatched。
//
// 响应写入器包装只做计数/状态捕获，并透传 Flusher（SSE 桥接依赖它）与
// Unwrap（http.ResponseController 可继续发现底层能力）。
func (s *Set) HTTPMiddleware(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s.IncHTTPInFlight(service)
		rec := &responseRecorder{ResponseWriter: w}
		defer func() {
			s.DecHTTPInFlight(service)
			status := rec.status
			if status == 0 {
				// handler 未写任何内容：net/http 会以 200 结束，指标按同一口径。
				status = http.StatusOK
			}
			// r.Pattern 在 ServeHTTP 返回后才可读；此处已经拿到最终模板。
			s.ObserveHTTP(service, methodLabel(r.Method), r.Pattern, status, time.Since(start), rec.written)
		}()
		next.ServeHTTP(rec, r)
	})
}

// responseRecorder 捕获状态码与响应字节数。
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written int
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// Flush 透传 SSE 刷新能力。底层不支持时静默忽略（与 net/http 的
// ResponseController 语义一致：能力缺失不是错误）。
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能继续发现底层 ResponseWriter 的能力。
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

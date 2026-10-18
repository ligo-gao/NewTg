package platform

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSecurityHeaders 断言 security-08 §2.1 的最小响应头集在正常响应与 404 上均存在，
// 且不得出现已废弃的 X-Frame-Options（TC-H-06/TC-H-07：防嵌入由 CSP 承担）。
//
// M0 的所有响应都是 API/运维类，故 Referrer-Policy 取 no-referrer（而非 HTML 类的
// strict-origin-when-cross-origin）。为什么同时测 404：错误路径若漏头，攻击者就能
// 用不存在的路径探测未加固的分支；为什么测 /debug/pprof：生产下调试面必须不可达。
func TestSecurityHeaders(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityHeaders(mux)

	wantHeaders := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "frame-ancestors 'none'",
	}
	// XFO 是 security-08 §2.1 明确要求「不发」的头；若将来有人以兼容为由加回，
	// 该断言立即失败。
	forbiddenHeaders := []string{"X-Frame-Options"}
	cases := []struct {
		name string
		path string
		want int
	}{
		{name: "healthz 仍为 200", path: "/healthz", want: http.StatusOK},
		{name: "debug 面不可达", path: "/debug/pprof", want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d", rr.Code, tc.want)
			}
			for name, want := range wantHeaders {
				if got := rr.Header().Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			for _, name := range forbiddenHeaders {
				if got := rr.Header().Get(name); got != "" {
					t.Errorf("%s = %q, want header absent", name, got)
				}
			}
		})
	}
}

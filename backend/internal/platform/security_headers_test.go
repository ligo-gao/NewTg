package platform

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSecurityHeaders 断言 security-08 §5.2 最小响应头集在正常响应与 404 上均存在。
//
// 为什么同时测 /debug/pprof：生产下调试面必须不可达；而 404 同样是「每个响应」的
// 一员，如果错误路径漏头，攻击者就能用不存在的路径探测未加固的分支。
func TestSecurityHeaders(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityHeaders(mux)

	wantHeaders := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "strict-origin-when-cross-origin",
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
	}
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
		})
	}
}

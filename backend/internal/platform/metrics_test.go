package platform

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMetricsAuth404：metrics handler 鉴权——无 token / 错 token 一律 404（不暴露端点存在性），
// Bearer 或 ?token= 匹配时 200 且 body 是真实 Prometheus 文本（W10-METRICS）。
func TestMetricsAuth404(t *testing.T) {
	const token = "s3cret"
	handler := newMetricsHandler(token)

	cases := []struct {
		name     string
		auth     string
		query    string
		wantCode int
		wantBody string
	}{
		{"no credentials", "", "", http.StatusNotFound, ""},
		{"wrong bearer", "Bearer wrong", "", http.StatusNotFound, ""},
		{"wrong query token", "", "token=wrong", http.StatusNotFound, ""},
		{"valid bearer", "Bearer " + token, "", http.StatusOK, "newtg_build_info"},
		{"valid query token", "", "token=" + token, http.StatusOK, "newtg_build_info"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := "/metrics"
			if tc.query != "" {
				target += "?" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			body, err := io.ReadAll(rec.Result().Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if tc.wantBody == "" {
				if rec.Code != http.StatusNotFound {
					t.Fatalf("expected 404, got %d", rec.Code)
				}
				return
			}
			if !strings.Contains(string(body), tc.wantBody) {
				t.Fatalf("body %q missing %q", string(body), tc.wantBody)
			}
		})
	}
}

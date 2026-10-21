package platform

// W8/MJ-a2 路由别名重写验收（R-MJ-07）。
//
// 断言 `/ <seg>/mj/<rest>` 在 mux 之前被重写成 `/mj/<rest>`，而
// `/v1|api|pg|healthz|readyz/mj/...` 不被吞掉（它们属于其它处理面）。

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRewriteMJPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/foo/mj/submit/imagine", "/mj/submit/imagine", true},
		{"/foo/mj/task/x/fetch", "/mj/task/x/fetch", true},
		{"/foo/mj", "/mj", true},
		{"/mj/submit/imagine", "", false},
		{"/mj", "", false},
		{"/v1/mj/submit/imagine", "", false},
		{"/api/mj/submit/imagine", "", false},
		{"/pg/mj/submit/imagine", "", false},
		{"/healthz/mj/x", "", false},
		{"/readyz/mj/x", "", false},
		{"/a/b/mj/submit/imagine", "", false},
		{"/foo", "", false},
		{"/foo/", "", false},
	}
	for _, tc := range cases {
		got, ok := rewriteMJPath(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("rewriteMJPath(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMJPathRewriteThroughMux(t *testing.T) {
	const (
		probeHeader   = "X-MJ-Probe"
		v1ProbeHeader = "X-V1-Probe"
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mj/submit/imagine", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(probeHeader, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	// MJ-b 的查询端点尚未实现，但重写必须把请求送到 mux 的 /mj 族；
	// 这里用一个探针冻结「别名路径命中 /mj/task/{id}/fetch」。
	mux.HandleFunc("GET /mj/task/{id}/fetch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(probeHeader, r.URL.Path+"?id="+r.PathValue("id"))
		w.WriteHeader(http.StatusOK)
	})
	// /v1 兜底：任何未被更具体模式命中的 /v1 请求都到这里。
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(v1ProbeHeader, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityHeaders(mjPathRewrite(mux))

	t.Run("alias reaches mj family", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/foo/mj/submit/imagine?x=1", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get(probeHeader); got != "/mj/submit/imagine" {
			t.Fatalf("probe path = %q, want /mj/submit/imagine", got)
		}
	})

	t.Run("alias query is preserved", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/foo/mj/submit/imagine?trace=abc", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get(probeHeader); got != "/mj/submit/imagine" {
			t.Fatalf("probe path = %q", got)
		}
	})

	t.Run("alias fetch path reaches mj family", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/foo/mj/task/x1/fetch", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get(probeHeader); got != "/mj/task/x1/fetch?id=x1" {
			t.Fatalf("probe path = %q", got)
		}
	})

	t.Run("v1 mj is not rewritten", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/mj/submit/imagine", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Header().Get(probeHeader) != "" {
			t.Fatalf("/v1/mj must not be rewritten into the mj probe")
		}
		if got := rec.Header().Get(v1ProbeHeader); got != "/v1/mj/submit/imagine" {
			t.Fatalf("v1 probe path = %q, want original path", got)
		}
	})

	t.Run("security headers cover rewritten path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/foo/mj/submit/imagine", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("X-Content-Type-Options = %q", got)
		}
	})
}

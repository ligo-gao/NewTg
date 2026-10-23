package holiday

// source.go 的纯单测：用 httptest 服务器验证解析、完整性校验与重试语义。
// 不触数据库；store.go 的集成测试单独跳过。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sampleYearJSON = `{
  "year": 2026,
  "days": [
    {"name": "元旦", "date": "2026-01-04", "isOffDay": false},
    {"name": "元旦", "date": "2026-01-01", "isOffDay": true}
  ]
}`

// noRetryDelays 把退避等待置空，避免失败用例为 sleep 买单。
func noRetryDelays(t *testing.T) {
	t.Helper()
	prev := fetchRetryDelays
	fetchRetryDelays = nil
	t.Cleanup(func() { fetchRetryDelays = prev })
}

func TestFetchYearParsesOffDayAndMakeupDay(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleYearJSON))
	}))
	defer srv.Close()

	days, err := FetchYear(context.Background(), srv.Client(), srv.URL+"/%d.json", 2026)
	if err != nil {
		t.Fatalf("FetchYear: %v", err)
	}
	if gotPath != "/2026.json" {
		t.Fatalf("请求路径 = %q，期望 /2026.json", gotPath)
	}
	if len(days) != 2 {
		t.Fatalf("days = %d，期望 2", len(days))
	}
	// 按日期升序返回，调用方不需要自己排。
	if days[0].Date != "2026-01-01" || !days[0].OffDay || days[0].Name != "元旦" {
		t.Fatalf("days[0] = %+v，期望 2026-01-01 元旦 法定假日", days[0])
	}
	if days[1].Date != "2026-01-04" || days[1].OffDay {
		t.Fatalf("days[1] = %+v，期望 2026-01-04 调休上班", days[1])
	}
}

func TestFetchYearRejectsBadPayloads(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"malformed json", http.StatusOK, `{"year": 2026,`, "不是合法的年度 JSON"},
		{"empty days", http.StatusOK, `{"year": 2026, "days": []}`, "没有任何日期"},
		{"missing days", http.StatusOK, `{"year": 2026}`, "没有任何日期"},
		{"year mismatch", http.StatusOK, `{"year": 2025, "days": [{"name":"元旦","date":"2025-01-01","isOffDay":true}]}`, "期望 2026"},
		{"date outside year", http.StatusOK, `{"year": 2026, "days": [{"name":"元旦","date":"2027-01-01","isOffDay":true}]}`, "不属于 2026 年"},
		{"bad date", http.StatusOK, `{"year": 2026, "days": [{"name":"元旦","date":"2026-1-1","isOffDay":true}]}`, "YYYY-MM-DD"},
		{"duplicate date", http.StatusOK, `{"year": 2026, "days": [
			{"name":"元旦","date":"2026-01-01","isOffDay":true},
			{"name":"元旦","date":"2026-01-01","isOffDay":false}]}`, "重复"},
		{"http 503", http.StatusServiceUnavailable, `upstream body must not leak`, "HTTP 503"},
		{"http 404", http.StatusNotFound, `not found`, "HTTP 404"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noRetryDelays(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := FetchYear(context.Background(), srv.Client(), srv.URL+"/%d.json", 2026)
			if err == nil {
				t.Fatal("期望失败，实际成功")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %q，期望包含 %q", err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), "must not leak") {
				t.Fatalf("错误信息不得透传响应体: %q", err.Error())
			}
		})
	}
}

func TestFetchYearRetriesServerErrorsThenFails(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	prev := fetchRetryDelays
	fetchRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { fetchRetryDelays = prev })

	_, err := FetchYear(context.Background(), srv.Client(), srv.URL+"/%d.json", 2026)
	if err == nil {
		t.Fatal("期望失败")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("请求次数 = %d，期望 3（1 次首发 + 2 次重试）", got)
	}
}

func TestFetchYearDoesNotRetryClientErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	prev := fetchRetryDelays
	fetchRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { fetchRetryDelays = prev })

	if _, err := FetchYear(context.Background(), srv.Client(), srv.URL+"/%d.json", 2026); err == nil {
		t.Fatal("期望失败")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("请求次数 = %d，期望 1（4xx 不重试）", got)
	}
}

func TestFetchYearRejectsBadTemplate(t *testing.T) {
	for _, template := range []string{
		"https://example.com/holiday.json", // 没有占位符
		"https://example.com/%d/%d.json",   // 两个占位符
		"https://example.com/year-%s.json", // 错误动词
	} {
		if _, err := FetchYear(context.Background(), http.DefaultClient, template, 2026); err == nil {
			t.Fatalf("模板 %q 应被拒绝", template)
		}
	}
}

func TestFetchYearRejectsOutOfRangeYear(t *testing.T) {
	if _, err := FetchYear(context.Background(), http.DefaultClient, "https://example.com/%d.json", 1800); err == nil {
		t.Fatal("年份 1800 应被拒绝")
	}
}

func TestFetchYearHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(sampleYearJSON))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := FetchYear(ctx, srv.Client(), srv.URL+"/%d.json", 2026)
	if err == nil {
		t.Fatal("超时后应返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望 context.DeadlineExceeded", err)
	}
}

func TestYearURLPreservesPercentEncoding(t *testing.T) {
	got, err := yearURL("https://example.com/%d.json?q=a%20b", 2026)
	if err != nil {
		t.Fatalf("yearURL: %v", err)
	}
	if got != "https://example.com/2026.json?q=a%20b" {
		t.Fatalf("URL = %q", got)
	}
}

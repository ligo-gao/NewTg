package perfquery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// matrixBody 是三点矩阵，刻意乱序返回，用来钉住「输出按时间升序」。
const matrixBody = `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1761000120,"3.5"],[1761000000,"1.5"],[1761000060,"2.5"]]}]}}`

// recordedRequest 是假 Prometheus 收到的单次请求。
type recordedRequest struct {
	path  string
	query map[string][]string
}

func (r recordedRequest) get(key string) string {
	if v := r.query[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// recordingProm 记录假 Prometheus 的每次请求。测试在 QueryRange 返回后读取，
// 用互斥锁把 handler goroutine 的写与测试的读串起来（-race 下也干净）。
type recordingProm struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (p *recordingProm) handler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.reqs = append(p.reqs, recordedRequest{path: r.URL.Path, query: r.URL.Query()})
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func (p *recordingProm) snapshot() []recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]recordedRequest, len(p.reqs))
	copy(out, p.reqs)
	return out
}

func (p *recordingProm) count() int {
	return len(p.snapshot())
}

func fixedNow(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func TestQueryRangeParsesMatrix(t *testing.T) {
	rec := &recordingProm{}
	srv := httptest.NewServer(rec.handler(http.StatusOK, matrixBody))
	defer srv.Close()

	start := time.Unix(1761000000, 0)
	end := time.Unix(1761000180, 0)
	c := New(Config{BaseURL: srv.URL, Now: fixedNow(time.Unix(1761000200, 0))})

	got, err := c.QueryRange(context.Background(), NameGatewayRPS, start, end, time.Minute)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}

	wantAt := []int64{1761000000, 1761000060, 1761000120}
	wantValue := []float64{1.5, 2.5, 3.5}
	if len(got) != len(wantAt) {
		t.Fatalf("样本数 = %d, want %d (%v)", len(got), len(wantAt), got)
	}
	for i := range wantAt {
		if got[i].At.Unix() != wantAt[i] {
			t.Errorf("样本 %d At = %d, want %d", i, got[i].At.Unix(), wantAt[i])
		}
		if got[i].Value != wantValue[i] {
			t.Errorf("样本 %d Value = %v, want %v", i, got[i].Value, wantValue[i])
		}
	}

	reqs := rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("请求数 = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.path != "/api/v1/query_range" {
		t.Errorf("path = %q", req.path)
	}
	wantPromQL, _, _ := Lookup(NameGatewayRPS)
	if req.get("query") != wantPromQL {
		t.Errorf("query = %q, want %q", req.get("query"), wantPromQL)
	}
	if req.get("start") != "1761000000" {
		t.Errorf("start = %q", req.get("start"))
	}
	if req.get("end") != "1761000180" {
		t.Errorf("end = %q", req.get("end"))
	}
	if req.get("step") != "60" {
		t.Errorf("step = %q, want \"60\"", req.get("step"))
	}
}

func TestQueryRangeRejectsUnknownName(t *testing.T) {
	rec := &recordingProm{}
	srv := httptest.NewServer(rec.handler(http.StatusOK, matrixBody))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	_, err := c.QueryRange(context.Background(), "gateway.nope", time.Unix(1761000000, 0), time.Unix(1761000180, 0), time.Minute)
	if !errors.Is(err, ErrUnknownQuery) {
		t.Fatalf("err = %v, want ErrUnknownQuery", err)
	}
	if rec.count() != 0 {
		t.Fatalf("未知查询名发出了 %d 个请求", rec.count())
	}
}

func TestQueryRangeTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
			_, _ = io.WriteString(w, matrixBody)
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	c := New(Config{BaseURL: slow.URL, Timeout: 20 * time.Millisecond})
	start := time.Unix(1761000000, 0)

	began := time.Now()
	_, err := c.QueryRange(context.Background(), NameGatewayRPS, start, start.Add(time.Minute), time.Minute)
	elapsed := time.Since(began)

	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("耗时 %v：客户端超时未生效（handler 睡 500ms）", elapsed)
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnknownQuery) {
		t.Fatalf("超时错误不应被降级错误掩盖: %v", err)
	}
}

func TestQueryRangeCachesWithinTTL(t *testing.T) {
	rec := &recordingProm{}
	srv := httptest.NewServer(rec.handler(http.StatusOK, matrixBody))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Now: fixedNow(time.Unix(1761000200, 0))})
	start := time.Unix(1761000000, 0)
	end := time.Unix(1761000180, 0)

	first, err := c.QueryRange(context.Background(), NameGatewayRPS, start, end, time.Minute)
	if err != nil {
		t.Fatalf("第一次查询: %v", err)
	}
	second, err := c.QueryRange(context.Background(), NameGatewayRPS, start, end, time.Minute)
	if err != nil {
		t.Fatalf("第二次查询: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("假服务器收到 %d 个请求, want 1（第二次应命中缓存）", rec.count())
	}
	if len(first) != len(second) {
		t.Fatalf("两次结果长度不同: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("样本 %d 不同: %v vs %v", i, first[i], second[i])
		}
	}

	// 调用方改写返回值不应污染缓存。
	first[0].Value = 99
	third, err := c.QueryRange(context.Background(), NameGatewayRPS, start, end, time.Minute)
	if err != nil {
		t.Fatalf("第三次查询: %v", err)
	}
	if third[0].Value != 1.5 {
		t.Fatalf("缓存被调用方改写: %v", third[0].Value)
	}
}

// TestQueryRangeCacheKeyTruncatesEnd 验证缓存的边界口径：end 先截到
// CacheTTL 桶再入键，桶内不同 end 复用同一次请求，跨桶则重新请求。
func TestQueryRangeCacheKeyTruncatesEnd(t *testing.T) {
	rec := &recordingProm{}
	srv := httptest.NewServer(rec.handler(http.StatusOK, matrixBody))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Now: fixedNow(time.Unix(1761000200, 0))})
	start := time.Unix(1761000000, 0)

	for _, end := range []time.Time{
		time.Unix(1761000005, 0),
		time.Unix(1761000010, 0),
	} {
		if _, err := c.QueryRange(context.Background(), NameGatewayRPS, start, end, time.Minute); err != nil {
			t.Fatalf("end=%d: %v", end.Unix(), err)
		}
	}
	if rec.count() != 1 {
		t.Fatalf("同一 TTL 桶内收到 %d 个请求, want 1", rec.count())
	}

	if _, err := c.QueryRange(context.Background(), NameGatewayRPS, start, time.Unix(1761000015, 0), time.Minute); err != nil {
		t.Fatalf("跨桶查询: %v", err)
	}
	if rec.count() != 2 {
		t.Fatalf("跨桶后累计 %d 个请求, want 2", rec.count())
	}
}

// TestQueryRangeCacheKeyTruncatesStart 钉住真实访问模式：调用方按 now 现算
// start，两次刷新相差 3 秒，但落在同一 TTL 桶内，应复用同一次上游请求，
// 且发给 Prometheus 的 start 是截断值而不是原始值。
func TestQueryRangeCacheKeyTruncatesStart(t *testing.T) {
	rec := &recordingProm{}
	srv := httptest.NewServer(rec.handler(http.StatusOK, matrixBody))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Now: fixedNow(time.Unix(1761000200, 0))})
	end := time.Unix(1761000180, 0)

	for _, start := range []time.Time{
		time.Unix(1761000001, 0),
		time.Unix(1761000004, 0),
	} {
		if _, err := c.QueryRange(context.Background(), NameGatewayRPS, start, end, time.Minute); err != nil {
			t.Fatalf("start=%d: %v", start.Unix(), err)
		}
	}
	if rec.count() != 1 {
		t.Fatalf("同一 TTL 桶内相隔 3s 的 start 收到 %d 个请求, want 1（缓存未命中）", rec.count())
	}
	if got := rec.snapshot()[0].get("start"); got != "1761000000" {
		t.Fatalf("请求 start = %q, want \"1761000000\"（截断值，非原始 start）", got)
	}
}

func TestUnavailableClient(t *testing.T) {
	c := New(Config{})
	if c == nil {
		t.Fatal("New 不得返回 nil")
	}
	if c.Available() {
		t.Fatal("空 BaseURL 的客户端不应 Available")
	}
	_, err := c.QueryRange(context.Background(), NameGatewayRPS, time.Unix(1761000000, 0), time.Unix(1761000180, 0), time.Minute)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestQueryRangeSurfacesPrometheusError(t *testing.T) {
	rec := &recordingProm{}
	srv := httptest.NewServer(rec.handler(http.StatusOK, `{"status":"error","error":"parse error at char 3: unexpected end of input"}`))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	start := time.Unix(1761000000, 0)
	_, err := c.QueryRange(context.Background(), NameGatewayRPS, start, start.Add(time.Minute), time.Minute)
	if err == nil {
		t.Fatal("status=error 应返回错误")
	}
	if !strings.Contains(err.Error(), "parse error at char 3") {
		t.Fatalf("错误未包含 Prometheus 文本: %v", err)
	}
	if !strings.Contains(err.Error(), NameGatewayRPS) {
		t.Fatalf("错误未包含查询名: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("请求数 = %d, want 1", rec.count())
	}
}

func TestQueryRangeHTTPError(t *testing.T) {
	rec := &recordingProm{}
	srv := httptest.NewServer(rec.handler(http.StatusServiceUnavailable, `{"status":"error","error":"service unavailable"}`))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	start := time.Unix(1761000000, 0)
	_, err := c.QueryRange(context.Background(), NameGatewayRPS, start, start.Add(time.Minute), time.Minute)
	if err == nil {
		t.Fatal("非 200 应返回错误")
	}
	if !strings.Contains(err.Error(), "service unavailable") {
		t.Fatalf("错误未包含响应文本: %v", err)
	}
}

func TestQueryValuesEncodeStepInSeconds(t *testing.T) {
	values := queryValues("up", time.Unix(0, 0), time.Unix(60, 0), 90*time.Second)
	if got := values.Get("step"); got != "90" {
		t.Fatalf("step = %q, want \"90\"", got)
	}
	if got := values.Get("start"); got != "0" {
		t.Fatalf("start = %q, want \"0\"", got)
	}
	if got := values.Get("end"); got != "60" {
		t.Fatalf("end = %q, want \"60\"", got)
	}
	if got := values.Get("query"); got != "up" {
		t.Fatalf("query = %q", got)
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c := New(Config{BaseURL: "http://prometheus:9090/"})
	if c.baseURL != "http://prometheus:9090" {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
}

package updatecheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/egress"
)

type mapReader map[string]json.RawMessage

func (m mapReader) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	return value, ok
}

func (m mapReader) set(t *testing.T, key string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	m[key] = raw
}

func newTestChecker(t *testing.T, reader ConfigReader, current string, now func() time.Time) (*Checker, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if now == nil {
		now = time.Now
	}
	checker, err := New(Config{
		Reader:         reader,
		Redis:          rdb,
		CurrentVersion: current,
		Now:            now,
		ClientFactory: func(egress.Policy) *http.Client {
			// 测试只关心抓取/解析；绕过 loopback 端口限制用普通客户端。
			return &http.Client{Timeout: 5 * time.Second}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return checker, mr
}

func TestCheckDisabledDoesNotFetch(t *testing.T) {
	reader := mapReader{}
	checker, _ := newTestChecker(t, reader, "1.0.0", nil)
	result, err := checker.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Enabled || result.State != StateDisabled || result.SourceConfigured {
		t.Fatalf("result = %+v，期望 disabled", result)
	}
}

func TestCheckSourceUnsetDoesNotFetch(t *testing.T) {
	reader := mapReader{}
	reader.set(t, KeyEnabled, true)
	checker, _ := newTestChecker(t, reader, "1.0.0", nil)
	result, err := checker.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.State != StateSourceUnset || result.SourceConfigured {
		t.Fatalf("result = %+v，期望 source_unset", result)
	}
}

func TestCheckSuccessAndCacheSharing(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"latest":"v1.1.0",
			"published_at":"2026-10-20T08:00:00Z",
			"notes_url":"https://example.com/releases/1.1.0",
			"prerelease":false,
			"notes":"first line\nsecond line"
		}`))
	}))
	t.Cleanup(server.Close)

	reader := mapReader{}
	reader.set(t, KeyEnabled, true)
	reader.set(t, KeySourceURL, server.URL+"/latest.json")
	reader.set(t, "sec.ssrf.mode", "off")
	checker, _ := newTestChecker(t, reader, "1.0.0", nil)

	first, err := checker.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if first.State != StateUpdateAvailable || first.Latest != "v1.1.0" || first.Cached {
		t.Fatalf("first = %+v", first)
	}
	if first.Notes != "first line\nsecond line" || first.PublishedAt == nil {
		t.Fatalf("manifest 字段丢失: %+v", first)
	}
	if first.LastSuccess == nil || first.LastSuccess.Latest != "v1.1.0" {
		t.Fatal("成功检查必须写 last_success")
	}

	second, err := checker.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check cached: %v", err)
	}
	if !second.Cached || second.CacheState != "fresh" || second.State != StateUpdateAvailable {
		t.Fatalf("second = %+v，期望缓存命中", second)
	}
	if hits.Load() != 1 {
		t.Fatalf("源站被请求 %d 次，期望 1（缓存共享）", hits.Load())
	}
}

func TestCheckForceRateLimitAndCacheBypass(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"latest":"1.0.0"}`))
	}))
	t.Cleanup(server.Close)

	reader := mapReader{}
	reader.set(t, KeyEnabled, true)
	reader.set(t, KeySourceURL, server.URL)
	reader.set(t, "sec.ssrf.mode", "off")
	checker, mr := newTestChecker(t, reader, "1.0.0", nil)

	if _, err := checker.Check(context.Background(), true); err != nil {
		t.Fatalf("force check: %v", err)
	}
	if _, err := checker.Check(context.Background(), true); err == nil {
		t.Fatal("60s 内第二次立即检查应被拒绝")
	} else if limited, ok := IsRateLimit(err); !ok || limited.RetryAfter <= 0 || limited.RetryAfter > MinCheckInterval {
		t.Fatalf("err = %v，期望 RateLimitError(0,60s]", err)
	}

	mr.FastForward(MinCheckInterval + time.Second)
	if _, err := checker.Check(context.Background(), true); err != nil {
		t.Fatalf("冷却结束后 force check: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("源站被请求 %d 次，期望 2（每次立即检查都绕缓存）", hits.Load())
	}
}

func TestCheckPrereleaseChannelFiltering(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"latest":"1.1.0-beta.1","prerelease":true}`))
	}))
	t.Cleanup(server.Close)

	reader := mapReader{}
	reader.set(t, KeyEnabled, true)
	reader.set(t, KeySourceURL, server.URL)
	reader.set(t, "sec.ssrf.mode", "off")
	checker, _ := newTestChecker(t, reader, "1.0.0", nil)

	stable, err := checker.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("stable check: %v", err)
	}
	if stable.State != StateUpToDate || !stable.PrereleaseSkipped {
		t.Fatalf("stable = %+v，期望按已是最新跳过预发布", stable)
	}

	reader.set(t, KeyChannel, "beta")
	beta, err := checker.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("beta check: %v", err)
	}
	if beta.State != StateUpdateAvailable || !beta.Prerelease || beta.PrereleaseSkipped {
		t.Fatalf("beta = %+v，期望展示预发布更新", beta)
	}
	if hits.Load() != 1 {
		t.Fatalf("切换通道不应清缓存/重新出站，hits=%d", hits.Load())
	}
}

func TestCheckIncomparableVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"latest":"1.2.0"}`))
	}))
	t.Cleanup(server.Close)

	reader := mapReader{}
	reader.set(t, KeyEnabled, true)
	reader.set(t, KeySourceURL, server.URL)
	reader.set(t, "sec.ssrf.mode", "off")

	for _, current := range []string{"v0.0.0", "dev"} {
		checker, _ := newTestChecker(t, reader, current, nil)
		result, err := checker.Check(context.Background(), false)
		if err != nil {
			t.Fatalf("Check(%s): %v", current, err)
		}
		if result.State != StateIncomparable {
			t.Fatalf("current=%s state=%s，期望 incomparable（绝不报有更新）", current, result.State)
		}
	}
}

func TestCheckPayloadErrors(t *testing.T) {
	cases := map[string]string{
		"not json":          `<html>`,
		"latest not semver": `{"latest":"latest"}`,
		"notes_url http":    `{"latest":"1.1.0","notes_url":"http://example.com/x"}`,
		"notes_url js":      `{"latest":"1.1.0","notes_url":"javascript:alert(1)"}`,
		"large notes":       `{"latest":"1.1.0","notes":"` + strings.Repeat("x", maxNotesRunes+1) + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)

			reader := mapReader{}
			reader.set(t, KeyEnabled, true)
			reader.set(t, KeySourceURL, server.URL)
			reader.set(t, "sec.ssrf.mode", "off")
			checker, _ := newTestChecker(t, reader, "1.0.0", nil)
			result, err := checker.Check(context.Background(), false)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if result.State != StateError || result.ErrorKind != ErrorPayload {
				t.Fatalf("result = %+v，期望 payload 错误", result)
			}
			if strings.Contains(result.Error, server.URL) {
				t.Fatalf("错误文本泄露源地址: %q", result.Error)
			}
		})
	}
}

func TestCheckSSRFBlocked(t *testing.T) {
	reader := mapReader{}
	reader.set(t, KeyEnabled, true)
	reader.set(t, KeySourceURL, "http://10.1.2.3/latest.json")
	reader.set(t, "sec.ssrf.mode", "basic")
	reader.set(t, "sec.ssrf.domain_mode", "all")

	checker, _ := newTestChecker(t, reader, "1.0.0", nil)
	result, err := checker.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.State != StateError || result.ErrorKind != ErrorSSRFBlocked {
		t.Fatalf("result = %+v，期望 ssrf_blocked", result)
	}
}

func TestPolicyPrivateOverrideIsFeatureScoped(t *testing.T) {
	reader := mapReader{}
	reader.set(t, "sec.ssrf.allow_private_ip", false)
	reader.set(t, "sec.ssrf.domain_mode", "allowlist")
	reader.set(t, "sec.ssrf.domain_list", []string{"example.com"})
	checker, _ := newTestChecker(t, reader, "1.0.0", nil)
	source := "http://10.0.0.5:8080/latest.json"

	if checker.policy(false, source).AllowPrivateIP {
		t.Fatal("未开启功能级豁免时不得放行私网")
	}
	enabled := checker.policy(true, source)
	if !enabled.AllowPrivateIP {
		t.Fatal("开启功能级豁免后本次出站应放行私网")
	}
	// 默认 allowlist 下必须把本次源主机追加进副本，否则私网仍会被域名清单拒绝。
	found := false
	for _, host := range enabled.DomainList {
		if host == "10.0.0.5" {
			found = true
		}
	}
	if !found {
		t.Fatalf("本次出站 allowlist 应包含源主机: %v", enabled.DomainList)
	}
	// 全局配置对象未被修改：再次构造仍为 false 且不含源主机。
	disabled := checker.policy(false, source)
	if disabled.AllowPrivateIP {
		t.Fatal("功能级豁免泄漏到了全局策略")
	}
	for _, host := range disabled.DomainList {
		if host == "10.0.0.5" {
			t.Fatal("功能级 allowlist 追加泄漏到了全局策略")
		}
	}
}

func TestCheckFailurePreservesLastSuccess(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"latest":"1.1.0","notes_url":"https://example.com/notes"}`))
	}))
	t.Cleanup(server.Close)

	reader := mapReader{}
	reader.set(t, KeyEnabled, true)
	reader.set(t, KeySourceURL, server.URL)
	reader.set(t, "sec.ssrf.mode", "off")
	checker, mr := newTestChecker(t, reader, "1.0.0", nil)

	first, err := checker.Check(context.Background(), false)
	if err != nil || first.State != StateUpdateAvailable {
		t.Fatalf("first = %+v err=%v", first, err)
	}
	fail.Store(true)
	mr.FastForward(MinCheckInterval + time.Second)
	second, err := checker.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("force failure: %v", err)
	}
	if second.State != StateError || second.ErrorKind != ErrorNetwork {
		t.Fatalf("second = %+v，期望 network 错误", second)
	}
	if second.LastSuccess == nil || second.LastSuccess.Latest != "1.1.0" {
		t.Fatalf("失败必须保留 last_success: %+v", second.LastSuccess)
	}
	if second.Latest != "" {
		t.Fatalf("失败结果不应伪装成有最新版本: %+v", second)
	}
}

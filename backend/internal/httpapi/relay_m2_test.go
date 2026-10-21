package httpapi

// W7/M2 端点层用例：令牌自动分组（group=auto）与跨组重试的 HTTP 语义。
//
// 覆盖裁决 R-61…R-66 与 R-68 中不依赖真实 PG 的部分；真实 PG 的投影/计费
// 用例在 relay_m2_integration_test.go（build tag integration）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

const m2TestModel = "gpt-test"

// m2Upstream 记录按分组隔离的上游调用次数。
type m2Upstream struct {
	server *httptest.Server
	calls  atomic.Int64
}

func newM2Upstream(t *testing.T, status int, body string) *m2Upstream {
	t.Helper()
	upstream := &m2Upstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *m2Upstream) count() int { return int(u.calls.Load()) }

// m2Channel 构造一个 OpenAI 渠道，指向给定上游。
func m2Channel(t *testing.T, id int64, upstream *m2Upstream, sealer *SecretSealer) *store.Channel {
	t.Helper()
	baseURL := upstream.server.URL
	return &store.Channel{
		ID:              id,
		Name:            fmt.Sprintf("m2-channel-%d", id),
		Type:            string(contract.ChannelTypeOpenAI),
		BaseURL:         &baseURL,
		Credentials:     sealedTestCredentials(t, sealer),
		SupportedModels: []byte(`["` + m2TestModel + `"]`),
		ModelMapping:    []byte(`{}`),
		Status:          1,
		CreatedAt:       time.Now().UTC(),
	}
}

// newM2Env 装配一个按分组路由的 RelayHandler 环境。
// groups 的键是分组名，值是该组的上游；渠道 ID 从 101 起递增。
func newM2Env(t *testing.T, meta *auth.TokenMeta, groups map[string]*m2Upstream) (*relayTestEnv, map[string]int64) {
	t.Helper()
	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("默认上游不应被调用：%s", r.URL.Path)
	}))
	t.Cleanup(dummy.Close)

	sealer := NewSecretSealer(relaySealerMaster)
	byGroup := make(map[string][]contract.ChannelCandidate, len(groups))
	channels := make(map[int64]*store.Channel, len(groups))
	channelIDs := make(map[string]int64, len(groups))
	nextID := int64(101)
	for group, upstream := range groups {
		id := nextID
		nextID++
		channelIDs[group] = id
		byGroup[group] = []contract.ChannelCandidate{{ChannelID: int(id), Priority: 10, Weight: 1}}
		channels[id] = m2Channel(t, id, upstream, sealer)
	}
	env := newRelayTestEnv(t, dummy, &fakeRelayChannelStore{
		byGroup:  byGroup,
		channels: channels,
		models:   []string{m2TestModel},
	})
	// 多组上游各自有独立 BaseURL：httptest 的 client 也基于普通 Transport，
	// 但显式换成无状态客户端更直观。
	env.handler.client = &http.Client{Timeout: 10 * time.Second}
	env.tokens.byHash[auth.HashTokenKey(relayTestToken)] = meta
	return env, channelIDs
}

func m2AutoMeta(groups []string, retry bool) *auth.TokenMeta {
	return &auth.TokenMeta{
		ID:              relayTestTokenID,
		UserID:          relayTestUserID,
		Status:          "enabled",
		UnlimitedQuota:  true,
		Group:           "auto",
		AutoGroups:      groups,
		CrossGroupRetry: retry,
	}
}

func m2ChatBody() string {
	return `{"model":"` + m2TestModel + `","messages":[{"role":"user","content":"hi"}]}`
}

// TestRelayAutoGroupsNotEnabled 覆盖 R-61 / I-20：历史空列表 auto 令牌在
// 请求期 403 auto_groups_not_enabled，绝不回退用户分组；零预扣、零上游。
func TestRelayAutoGroupsNotEnabled(t *testing.T) {
	upstream := newM2Upstream(t, http.StatusOK, `{}`)
	env, _ := newM2Env(t, m2AutoMeta(nil, true), map[string]*m2Upstream{"g1": upstream})

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403；body=%s", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "auto_groups_not_enabled" {
		t.Fatalf("code = %q，期望 auto_groups_not_enabled", got)
	}
	if env.billing.preConsumeCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatalf("PreConsume=%d Refund=%d，期望 0/0",
			env.billing.preConsumeCount(), env.billing.refundCount())
	}
	if upstream.count() != 0 {
		t.Fatalf("上游调用 = %d，期望 0", upstream.count())
	}
}

// TestRelayAutoCrossGroupRetrySucceeds 覆盖 R-62 / TC-F-02-14-01：
// g1 全部 500 → 换组到 g2 成功；PreConsume 恰好一次，候选按 g1→g2 重查。
func TestRelayAutoCrossGroupRetrySucceeds(t *testing.T) {
	g1 := newM2Upstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	g2 := newM2Upstream(t, http.StatusOK, `{"id":"ok","object":"chat.completion","choices":[]}`)
	env, ids := newM2Env(t, m2AutoMeta([]string{"g1", "g2"}, true),
		map[string]*m2Upstream{"g1": g1, "g2": g2})

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	if g1.count() == 0 {
		t.Fatal("g1 应被尝试（上游调用数 > 0）")
	}
	if got := g2.count(); got != 1 {
		t.Fatalf("g2 上游调用 = %d，期望 1（第二次尝试成功）", got)
	}
	if env.billing.preConsumeCount() != 1 {
		t.Fatalf("PreConsume = %d，期望 1", env.billing.preConsumeCount())
	}
	if got := fmt.Sprint(env.channels.queriedGroups()); got != "[g1 g2]" {
		t.Fatalf("候选查询分组 = %v，期望 [g1 g2]", got)
	}
	_ = ids
}

// TestRelayAutoCrossGroupRetryDisabled 覆盖 TC-F-02-14-02：
// 开关关闭时 g1 耗尽即失败，g2 调用数必须为 0，错误体带 R-66 序列。
func TestRelayAutoCrossGroupRetryDisabled(t *testing.T) {
	g1 := newM2Upstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	g2 := newM2Upstream(t, http.StatusOK, `{"id":"ok"}`)
	env, _ := newM2Env(t, m2AutoMeta([]string{"g1", "g2"}, false),
		map[string]*m2Upstream{"g1": g1, "g2": g2})

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d，期望 502；body=%s", w.Code, w.Body.String())
	}
	if g2.count() != 0 {
		t.Fatalf("g2 上游调用 = %d，期望 0（开关关闭不得换组）", g2.count())
	}
	message := decodeRelayError(t, w.Body.Bytes()).Error.Message
	if !strings.Contains(message, "; groups tried: g1") {
		t.Fatalf("message = %q，期望包含 \"; groups tried: g1\"", message)
	}
}

// TestRelayAutoCrossGroupLimitMessage 覆盖 R-63/R-66 与用例 9：
// auto=[g1..g4]、默认上限 3、全部 500 → 仅 g1..g3 被调用，
// message 含 "g1, g2, g3"，g4 上游零调用。
func TestRelayAutoCrossGroupLimitMessage(t *testing.T) {
	groups := map[string]*m2Upstream{}
	for _, name := range []string{"g1", "g2", "g3", "g4"} {
		groups[name] = newM2Upstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	}
	env, _ := newM2Env(t, m2AutoMeta([]string{"g1", "g2", "g3", "g4"}, true), groups)

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d，期望 502；body=%s", w.Code, w.Body.String())
	}
	if groups["g4"].count() != 0 {
		t.Fatalf("g4 上游调用 = %d，期望 0（R-63 上限 3）", groups["g4"].count())
	}
	message := decodeRelayError(t, w.Body.Bytes()).Error.Message
	if !strings.Contains(message, "; groups tried: g1, g2, g3") {
		t.Fatalf("message = %q，期望包含 \"; groups tried: g1, g2, g3\"", message)
	}
}

// TestRelayAutoAllGroupsEmpty 覆盖补充口径 #5：所有分组无候选 → 503
// （保留 ErrNoAvailableChannel 语义）并携带已尝试分组序列。
func TestRelayAutoAllGroupsEmpty(t *testing.T) {
	dummy := newM2Upstream(t, http.StatusOK, `{}`)
	env, _ := newM2Env(t, m2AutoMeta([]string{"g1", "g2"}, true),
		map[string]*m2Upstream{"g1": dummy, "g2": dummy})
	// 清空两组的候选：上游存在但没有任何可用渠道。
	env.channels.byGroup["g1"] = nil
	env.channels.byGroup["g2"] = nil

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503；body=%s", w.Code, w.Body.String())
	}
	body := decodeRelayError(t, w.Body.Bytes())
	if body.Error.Code != "model_not_found" {
		t.Fatalf("code = %q，期望 model_not_found（503 语义保持）", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "; groups tried: g1, g2") {
		t.Fatalf("message = %q，期望包含已尝试分组", body.Error.Message)
	}
	if env.billing.preConsumeCount() != 0 {
		t.Fatalf("PreConsume = %d，期望 0（无候选不得预扣）", env.billing.preConsumeCount())
	}
	if dummy.count() != 0 {
		t.Fatalf("上游调用 = %d，期望 0", dummy.count())
	}
}

// TestRelayAutoEmptyFirstGroupSucceeds 覆盖 TC-F-02-13-01：
// g1 无候选 → 换组到 g2 成功。
func TestRelayAutoEmptyFirstGroupSucceeds(t *testing.T) {
	g2 := newM2Upstream(t, http.StatusOK, `{"id":"ok","object":"chat.completion","choices":[]}`)
	env, _ := newM2Env(t, m2AutoMeta([]string{"g1", "g2"}, true),
		map[string]*m2Upstream{"g1": newM2Upstream(t, http.StatusOK, `{}`), "g2": g2})
	env.channels.byGroup["g1"] = nil

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	if g2.count() != 1 {
		t.Fatalf("g2 上游调用 = %d，期望 1", g2.count())
	}
}

// TestRelayGroupRatioMissing 覆盖 R-64 / I-25：切换目标组倍率缺失 →
// 403 group_ratio_missing（不落 502），零上游、预扣全额退回。
func TestRelayGroupRatioMissing(t *testing.T) {
	g1 := newM2Upstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	g2 := newM2Upstream(t, http.StatusOK, `{"id":"ok"}`)
	env, _ := newM2Env(t, m2AutoMeta([]string{"g1", "g2"}, true),
		map[string]*m2Upstream{"g1": g1, "g2": g2})
	env.handler.groupConfigured = func(_ context.Context, group string) (bool, error) {
		return group == "g1", nil
	}

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403；body=%s", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "group_ratio_missing" {
		t.Fatalf("code = %q，期望 group_ratio_missing", got)
	}
	if g2.count() != 0 {
		t.Fatalf("g2 上游调用 = %d，期望 0（R-64 在触达上游前拒绝）", g2.count())
	}
	if env.billing.preConsumeCount() != 1 || env.billing.refundCount() != 1 {
		t.Fatalf("PreConsume=%d Refund=%d，期望 1/1（预扣全额退回）",
			env.billing.preConsumeCount(), env.billing.refundCount())
	}
}

// TestRelayGroupConfiguredStorageError 覆盖补充口径 #7：存储故障 → 500，
// 不得当作 false（403）或 true（放行）。
func TestRelayGroupConfiguredStorageError(t *testing.T) {
	g1 := newM2Upstream(t, http.StatusOK, `{}`)
	env, _ := newM2Env(t, m2AutoMeta([]string{"g1"}, true), map[string]*m2Upstream{"g1": g1})
	env.handler.groupConfigured = func(context.Context, string) (bool, error) {
		return false, fmt.Errorf("db down")
	}

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d，期望 500；body=%s", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "database_error" {
		t.Fatalf("code = %q，期望 database_error", got)
	}
	if g1.count() != 0 || env.billing.preConsumeCount() != 0 {
		t.Fatalf("上游=%d PreConsume=%d，期望 0/0", g1.count(), env.billing.preConsumeCount())
	}
}

// TestRelaySpecificGroupMessageUnchanged 覆盖用例 14：具体分组令牌的失败体
// 与 M1 逐字节一致（无 "; groups tried" 后缀）。
func TestRelaySpecificGroupMessageUnchanged(t *testing.T) {
	g1 := newM2Upstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	env, _ := newM2Env(t, &auth.TokenMeta{
		ID: relayTestTokenID, UserID: relayTestUserID, Status: "enabled",
		UnlimitedQuota: true, Group: "g1",
	}, map[string]*m2Upstream{"g1": g1})

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", m2ChatBody())
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d，期望 502；body=%s", w.Code, w.Body.String())
	}
	message := decodeRelayError(t, w.Body.Bytes()).Error.Message
	if strings.Contains(message, "groups tried") {
		t.Fatalf("message = %q，具体分组不得出现 groups tried 后缀", message)
	}
	if !strings.HasPrefix(message, "upstream request failed after 4 attempt(s)") {
		t.Fatalf("message = %q，期望与 M1 相同的重试耗尽文案", message)
	}
}

// TestMessagesAutoCrossGroupDirection 覆盖 R-68：auto=[openai 组, anthropic 组]、
// retry=true → /v1/messages 不得 501，执行层换组到 Anthropic 组并返回 200。
func TestMessagesAutoCrossGroupDirection(t *testing.T) {
	openai := newM2Upstream(t, http.StatusOK, `{}`)
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(messagesUpstreamBody))
	}))
	t.Cleanup(anthropic.Close)

	sealer := NewSecretSealer(relaySealerMaster)
	byGroup := map[string][]contract.ChannelCandidate{
		"g1": {{ChannelID: 201, Priority: 10, Weight: 1}},
		"g2": {{ChannelID: 202, Priority: 10, Weight: 1}},
	}
	channels := map[int64]*store.Channel{
		201: m2Channel(t, 201, openai, sealer),
		202: {
			ID: 202, Name: "m2-anthropic", Type: string(contract.ChannelTypeAnthropic),
			BaseURL: stringPtr(anthropic.URL), Credentials: sealedTestCredentials(t, sealer),
			SupportedModels: []byte(`["` + m2TestModel + `"]`), ModelMapping: []byte(`{}`),
			Status: 1, CreatedAt: time.Now().UTC(),
		},
	}
	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)
	env := newRelayTestEnv(t, dummy, &fakeRelayChannelStore{
		byGroup: byGroup, channels: channels, models: []string{m2TestModel},
	})
	env.handler.client = &http.Client{Timeout: 10 * time.Second}
	env.tokens.byHash[auth.HashTokenKey(relayTestToken)] = m2AutoMeta([]string{"g1", "g2"}, true)

	body := `{"model":"` + m2TestModel + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（不得 501）；body=%s", w.Code, w.Body.String())
	}
	if openai.count() != 0 {
		t.Fatalf("OpenAI 组上游调用 = %d，期望 0（方向过滤后该组无候选）", openai.count())
	}
	if !strings.Contains(w.Body.String(), `"type":"message"`) {
		t.Fatalf("响应不是 Anthropic 原生体：%s", w.Body.String())
	}
}

// TestMessagesAutoSingleOpenAIGroupStill501 覆盖 R-68 的反向：
// 有效序列只有 OpenAI 组时仍按 M1 语义 501，零计费零上游。
func TestMessagesAutoSingleOpenAIGroupStill501(t *testing.T) {
	openai := newM2Upstream(t, http.StatusOK, `{}`)
	env, _ := newM2Env(t, m2AutoMeta([]string{"g1"}, true), map[string]*m2Upstream{"g1": openai})

	body := `{"model":"` + m2TestModel + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d，期望 501；body=%s", w.Code, w.Body.String())
	}
	if openai.count() != 0 || env.billing.preConsumeCount() != 0 {
		t.Fatalf("上游=%d PreConsume=%d，期望 0/0", openai.count(), env.billing.preConsumeCount())
	}
}

// TestMessagesGroupRatioMissingUsesSameMapping 覆盖补充口径 #4：
// /v1/messages 的 writeMessagesMappedError 必须走同一套映射——R-64 的
// group_ratio_missing 是 403，不得落 501（方向不支持）或 502（上游故障）。
func TestMessagesGroupRatioMissingUsesSameMapping(t *testing.T) {
	g1 := newM2Upstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	g2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("g2 upstream must not be called after R-64 rejection")
	}))
	t.Cleanup(g2.Close)

	sealer := NewSecretSealer(relaySealerMaster)
	byGroup := map[string][]contract.ChannelCandidate{
		"g1": {{ChannelID: 301, Priority: 10, Weight: 1}},
		"g2": {{ChannelID: 302, Priority: 10, Weight: 1}},
	}
	channels := map[int64]*store.Channel{
		301: {
			ID: 301, Name: "m2-msg-g1", Type: string(contract.ChannelTypeAnthropic),
			BaseURL: stringPtr(g1.server.URL), Credentials: sealedTestCredentials(t, sealer),
			SupportedModels: []byte(`["` + m2TestModel + `"]`), ModelMapping: []byte(`{}`),
			Status: 1, CreatedAt: time.Now().UTC(),
		},
		302: {
			ID: 302, Name: "m2-msg-g2", Type: string(contract.ChannelTypeAnthropic),
			BaseURL: stringPtr(g2.URL), Credentials: sealedTestCredentials(t, sealer),
			SupportedModels: []byte(`["` + m2TestModel + `"]`), ModelMapping: []byte(`{}`),
			Status: 1, CreatedAt: time.Now().UTC(),
		},
	}
	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)
	env := newRelayTestEnv(t, dummy, &fakeRelayChannelStore{
		byGroup: byGroup, channels: channels, models: []string{m2TestModel},
	})
	env.handler.client = &http.Client{Timeout: 10 * time.Second}
	env.tokens.byHash[auth.HashTokenKey(relayTestToken)] = m2AutoMeta([]string{"g1", "g2"}, true)
	env.handler.groupConfigured = func(_ context.Context, group string) (bool, error) {
		return group == "g1", nil
	}

	body := `{"model":"` + m2TestModel + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403；body=%s", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "group_ratio_missing" {
		t.Fatalf("code = %q，期望 group_ratio_missing", got)
	}
	if env.billing.preConsumeCount() != 1 || env.billing.refundCount() != 1 {
		t.Fatalf("PreConsume=%d Refund=%d，期望 1/1（预扣全额退回）",
			env.billing.preConsumeCount(), env.billing.refundCount())
	}
}

// m2Config 是 ConfigReader 的最小内存实现（单测不得依赖集成库 helper）。
type m2Config map[string]json.RawMessage

func (m m2Config) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	return value, ok
}

// TestResolveCrossGroupRetryMaxGroups 冻结 R-63 的装配期解析与钳制。
func TestResolveCrossGroupRetryMaxGroups(t *testing.T) {
	cases := []struct {
		name   string
		config m2Config
		want   int
	}{
		{"缺省默认 3", nil, 3},
		{"合法值", m2Config{"relay.cross_group_retry_max_groups": json.RawMessage(`5`)}, 5},
		{"零回退默认", m2Config{"relay.cross_group_retry_max_groups": json.RawMessage(`0`)}, 3},
		{"负数回退默认", m2Config{"relay.cross_group_retry_max_groups": json.RawMessage(`-2`)}, 3},
		{"字符串数字", m2Config{"relay.cross_group_retry_max_groups": json.RawMessage(`"4"`)}, 4},
		{"非整数回退默认", m2Config{"relay.cross_group_retry_max_groups": json.RawMessage(`"abc"`)}, 3},
		{"超限钳制 20", m2Config{"relay.cross_group_retry_max_groups": json.RawMessage(`99`)}, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCrossGroupRetryMaxGroups(tc.config); got != tc.want {
				t.Fatalf("resolveCrossGroupRetryMaxGroups = %d，期望 %d", got, tc.want)
			}
		})
	}
}

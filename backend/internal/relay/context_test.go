package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
)

// stubTokenStore 是 auth.TokenLookup 的测试替身。
//
// 键按**哈希后的 key_hash** 存储而不是明文：实现若忘记先调 auth.HashTokenKey
// 直接拿明文查询，测试会取不到元数据而失败——这正是 Task 12 的存储口径
// （auth/context.go：Bearer 路径在查询前必须先哈希，绝不可以明文访问存储）。
type stubTokenStore struct {
	metas map[string]*auth.TokenMeta
	err   error
	calls []string
}

func (s *stubTokenStore) Resolve(_ context.Context, keyHash string) (*auth.TokenMeta, error) {
	s.calls = append(s.calls, keyHash)
	if s.err != nil {
		return nil, s.err
	}
	return s.metas[keyHash], nil
}

const (
	testNKT = "nkt-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// legacy key 自身不得含 '-'：§3.2 的基线怪癖会在首个 '-' 处截断，
	// 含 '-' 的 key 无法与 pin 后缀共存（这正是 §3.2 记录的歧义）。
	testLegacy = "sk-legacykey"
)

func testMeta() *auth.TokenMeta {
	return &auth.TokenMeta{
		ID:             9,
		UserID:         42,
		Status:         "enabled",
		Group:          "vip",
		UnlimitedQuota: true,
	}
}

// TestBuildRelayContext 覆盖 Task 1 的组装断言：
// 身份组来自 M1 Principal（Bearer 路径再补令牌分组）、请求组只填归一化前可判定
// 的字段、鉴权类错误在构造上下文之前早退（返回 nil 上下文，绝不下发半成品）。
func TestBuildRelayContext(t *testing.T) {
	tokenID := int64(9)
	bearerPrincipal := &auth.Principal{UserID: 42, Role: "user", TokenID: &tokenID}

	t.Run("Bearer 令牌填充身份组与令牌分组", func(t *testing.T) {
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testNKT): testMeta(),
		}}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-x","stream":true}`))
		req.Header.Set("Authorization", "Bearer "+testNKT)

		rc, err := BuildRelayContext(context.Background(), bearerPrincipal, req, store)
		if err != nil {
			t.Fatalf("BuildRelayContext() error = %v，期望 nil", err)
		}
		if rc == nil {
			t.Fatal("BuildRelayContext() 返回 nil 上下文")
		}
		if rc.RequestID == "" {
			t.Error("RequestID 为空：计费幂等键缺失（relay-01 §7.1）")
		}
		if rc.UserID != 42 {
			t.Errorf("UserID = %d，期望 42", rc.UserID)
		}
		if rc.TokenID != 9 {
			t.Errorf("TokenID = %d，期望 9", rc.TokenID)
		}
		if rc.UsingGroup != "vip" || rc.UserGroup != "vip" {
			t.Errorf("UsingGroup/UserGroup = %q/%q，期望都为 vip", rc.UsingGroup, rc.UserGroup)
		}
		if rc.RequestPath != "/v1/chat/completions" {
			t.Errorf("RequestPath = %q，期望 /v1/chat/completions", rc.RequestPath)
		}
		if rc.RawFormat != FormatOpenAI {
			t.Errorf("RawFormat = %q，期望 %q", rc.RawFormat, FormatOpenAI)
		}
		if !rc.Streaming {
			t.Error("Streaming = false，期望从请求体 stream 字段读出 true")
		}
		// Task 1 的边界：消息解析归归一化层；此处不得擅自产出半截 IR。
		if rc.Request != nil {
			t.Error("Request 非 nil：Task 1 不得解析 IR（归一化层持有该字段写权）")
		}
		if rc.OriginModel != "" {
			t.Errorf("OriginModel = %q，期望留空（归一化层写入）", rc.OriginModel)
		}
		// 路由/渠道/计费/流式/任务/审计组的写入方在后续层（relay-01 §7.3–§7.8），
		// 组装阶段必须保持零值，否则等于伪造「已选定渠道/已估算计费」。
		if rc.AttemptPlan != nil || rc.ChannelID != 0 || rc.PriceSnapshot != nil ||
			rc.UsageFact != nil || rc.StreamStatus != nil || rc.TaskHandle != nil ||
			len(rc.ConversionChain) != 0 || len(rc.OverrideAudit) != 0 || len(rc.Diagnostics) != 0 {
			t.Error("后续层字段组出现非零值：Task 1 只应写身份组与归一化前可判定的请求组")
		}
	})

	t.Run("Cookie 会话不查令牌且 TokenID 为零", func(t *testing.T) {
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testNKT): testMeta(),
		}}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"stream":false}`))
		// SessionAuth 的 Cookie 优先：同时带 Authorization 时身份仍来自会话
		// （httpapi/middleware.go），因此不得用该头反查令牌分组。
		req.Header.Set("Authorization", "Bearer "+testNKT)

		p := &auth.Principal{UserID: 42, Role: "user"} // TokenID == nil
		rc, err := BuildRelayContext(context.Background(), p, req, store)
		if err != nil {
			t.Fatalf("BuildRelayContext() error = %v，期望 nil", err)
		}
		if rc.UserID != 42 {
			t.Errorf("UserID = %d，期望 42", rc.UserID)
		}
		if rc.TokenID != 0 {
			t.Errorf("TokenID = %d，期望 0（会话路径无令牌，relay-01 §1.5）", rc.TokenID)
		}
		if rc.UsingGroup != "" || rc.UserGroup != "" {
			t.Errorf("分组 = %q/%q，期望留空（会话路径不写令牌分组）", rc.UsingGroup, rc.UserGroup)
		}
		if len(store.calls) != 0 {
			t.Errorf("Resolve 被调用 %d 次，期望 0 次（会话路径不得反查令牌）", len(store.calls))
		}
	})

	t.Run("缺少身份早退且不构造上下文", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		rc, err := BuildRelayContext(context.Background(), nil, req, &stubTokenStore{})
		if !errors.Is(err, ErrPrincipalMissing) {
			t.Fatalf("error = %v，期望 ErrPrincipalMissing", err)
		}
		if rc != nil {
			t.Fatal("鉴权失败时仍返回了 RelayContext：不得下发半成品上下文")
		}
	})

	t.Run("令牌解析失败早退", func(t *testing.T) {
		store := &stubTokenStore{err: errors.New("store unavailable")}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testNKT)

		rc, err := BuildRelayContext(context.Background(), bearerPrincipal, req, store)
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("error = %v，期望 ErrTokenInvalid", err)
		}
		// 对外语义不变（errors.Is 命中哨兵），但 cause 必须保留：
		// 日志要能区分「存储故障」与「令牌不存在」，否则排障只能看到笼统 401。
		if !strings.Contains(err.Error(), "store unavailable") {
			t.Errorf("error = %v，期望保留存储故障 cause", err)
		}
		if rc != nil {
			t.Fatal("令牌解析失败时仍返回了 RelayContext")
		}
	})

	t.Run("Bearer 身份但请求无凭据", func(t *testing.T) {
		store := &stubTokenStore{}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		// 非模型路径下 x-api-key 不生效（§3.1），因此等价于完全没有凭据。
		req.Header.Set("x-api-key", testNKT)

		rc, err := BuildRelayContext(context.Background(), bearerPrincipal, req, store)
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("error = %v，期望 ErrTokenInvalid", err)
		}
		if rc != nil {
			t.Fatal("缺少凭据时仍返回了 RelayContext")
		}
		if len(store.calls) != 0 {
			t.Errorf("Resolve 被调用 %d 次，期望 0 次", len(store.calls))
		}
	})

	t.Run("缺少请求早退", func(t *testing.T) {
		rc, err := BuildRelayContext(context.Background(), bearerPrincipal, nil, &stubTokenStore{})
		if !errors.Is(err, ErrRequestMissing) {
			t.Fatalf("error = %v，期望 ErrRequestMissing", err)
		}
		if rc != nil {
			t.Fatal("缺少请求时仍返回了 RelayContext")
		}
	})

	t.Run("令牌解析结果与身份不一致时拒绝", func(t *testing.T) {
		// 两次解析（鉴权中间件与本函数）必须看到同一令牌/同一用户。
		// 任一侧不一致都意味着凭据被替换或存储返回错误行，分组不得混入。
		cases := map[string]*auth.TokenMeta{
			"令牌 id 不一致": {ID: 10, UserID: 42, Group: "vip"},
			"用户 id 不一致": {ID: 9, UserID: 43, Group: "vip"},
		}
		for name, meta := range cases {
			t.Run(name, func(t *testing.T) {
				store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
					auth.HashTokenKey(testNKT): meta,
				}}
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
				req.Header.Set("Authorization", "Bearer "+testNKT)

				rc, err := BuildRelayContext(context.Background(), bearerPrincipal, req, store)
				if !errors.Is(err, ErrTokenInvalid) {
					t.Fatalf("error = %v，期望 ErrTokenInvalid", err)
				}
				if rc != nil {
					t.Fatal("身份不一致时仍返回了 RelayContext")
				}
			})
		}
	})

	t.Run("非管理员使用渠道 pin 后缀被拒", func(t *testing.T) {
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testLegacy): testMeta(),
		}}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testLegacy+"-5")

		rc, err := BuildRelayContext(context.Background(), bearerPrincipal, req, store)
		if !errors.Is(err, ErrPinForbidden) {
			t.Fatalf("error = %v，期望 ErrPinForbidden（relay-01 §3.5：非管理员 → 403）", err)
		}
		if rc != nil {
			t.Fatal("越权 pin 时仍返回了 RelayContext")
		}
		// 查询必须用剥掉 pin 后缀的 key（relay-01 §3.2 第 3 步），
		// 否则会拿 sk-<key>-<id> 去哈希，永远查不到令牌。
		want := auth.HashTokenKey(testLegacy)
		if len(store.calls) != 1 || store.calls[0] != want {
			t.Errorf("Resolve 入参 = %v，期望 [%s]", store.calls, want)
		}
	})

	t.Run("管理员渠道 pin 可解析且上下文正常构造", func(t *testing.T) {
		admin := &auth.Principal{UserID: 42, Role: "admin", TokenID: &tokenID}
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testLegacy): testMeta(),
		}}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testLegacy+"-5")

		id, ok := ParseChannelPin(req)
		if !ok || id != 5 {
			t.Fatalf("ParseChannelPin() = (%d, %v)，期望 (5, true)", id, ok)
		}
		rc, err := BuildRelayContext(context.Background(), admin, req, store)
		if err != nil {
			t.Fatalf("BuildRelayContext() error = %v，期望 nil", err)
		}
		if rc.TokenID != 9 || rc.UsingGroup != "vip" {
			t.Errorf("TokenID/UsingGroup = %d/%q，期望 9/vip", rc.TokenID, rc.UsingGroup)
		}
	})

	t.Run("非管理员会话携带 pin 后缀也被拒", func(t *testing.T) {
		store := &stubTokenStore{} // 会话路径不需要令牌查询
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		// 路由层的 ParseChannelPin 只看请求头、不看身份来源：即使本请求按
		// Cookie 会话鉴权，这个头也必须在这里拒绝，否则 pin 会绕过 §3.5。
		req.Header.Set("Authorization", "Bearer "+testLegacy+"-5")

		sessionPrincipal := &auth.Principal{UserID: 42, Role: "user"} // TokenID == nil
		rc, err := BuildRelayContext(context.Background(), sessionPrincipal, req, store)
		if !errors.Is(err, ErrPinForbidden) {
			t.Fatalf("error = %v，期望 ErrPinForbidden（会话路径同样受 §3.5 约束）", err)
		}
		if rc != nil {
			t.Fatal("会话越权 pin 时仍返回了 RelayContext")
		}
		if len(store.calls) != 0 {
			t.Errorf("Resolve 被调用 %d 次，期望 0 次", len(store.calls))
		}
	})

	t.Run("管理员会话携带 pin 后缀放行", func(t *testing.T) {
		store := &stubTokenStore{}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testLegacy+"-5")

		admin := &auth.Principal{UserID: 42, Role: "admin"} // TokenID == nil
		rc, err := BuildRelayContext(context.Background(), admin, req, store)
		if err != nil {
			t.Fatalf("BuildRelayContext() error = %v，期望 nil（管理员可用 pin）", err)
		}
		if rc.TokenID != 0 || rc.UsingGroup != "" {
			t.Errorf("TokenID/UsingGroup = %d/%q，期望 0/空（会话路径）", rc.TokenID, rc.UsingGroup)
		}
		if len(store.calls) != 0 {
			t.Errorf("Resolve 被调用 %d 次，期望 0 次", len(store.calls))
		}
	})

	t.Run("非 JSON 请求体不阻断构造且原样恢复", func(t *testing.T) {
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testNKT): testMeta(),
		}}
		body := []byte("this is not json")
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testNKT)

		rc, err := BuildRelayContext(context.Background(), bearerPrincipal, req, store)
		if err != nil {
			t.Fatalf("BuildRelayContext() error = %v，期望 nil（窥视失败不得阻断）", err)
		}
		if rc.Streaming {
			t.Error("Streaming = true，非 JSON 请求体应保持 false")
		}
		got, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("读取恢复后的请求体: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("恢复后的请求体 = %q，期望原样 %q", got, body)
		}
	})

	t.Run("超过窥视上限的请求体恢复后仍完整", func(t *testing.T) {
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testNKT): testMeta(),
		}}
		// 构造一个超过 maxPeekBytes 的合法 JSON：窥视只读前 1 MiB，
		// 恢复时必须把「已读前缀 + 未读余段」拼接回去，否则下游拿到截断体。
		body := append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), maxPeekBytes+1024)...)
		body = append(body, []byte(`"}`)...)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testNKT)

		if _, err := BuildRelayContext(context.Background(), bearerPrincipal, req, store); err != nil {
			t.Fatalf("BuildRelayContext() error = %v，期望 nil", err)
		}
		got, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("读取恢复后的请求体: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("恢复后的请求体长度 = %d，期望 %d（内容不一致）", len(got), len(body))
		}
	})

	t.Run("RequestID 不可预测且互不相同", func(t *testing.T) {
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testNKT): testMeta(),
		}}
		ids := make(map[string]struct{}, 2)
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+testNKT)
			rc, err := BuildRelayContext(context.Background(), bearerPrincipal, req, store)
			if err != nil {
				t.Fatalf("BuildRelayContext() error = %v", err)
			}
			if len(rc.RequestID) != 32 {
				t.Errorf("RequestID = %q（长度 %d），期望 32 位小写十六进制", rc.RequestID, len(rc.RequestID))
			}
			ids[rc.RequestID] = struct{}{}
		}
		if len(ids) != 2 {
			t.Error("两次构造得到相同 RequestID：随机源未生效")
		}
	})
}

// TestParseChannelPin 冻结 relay-01 §3.2/§3.5 的后缀语义：
// 只有 legacy sk- 形态切分，末段纯数字才是渠道 id；nkt- 整段不切分（G7-R51）。
func TestParseChannelPin(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		header string
		apiKey string
		wantID int
		wantOK bool
	}{
		{name: "legacy 后缀", header: "Bearer sk-abc-5", wantID: 5, wantOK: true},
		{name: "legacy 多分隔符取末段", header: "Bearer sk-abc-def-7", wantID: 7, wantOK: true},
		{name: "legacy 无数字后缀", header: "Bearer sk-abc-def", wantOK: false},
		{name: "legacy 空后缀", header: "Bearer sk-abc-", wantOK: false},
		{name: "渠道 id 不得为 0", header: "Bearer sk-abc-0", wantOK: false},
		{name: "nkt 整段不切分", header: "Bearer nkt-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-5", wantOK: false},
		{name: "裸 key 无后缀", header: "sk-abc", wantOK: false},
		{name: "无凭据", wantOK: false},
		{name: "x-api-key 载体（模型路径）", path: "/v1/models/gemini-pro:generateContent", apiKey: "sk-abc-12", wantID: 12, wantOK: true},
		{name: "x-api-key 非模型路径不生效", path: "/v1/chat/completions", apiKey: "sk-abc-12", wantOK: false},
		// Authorization 存在但不是 Bearer 形态时不得吞掉 x-api-key 回退（§3.1 载体表）。
		{name: "非 Bearer Authorization 回退 x-api-key", path: "/v1/models/gemini-pro:generateContent", header: "Basic abc", apiKey: "sk-abc-12", wantID: 12, wantOK: true},
		// Bearer 是规范载体：与 x-api-key 并存时优先。
		{name: "Bearer 优先于 x-api-key", path: "/v1/models/gemini-pro:generateContent", header: "Bearer sk-xyz-9", apiKey: "sk-abc-12", wantID: 9, wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/v1/chat/completions"
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if tc.apiKey != "" {
				req.Header.Set("x-api-key", tc.apiKey)
			}
			id, ok := ParseChannelPin(req)
			if id != tc.wantID || ok != tc.wantOK {
				t.Errorf("ParseChannelPin() = (%d, %v)，期望 (%d, %v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestDetectRawFormat 冻结归一化前的路径判定顺序：
// /v1beta/openai/* 是 Gemini 网关上的 OpenAI 兼容路由，必须按 OpenAI 而非 Gemini。
func TestDetectRawFormat(t *testing.T) {
	cases := []struct {
		path string
		want contract.RelayFormat
	}{
		{"/v1/chat/completions", FormatOpenAI},
		{"/v1/completions", FormatOpenAI},
		{"/v1/embeddings", FormatOpenAI},
		{"/v1/models", FormatOpenAI},
		{"/v1/messages", FormatClaude},
		{"/v1/responses", FormatOpenAIResponses},
		{"/v1beta/openai/chat/completions", FormatOpenAI},
		{"/v1beta/models/gemini-pro:generateContent", FormatGemini},
		{"/v1/models/gemini-pro:embedContent", FormatGemini},
		{"/v1/engines/gemini/embeddings", FormatGemini},
	}
	for _, tc := range cases {
		if got := detectRawFormat(tc.path); got != tc.want {
			t.Errorf("detectRawFormat(%q) = %q，期望 %q", tc.path, got, tc.want)
		}
	}
}

// TestWithFieldWriters 覆盖 R-P1a 的自由函数写权 helper：
// 每个 helper 只写自己那一组字段，审计组是追加而不是覆盖。
func TestWithFieldWriters(t *testing.T) {
	rc := &contract.RelayContext{}
	created := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	WithChannel(rc, ChannelAssignment{
		ChannelID:     7,
		ChannelType:   contract.ChannelTypeOpenAI,
		APIBase:       "https://api.example.com",
		Credential:    "sk-upstream",
		MultiKeyIndex: 2,
		UpstreamModel: "gpt-upstream",
		GroupRatio:    1.5,
		CreatedTime:   created,
	})
	if rc.ChannelID != 7 || rc.ChannelType != contract.ChannelTypeOpenAI ||
		rc.APIBase != "https://api.example.com" || rc.Credential != "sk-upstream" ||
		rc.MultiKeyIndex != 2 || rc.UpstreamModel != "gpt-upstream" ||
		rc.GroupRatio != 1.5 || !rc.CreatedTime.Equal(created) {
		t.Errorf("WithChannel 未完整写入渠道组: %+v", rc)
	}

	req := &contract.Request{Model: "gpt-client", Stream: true}
	WithRequest(rc, req, FormatOpenAI, "/v1/chat/completions")
	if rc.Request != req || rc.OriginModel != "gpt-client" || !rc.Streaming ||
		rc.RawFormat != FormatOpenAI || rc.RequestPath != "/v1/chat/completions" {
		t.Errorf("WithRequest 未完整写入请求组: %+v", rc)
	}

	AppendDiagnostic(rc, contract.DiagnosticEntry{Stage: "a"})
	AppendDiagnostic(rc, contract.DiagnosticEntry{Stage: "b"})
	if len(rc.Diagnostics) != 2 || rc.Diagnostics[0].Stage != "a" || rc.Diagnostics[1].Stage != "b" {
		t.Errorf("AppendDiagnostic 未按追加语义写入: %+v", rc.Diagnostics)
	}
}

// TestBuildRelayContextFiltersAnthropicBeta 锁定 R-W18-05 的契约载体：
// HTTP 边界只把**白名单内**的 anthropic-beta 写入显式字段；多个同名头与
// 单头逗号分隔等价；零值表示不发送。WithRequest 再从 RelayContext 单向复制
// 到 Request，归一化层不重新解析请求头。
func TestBuildRelayContextFiltersAnthropicBeta(t *testing.T) {
	tokenID := int64(9)
	principal := &auth.Principal{UserID: 42, Role: "user", TokenID: &tokenID}
	store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
		auth.HashTokenKey(testNKT): testMeta(),
	}}

	t.Run("allowlisted values survive", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-x"}`))
		req.Header.Set("Authorization", "Bearer "+testNKT)
		req.Header.Add("anthropic-beta", "interleaved-thinking-2025-05-14")
		req.Header.Add("anthropic-beta", "not-a-real-beta, context-1m-2025-08-07")

		rc, err := BuildRelayContext(context.Background(), principal, req, store)
		if err != nil {
			t.Fatalf("BuildRelayContext: %v", err)
		}
		want := "interleaved-thinking-2025-05-14, context-1m-2025-08-07"
		if rc.AnthropicBeta != want {
			t.Fatalf("AnthropicBeta = %q, want %q", rc.AnthropicBeta, want)
		}
		parsed := &contract.Request{Model: "claude-x"}
		WithRequest(rc, parsed, FormatClaude, "/v1/messages")
		if parsed.AnthropicBeta != want {
			t.Fatalf("Request.AnthropicBeta = %q, want %q", parsed.AnthropicBeta, want)
		}
	})

	t.Run("unknown values are dropped to empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-x"}`))
		req.Header.Set("Authorization", "Bearer "+testNKT)
		req.Header.Set("anthropic-beta", "totally-unknown")

		rc, err := BuildRelayContext(context.Background(), principal, req, store)
		if err != nil {
			t.Fatalf("BuildRelayContext: %v", err)
		}
		if rc.AnthropicBeta != "" {
			t.Fatalf("AnthropicBeta = %q，未知值必须丢弃为空", rc.AnthropicBeta)
		}
	})
}

package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/egress"
)

// notify.webhook 发送器的单元验收（D2-12/D2-13/D2-15）：
// 签名固定向量、字节保真、3xx 不跟随、SSRF 守卫、订阅过滤、错误脱敏。

type openSealer struct{}

func (openSealer) Open(sealed string) (string, error) {
	return strings.TrimPrefix(sealed, "sealed:"), nil
}

func webhookConfig(overrides map[string]any) mapConfig {
	cfg := mapConfig{
		KeyWebhookEnabled: true,
		KeyWebhookURL:     "https://hooks.example.com/endpoint",
		KeyWebhookSecret:  "sealed:s3cret",
		KeyWebhookEvents:  []string{"balance_low"},
		// 单元用例使用 httptest / 伪造解析器，主机名不在出厂 allowlist 内；
		// 域名维度在这里放开，地址维度仍由 egress 的 IP 规则验证。
		egress.KeyDomainMode: "all",
	}
	for key, value := range overrides {
		cfg[key] = value
	}
	return cfg
}

// testSender 返回允许 loopback 的发送器（httptest 只能监听 127.0.0.1）。
func testSender(cfg ConfigReader, now time.Time) *WebhookSender {
	sender := NewWebhookSender(cfg, openSealer{})
	sender.AllowPrivateTargets = true
	sender.Now = func() time.Time { return now }
	return sender
}

func TestWebhookSignatureFixedVector(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := testSender(webhookConfig(map[string]any{
		KeyWebhookURL: server.URL + "/hook",
	}), time.Unix(1700000000, 0))
	payload := []byte(`{"event":"test","message":"hello","n":1}`)
	result, err := sender.Send(context.Background(), payload, "balance_low", "evt-1")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !result.OK || result.StatusCode != http.StatusOK {
		t.Fatalf("result = %+v，期望 200/ok", result)
	}
	if gotHeaders.Get(WebhookTimestampHeader) != "1700000000" {
		t.Fatalf("timestamp = %q", gotHeaders.Get(WebhookTimestampHeader))
	}
	// 第三头是 B1a 实现扩展（台账 G-DOM-55），值就是调用方传入的幂等锚。
	if got := gotHeaders.Get(WebhookEventIDHeader); got != "evt-1" {
		t.Fatalf("event id = %q，期望 evt-1", got)
	}
	// 固定向量：HMAC-SHA256("s3cret", "1700000000." + body)，由独立实现算出。
	// 它同时证明 Event-Id 不参与签名——若实现把 eventID 拼进原文字节，
	// 这条向量会立刻失配。
	const want = "sha256=7309f4d898fa7748ec38492ebcd583ea43fadd45e069f17458663bed137fb439"
	if got := gotHeaders.Get(WebhookSignatureHeader); got != want {
		t.Fatalf("signature = %q，期望 %q", got, want)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("发送字节被改写: %q", gotBody)
	}
}

func TestWebhookEventIDHeaderIsNotSigned(t *testing.T) {
	payload := []byte(`{"event":"balance_low"}`)
	var signatures []string
	var eventIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signatures = append(signatures, r.Header.Get(WebhookSignatureHeader))
		eventIDs = append(eventIDs, r.Header.Get(WebhookEventIDHeader))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := testSender(webhookConfig(map[string]any{KeyWebhookURL: server.URL}), time.Unix(1700000000, 0))
	for _, eventID := range []string{"dedupe-a", "dedupe-b"} {
		if _, err := sender.Send(context.Background(), payload, "balance_low", eventID); err != nil {
			t.Fatalf("Send(%s): %v", eventID, err)
		}
	}
	if len(signatures) != 2 || signatures[0] != signatures[1] {
		t.Fatalf("不同 Event-Id 改变了签名：%v", signatures)
	}
	// 签名原文字节仍只有 timestamp + "." + body。
	if want := signForTest("s3cret", "1700000000", payload); signatures[0] != want {
		t.Fatalf("signature = %s，期望 %s", signatures[0], want)
	}
	if eventIDs[0] != "dedupe-a" || eventIDs[1] != "dedupe-b" {
		t.Fatalf("Event-Id 头 = %v", eventIDs)
	}
}

func TestWebhookTestSendOmitsEventIDHeader(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := testSender(webhookConfig(map[string]any{KeyWebhookURL: server.URL}), time.Unix(1700000000, 0))
	if _, err := sender.SendTest(context.Background(), []byte(`{"event":"test"}`)); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	// 测试事件没有投递行：不得伪造一个空/占位幂等锚。
	if _, ok := got[http.CanonicalHeaderKey(WebhookEventIDHeader)]; ok {
		t.Fatalf("SendTest 不应发送 %s：%q", WebhookEventIDHeader, got.Get(WebhookEventIDHeader))
	}
}

func TestWebhookByteFidelity(t *testing.T) {
	// 含多字节字符、重复空格与换行的 payload：任何重新序列化都会改变字节。
	payload := []byte("{\n  \"msg\": \"余额不足 ⚠️\",\n  \"spaces\": \"a  b\",\n  \"n\": 1.0\n}")
	var gotBody []byte
	var gotSignature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSignature = r.Header.Get(WebhookSignatureHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := testSender(webhookConfig(map[string]any{KeyWebhookURL: server.URL}), time.Unix(1700000123, 0))
	if _, err := sender.Send(context.Background(), payload, "balance_low", ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("接收端字节与 payload 不一致:\n got=%q\nwant=%q", gotBody, payload)
	}
	// 用接收到的原始字节复算，必须与头部一致（接收方在任何解析前验签）。
	if want := signForTest("s3cret", "1700000123", gotBody); gotSignature != want {
		t.Fatalf("signature 不覆盖实际发送字节: got=%s want=%s", gotSignature, want)
	}
	// 篡改任意一个字节必须导致验签失败。
	tampered := append([]byte(nil), gotBody...)
	tampered[0] ^= 0x01
	if signForTest("s3cret", "1700000123", tampered) == gotSignature {
		t.Fatal("篡改字节后签名仍然一致")
	}
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var secondHits int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()

	sender := testSender(webhookConfig(map[string]any{KeyWebhookURL: first.URL}), time.Unix(1700000000, 0))
	// 即使注入的客户端默认会跟随重定向，sender 也必须强制不跟随。
	sender.httpClient = &http.Client{Timeout: 2 * time.Second}
	result, err := sender.Send(context.Background(), []byte(`{}`), "balance_low", "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.StatusCode != http.StatusFound || result.OK {
		t.Fatalf("result = %+v，期望 302/失败", result)
	}
	if atomic.LoadInt32(&secondHits) != 0 {
		t.Fatal("3xx 被跟随：重定向目标收到了请求")
	}
}

func TestWebhookBlocksUnsafeTargets(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer server.Close()
	loopbackURL := strings.Replace(server.URL, "http://", "https://", 1)

	cases := []struct {
		name    string
		url     string
		resolve func(context.Context, string) ([]net.IP, error)
	}{
		{name: "loopback", url: loopbackURL},
		{name: "loopback ipv6", url: "https://[::1]/hook"},
		{name: "private 10/8", url: "https://10.1.2.3/hook"},
		{name: "private 192.168/16", url: "https://192.168.1.10/hook"},
		{name: "link-local", url: "https://169.254.169.254/latest/meta-data"},
		{name: "http not allowed", url: "http://hooks.example.com/endpoint"},
		{
			name: "unresolved host",
			url:  "https://does-not-exist.invalid/hook",
			resolve: func(context.Context, string) ([]net.IP, error) {
				return nil, errors.New("no such host")
			},
		},
		{
			name: "host resolving to private",
			url:  "https://rebind.example.com/hook",
			resolve: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("203.0.113.5"), net.ParseIP("10.0.0.8")}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := NewWebhookSender(webhookConfig(map[string]any{KeyWebhookURL: tc.url}), openSealer{})
			sender.Resolve = tc.resolve
			_, err := sender.Send(context.Background(), []byte(`{}`), "balance_low", "")
			if !errors.Is(err, ErrWebhookBlockedTarget) {
				t.Fatalf("err = %v，期望 ErrWebhookBlockedTarget", err)
			}
			if atomic.LoadInt32(&hits) != 0 {
				t.Fatal("被拒绝的目标不应收到任何连接")
			}
		})
	}
}

// TestWebhookResolvesOnceAndDialsValidatedIP 是 D2-12 MEDIUM-1 的回归：
// 解析器第一次给公网、之后给私网（指向本机测试服务器）时，发送必须失败，
// 且解析只能发生一次、测试服务器零命中——证明拨号用的是已校验的 IP，
// 而不是让 DialContext 拿着主机名再做一次可被 rebinding 的解析。
func TestWebhookResolvesOnceAndDialsValidatedIP(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var calls int32
	sender := NewWebhookSender(webhookConfig(map[string]any{
		KeyWebhookURL: "https://rebind.example.com/hook",
	}), openSealer{})
	sender.Timeout = 200 * time.Millisecond
	sender.Resolve = func(context.Context, string) ([]net.IP, error) {
		// 第一次（也是唯一一次允许的）解析：公网地址，可放行。
		if atomic.AddInt32(&calls, 1) == 1 {
			return []net.IP{net.ParseIP("203.0.113.5")}, nil
		}
		// 若实现再次解析，这一次会得到本机测试服务器地址。旧实现会因此
		// 连上 loopback 并“成功”，绕过守卫。
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}

	_, sendErr := sender.Send(context.Background(), []byte(`{}`), "balance_low", "")
	if !errors.Is(sendErr, ErrWebhookTransport) {
		// 公网 IP 无监听 → 传输失败。关键是不能是 ErrWebhookBlockedTarget：
		// 那说明实现又解析了一次并采信了第二次（私网）答案。
		t.Fatalf("err = %v，期望 ErrWebhookTransport", sendErr)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("解析次数 = %d，期望 1（第二次解析就是 rebinding 窗口）", got)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("loopback 测试服务器命中 %d 次，期望 0（不得拨向第二次解析结果）", got)
	}
}

// TestWebhookBlockedAtConnectIsSanitized 是 F1 的回归：拨号期守卫拒绝时，
// 错误必须保留 ErrWebhookBlockedTarget 分类，但**不得**携带底层 *url.Error
// （它会拼出含 query 的完整 URL）。旧实现直接 return err，测试 URL 里的
// token 就随 422 响应体泄露给调用方。
func TestWebhookBlockedAtConnectIsSanitized(t *testing.T) {
	sender := NewWebhookSender(webhookConfig(map[string]any{
		// 不用 AllowPrivateTargets、也不注入客户端：走内置 client + guardedDial，
		// 拨号时解析器给出私网地址，守卫在连接前拒绝。
		KeyWebhookURL: "https://rebind.example.com/hook?token=super-secret-value",
	}), openSealer{})
	sender.Timeout = 2 * time.Second
	sender.Resolve = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.0.0.8")}, nil
	}

	_, err := sender.Send(context.Background(), []byte(`{}`), "balance_low", "")
	if !errors.Is(err, ErrWebhookBlockedTarget) {
		t.Fatalf("err = %v，期望 ErrWebhookBlockedTarget", err)
	}
	for _, leak := range []string{"super-secret-value", "token=", "/hook", "rebind.example.com", "https://"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("拨号期拒绝的错误文本泄露 %q: %v", leak, err)
		}
	}
}

// TestWebhookDialsValidatedIP 是 F2 的补强：用原始 TCP listener 的 accept
// 计数直接证明连接**落在已校验 IP 上**。若实现把主机名交给 DialContext，
// 解析会得到 rebind.example.com 的真实公网地址（或失败），listener 永远
// 收不到连接。AllowPrivateTargets=true 让这次合法的私网解析不被守卫拦下，
// 于是「是否真的拨了 127.0.0.1」成为唯一变量。
func TestWebhookDialsValidatedIP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	var accepted int32
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			atomic.AddInt32(&accepted, 1)
			// 立刻关闭：客户端 TLS 握手失败，测试无需真正跑 TLS。
			_ = conn.Close()
		}
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}

	var calls int32
	sender := NewWebhookSender(webhookConfig(map[string]any{
		KeyWebhookURL: "https://rebind.example.com:" + port + "/hook",
	}), openSealer{})
	sender.AllowPrivateTargets = true
	sender.Timeout = 2 * time.Second
	sender.Resolve = func(context.Context, string) ([]net.IP, error) {
		atomic.AddInt32(&calls, 1)
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}

	// 明文 listener 接不住 TLS 握手 → 传输失败；关键断言是下方的计数。
	if _, sendErr := sender.Send(context.Background(), []byte(`{}`), "balance_low", ""); !errors.Is(sendErr, ErrWebhookTransport) {
		t.Fatalf("err = %v，期望 ErrWebhookTransport（TLS 握手对端不是 TLS）", sendErr)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("解析次数 = %d，期望 1（拨号必须复用这次解析结果）", got)
	}
	if got := atomic.LoadInt32(&accepted); got != 1 {
		t.Fatalf("listener accept 次数 = %d，期望 1（说明 TCP 连接确实拨向了已校验的 127.0.0.1）", got)
	}
}

func TestWebhookSubscriptionFilter(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer server.Close()

	sender := testSender(webhookConfig(map[string]any{
		KeyWebhookURL:    server.URL,
		KeyWebhookEvents: []string{"balance_low", "announcement"},
	}), time.Unix(1700000000, 0))

	if _, err := sender.Send(context.Background(), []byte(`{}`), "payment_success", ""); !errors.Is(err, ErrEventNotSubscribed) {
		t.Fatalf("未订阅事件 err = %v，期望 ErrEventNotSubscribed", err)
	}
	if hits != 0 {
		t.Fatal("未订阅事件不应发起请求")
	}
	if _, err := sender.Send(context.Background(), []byte(`{}`), "announcement", ""); err != nil {
		t.Fatalf("已订阅事件: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d，期望 1", hits)
	}

	// 开关关闭：不发送，且与「未订阅」是不同的哨兵。
	disabled := testSender(webhookConfig(map[string]any{
		KeyWebhookURL:     server.URL,
		KeyWebhookEnabled: false,
	}), time.Unix(1700000000, 0))
	if _, err := disabled.Send(context.Background(), []byte(`{}`), "balance_low", ""); !errors.Is(err, ErrWebhookDisabled) {
		t.Fatalf("disabled err = %v，期望 ErrWebhookDisabled", err)
	}
	if hits != 1 {
		t.Fatal("关闭状态不应发起请求")
	}
}

func TestWebhookTestBypassesEnabledAndSubscription(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sender := testSender(webhookConfig(map[string]any{
		KeyWebhookURL:     server.URL,
		KeyWebhookEnabled: false,
		KeyWebhookEvents:  []string{},
	}), time.Unix(1700000000, 0))
	result, err := sender.SendTest(context.Background(), []byte(`{"event":"test"}`))
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if result.StatusCode != http.StatusAccepted || !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if hits != 1 {
		t.Fatal("测试发送不受 enabled/events 影响")
	}
}

func TestWebhookConfiguredIgnoresEnabled(t *testing.T) {
	cfg := webhookConfig(map[string]any{KeyWebhookEnabled: false})
	if !NewWebhookSender(cfg, openSealer{}).Configured(context.Background()) {
		t.Fatal("URL+secret 齐备时 Configured 应为 true（测试端点验证未启用配置）")
	}
	if NewWebhookSender(mapConfig{}, nil).Configured(context.Background()) {
		t.Fatal("空配置不应视为已配置")
	}
	sender := NewWebhookSender(webhookConfig(nil), nil)
	if sender.Configured(context.Background()) {
		t.Fatal("密钥密封但无 SecretOpener 必须 fail-closed")
	}
}

func TestWebhookTransportErrorIsSanitized(t *testing.T) {
	sender := testSender(webhookConfig(map[string]any{
		// 带凭据的 query 不得出现在错误里（url.Error 会拼完整 URL）。
		KeyWebhookURL: "http://127.0.0.1:1/hook?token=super-secret-value",
	}), time.Unix(1700000000, 0))
	sender.Timeout = 200 * time.Millisecond
	_, err := sender.Send(context.Background(), []byte(`{}`), "balance_low", "")
	if !errors.Is(err, ErrWebhookTransport) {
		t.Fatalf("err = %v，期望 ErrWebhookTransport", err)
	}
	for _, leak := range []string{"super-secret-value", "token=", "/hook"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误文本泄露 %q: %v", leak, err)
		}
	}
}

func TestWebhookTimeout(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() {
		close(block)
		server.Close()
	}()

	sender := testSender(webhookConfig(map[string]any{KeyWebhookURL: server.URL}), time.Unix(1700000000, 0))
	sender.Timeout = 50 * time.Millisecond
	started := time.Now()
	_, err := sender.Send(context.Background(), []byte(`{}`), "balance_low", "")
	if !errors.Is(err, ErrWebhookTransport) {
		t.Fatalf("err = %v，期望 ErrWebhookTransport", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("超时未生效：%v", elapsed)
	}
}

func TestWebhookTargetHost(t *testing.T) {
	sender := NewWebhookSender(webhookConfig(map[string]any{
		KeyWebhookURL: "https://user:pass@hooks.example.com:8443/path?x=1#f",
	}), openSealer{})
	if host := sender.TargetHost(); host != "hooks.example.com" {
		t.Fatalf("TargetHost = %q，期望仅主机名", host)
	}
	if sender := NewWebhookSender(mapConfig{}, nil); sender.TargetHost() != "" {
		t.Fatal("未配置时应返回空主机名")
	}
}

// D2-15：Webhook 不是模板渠道，模板注册表不得认识它。
func TestWebhookIsNotATemplateChannel(t *testing.T) {
	if _, ok := ParseChannel("webhook"); ok {
		t.Fatal("ParseChannel(webhook) 必须为 false：webhook 只在投递记录里出现")
	}
	if _, ok := LookupTemplate("billing.balance_low", ChannelWebhook); ok {
		t.Fatal("模板注册表不得命中 webhook 载体")
	}
	if ChannelWebhook != "webhook" {
		t.Fatalf("投递记录 channel 枚举必须仍是 webhook，实际 %q", ChannelWebhook)
	}
}

func TestWebhookSettingsMalformedEventsFailClosed(t *testing.T) {
	bad := mapConfig{
		KeyWebhookEnabled: true,
		KeyWebhookURL:     "https://hooks.example.com/endpoint",
		KeyWebhookSecret:  "sealed:s3cret",
		KeyWebhookEvents:  json.RawMessage(`"not-an-array"`),
	}
	// mapConfig 会对 RawMessage 再次 Marshal，这里直接构造 raw 读取器。
	sender := NewWebhookSender(rawConfig(bad), openSealer{})
	if sender.Subscribed("balance_low") {
		t.Fatal("损坏的 events 必须 fail-closed（不命中任何事件）")
	}
}

// rawConfig 直接返回原始 JSON，用于构造 mapConfig 表达不了的损坏值。
type rawConfig map[string]any

func (r rawConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := r[key]
	if !ok {
		return nil, false
	}
	switch typed := value.(type) {
	case json.RawMessage:
		return typed, true
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		return raw, true
	}
}

func signForTest(secret, timestamp string, body []byte) string {
	// 与生产代码同一构造（不引入第二套实现差异），仅用于断言。
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

package channelhealth

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
)

// enabledSettings 构造「开关打开 + 指定关键词」的判定设置。
// 关键词走与生产相同的 normalize 路径（trim + lowercase）。
func enabledSettings(keywords ...string) DecisionSettings {
	return DecisionSettings{
		AutoDisableEnabled: true,
		DisableKeywords:    normalizeKeywords(keywords),
	}
}

// TestDecideFailureGlobalSwitchOff：全局开关默认 false（R-M4-01 C）。
// 即使命中鉴权错误/关键词，也必须不判定禁用。
func TestDecideFailureGlobalSwitchOff(t *testing.T) {
	settings := DecisionSettings{
		AutoDisableEnabled: false,
		DisableKeywords:    normalizeKeywords([]string{"quota"}),
	}
	cases := []struct {
		name   string
		class  contract.ErrorClass
		status int
		body   string
	}{
		{"auth-401", contract.ErrUpstreamAuth, 401, ""},
		{"auth-403", contract.ErrUpstreamAuth, 403, ""},
		{"keyword-500", contract.ErrUpstreamServer, 500, "You exceeded your current quota"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if decision, ok := DecideFailure(tc.class, tc.status, []byte(tc.body), true, settings); ok {
				t.Fatalf("全局开关关闭时仍判定禁用: %+v", decision)
			}
		})
	}
}

// TestDecideFailureAutoBanFalse：渠道级 auto_ban=false 不禁（R-M4-01 C）。
func TestDecideFailureAutoBanFalse(t *testing.T) {
	settings := enabledSettings("quota")
	cases := []struct {
		name   string
		class  contract.ErrorClass
		status int
		body   string
	}{
		{"auth", contract.ErrUpstreamAuth, 401, ""},
		{"keyword", contract.ErrUpstreamServer, 500, "You exceeded your current quota"},
		{"channel-error", contract.ErrChannelError, 500, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if decision, ok := DecideFailure(tc.class, tc.status, []byte(tc.body), false, settings); ok {
				t.Fatalf("auto_ban=false 时仍判定禁用: %+v", decision)
			}
		})
	}
}

// TestDecideFailureAuthStatuses：401/403 → channel_auth_error。
// 分类相同但状态不是 401/403 时不得仅凭分类禁用（防止判定函数被误用放宽）。
func TestDecideFailureAuthStatuses(t *testing.T) {
	settings := enabledSettings()
	for _, status := range []int{401, 403} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			decision, ok := DecideFailure(contract.ErrUpstreamAuth, status, nil, true, settings)
			if !ok {
				t.Fatalf("状态 %d 应触发自动禁用", status)
			}
			if decision.ReasonCode != ReasonAuthError || decision.Trigger != TriggerAuth {
				t.Fatalf("decision = %+v, want reason=%s trigger=%s",
					decision, ReasonAuthError, TriggerAuth)
			}
		})
	}
	if decision, ok := DecideFailure(contract.ErrUpstreamAuth, 500, nil, true, settings); ok {
		t.Fatalf("ErrUpstreamAuth + 500 不应触发（变异：放宽到 5xx 必须红）: %+v", decision)
	}
}

// TestDecideFailureChannelError：ErrChannelError → channel_upstream_error
// （当前无生产者，保留接缝）。
func TestDecideFailureChannelError(t *testing.T) {
	decision, ok := DecideFailure(contract.ErrChannelError, 502, nil, true, enabledSettings())
	if !ok {
		t.Fatal("ErrChannelError 应触发自动禁用（接缝）")
	}
	if decision.ReasonCode != ReasonUpstreamError || decision.Trigger != TriggerUpstreamError {
		t.Fatalf("decision = %+v, want reason=%s trigger=%s",
			decision, ReasonUpstreamError, TriggerUpstreamError)
	}
}

// TestDecideFailureKeyword：关键词命中触发，且大小写不敏感（R-M4-12）。
func TestDecideFailureKeyword(t *testing.T) {
	settings := enabledSettings("Your credit balance is too low", "quota exceeded")
	cases := []struct {
		name string
		body string
	}{
		{"exact", `{"error":{"message":"Your credit balance is too low"}}`},
		{"upper", `{"error":{"message":"YOUR CREDIT BALANCE IS TOO LOW"}}`},
		{"mixed", `{"error":{"message":"your Credit Balance Is Too Low"}}`},
		{"second", `Quota Exceeded for this organization`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, ok := DecideFailure(contract.ErrUpstreamServer, 500, []byte(tc.body), true, settings)
			if !ok {
				t.Fatalf("关键词 %q 未命中", tc.body)
			}
			if decision.ReasonCode != ReasonKeyword || decision.Trigger != TriggerKeyword {
				t.Fatalf("decision = %+v, want reason=%s trigger=%s",
					decision, ReasonKeyword, TriggerKeyword)
			}
		})
	}
}

// TestDecideFailureKeywordOnRateLimit：429 + 默认关键词（OpenAI 配额文案）
// 属于关键词触发；429 本身不是触发条件（R-M4-01 C 的「纯 429 不禁」）。
func TestDecideFailureKeywordOnRateLimit(t *testing.T) {
	settings := enabledSettings("You exceeded your current quota")
	console := []byte(`{"error":{"message":"You exceeded your current quota, please check your plan"}}`)
	if _, ok := DecideFailure(contract.ErrUpstreamRateLimit, 429, console, true, settings); !ok {
		t.Fatal("429 + 关键词应触发")
	}
	if decision, ok := DecideFailure(contract.ErrUpstreamRateLimit, 429, []byte(`{"error":"slow down"}`), true, settings); ok {
		t.Fatalf("429 未命中关键词不得触发: %+v", decision)
	}
}

// TestDecideFailurePlainServerErrors：纯 5xx（未命中关键词）不触发。
func TestDecideFailurePlainServerErrors(t *testing.T) {
	settings := enabledSettings("credit balance is too low")
	for _, status := range []int{500, 502, 503, 504, 524} {
		if decision, ok := DecideFailure(contract.ErrUpstreamServer, status, []byte(`{"error":"internal"}`), true, settings); ok {
			t.Fatalf("状态 %d 未命中关键词却触发: %+v", status, decision)
		}
	}
}

// TestDecideFailureExcludedClasses：ErrEndpointUnsupported / ErrChannelConfig /
// 参数类错误一律不禁（R-M4-01 C）。
func TestDecideFailureExcludedClasses(t *testing.T) {
	settings := enabledSettings()
	cases := []struct {
		name   string
		status int
	}{
		{"endpoint-unsupported", 400},
		{"channel-config", 400},
		{"param-invalid", 422},
		{"unknown", 418},
	}
	classes := []contract.ErrorClass{
		contract.ErrEndpointUnsupported,
		contract.ErrChannelConfig,
		contract.ErrParamInvalid,
		contract.ErrUnknown,
	}
	for i, tc := range cases {
		if decision, ok := DecideFailure(classes[i], tc.status, []byte(`{"error":"bad request"}`), true, settings); ok {
			t.Fatalf("%s 不得触发自动禁用: %+v", tc.name, decision)
		}
	}
}

// TestDecideFailureKeywordIgnoredForExcludedClasses：I-1 回归——四类「非渠道
// 健康信号」即使响应体命中关键词也不得触发（R-M4-01 C）。
//
// 典型现实场景：上游把 "Permission denied" 回显在 400/422 的错误体里；
// 若关键词匹配无条件执行，参数错误会被误判成渠道故障并禁用。
func TestDecideFailureKeywordIgnoredForExcludedClasses(t *testing.T) {
	settings := enabledSettings(
		"permission denied",
		"you exceeded your current quota",
		"your credit balance is too low",
	)
	cases := []struct {
		name   string
		class  contract.ErrorClass
		status int
	}{
		{"endpoint-unsupported", contract.ErrEndpointUnsupported, 400},
		{"channel-config", contract.ErrChannelConfig, 400},
		{"param-invalid", contract.ErrParamInvalid, 422},
		{"unknown", contract.ErrUnknown, 418},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"error":"Permission denied: You exceeded your current quota"}`)
			if decision, ok := DecideFailure(tc.class, tc.status, body, true, settings); ok {
				t.Fatalf("%s + 关键词 body 不得触发（I-1: 删早退必红）: %+v", tc.name, decision)
			}
		})
	}
	// 反例：429/5xx 仍保留关键词触发语义（I-1 只排除四类）。
	if _, ok := DecideFailure(contract.ErrUpstreamRateLimit, 429,
		[]byte("You exceeded your current quota"), true, settings); !ok {
		t.Fatal("429 + 关键词仍应触发")
	}
	if _, ok := DecideFailure(contract.ErrUpstreamServer, 500,
		[]byte("Permission denied"), true, settings); !ok {
		t.Fatal("5xx + 关键词仍应触发")
	}
}

// TestDecideFailureBodyUpperBound：关键词只在前 64 KiB 内匹配。
func TestDecideFailureBodyUpperBound(t *testing.T) {
	settings := enabledSettings("disable-me")
	below := strings.Repeat("a", maxDecisionBodyBytes-len("disable-me")) + "disable-me"
	if _, ok := DecideFailure(contract.ErrUpstreamServer, 500, []byte(below), true, settings); !ok {
		t.Fatal("上界内的关键词应命中")
	}
	above := strings.Repeat("a", maxDecisionBodyBytes+16) + "disable-me"
	if decision, ok := DecideFailure(contract.ErrUpstreamServer, 500, []byte(above), true, settings); ok {
		t.Fatalf("上界外的关键词不得命中（变异：去掉截断必红）: %+v", decision)
	}
}

// TestDecideFailureKeywordBeatsNothingButClassWins：分类触发优先于关键词，
// reason 必须反映「命中来源」而不是永远写 keyword。
func TestDecideFailureClassPrecedence(t *testing.T) {
	settings := enabledSettings("quota exceeded")
	decision, ok := DecideFailure(contract.ErrUpstreamAuth, 401,
		[]byte("quota exceeded"), true, settings)
	if !ok || decision.ReasonCode != ReasonAuthError {
		t.Fatalf("401 + 关键词应记 channel_auth_error（分类优先），得到 %+v/%v", decision, ok)
	}
}

// probeSettings 构造「开关打开 + 5s 阈值」的体检设置。
func probeSettings(threshold time.Duration) WorkerSettings {
	return WorkerSettings{
		DecisionSettings:      enabledSettings("quota exceeded"),
		AutoEnableEnabled:     true,
		ResponseTimeThreshold: threshold,
		Interval:              10 * time.Minute,
		Concurrency:           5,
	}
}

// TestDecideProbeFailureTriggers：探测路径的四类触发（鉴权/关键词/阈值）
// 与三类明确不触发（传输/无 adapter/本地配置）。
func TestDecideProbeFailureTriggers(t *testing.T) {
	settings := probeSettings(5 * time.Second)
	cases := []struct {
		name     string
		result   ProbeResult
		autoBan  bool
		wantOK   bool
		wantCode string
	}{
		{
			name:     "auth-401",
			result:   ProbeResult{StatusCode: 401, ErrorClass: contract.ErrUpstreamAuth},
			autoBan:  true,
			wantOK:   true,
			wantCode: ReasonAuthError,
		},
		{
			name:     "keyword-500",
			result:   ProbeResult{StatusCode: 500, ErrorClass: contract.ErrUpstreamServer, Body: []byte("QUOTA EXCEEDED")},
			autoBan:  true,
			wantOK:   true,
			wantCode: ReasonKeyword,
		},
		{
			name:     "slow-success",
			result:   ProbeResult{Success: true, LatencyMS: 5000},
			autoBan:  true,
			wantOK:   true,
			wantCode: ReasonTimeout,
		},
		{
			name:    "fast-success",
			result:  ProbeResult{Success: true, LatencyMS: 4999},
			autoBan: true,
		},
		{
			name:    "transport-timeout",
			result:  ProbeResult{TransportClass: TransportTimeout},
			autoBan: true,
		},
		{
			name:    "unsupported",
			result:  ProbeResult{Unsupported: true},
			autoBan: true,
		},
		{
			name:    "credential-invalid",
			result:  ProbeResult{CredentialInvalid: true},
			autoBan: true,
		},
		{
			name:    "build-failed",
			result:  ProbeResult{BuildFailed: true},
			autoBan: true,
		},
		{
			name:    "auto-ban-false",
			result:  ProbeResult{StatusCode: 401, ErrorClass: contract.ErrUpstreamAuth},
			autoBan: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, ok := DecideProbeFailure(tc.result, tc.autoBan, settings)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v（decision=%+v）", ok, tc.wantOK, decision)
			}
			if tc.wantOK && decision.ReasonCode != tc.wantCode {
				t.Fatalf("reason = %q, want %q", decision.ReasonCode, tc.wantCode)
			}
		})
	}
}

// TestDecideProbeFailureSwitchOff：全局自动禁用关闭时，阈值/鉴权/关键词都不触发。
func TestDecideProbeFailureSwitchOff(t *testing.T) {
	settings := probeSettings(5 * time.Second)
	settings.AutoDisableEnabled = false
	for _, result := range []ProbeResult{
		{Success: true, LatencyMS: 60_000},
		{StatusCode: 401, ErrorClass: contract.ErrUpstreamAuth},
		{StatusCode: 500, ErrorClass: contract.ErrUpstreamServer, Body: []byte("quota exceeded")},
	} {
		if decision, ok := DecideProbeFailure(result, true, settings); ok {
			t.Fatalf("开关关闭仍判定禁用: %+v（result=%+v）", decision, result)
		}
	}
}

// TestProbeSucceededBoundary：健康判定必须与 DecideProbeFailure 的 >= 边界互补
// （阈值处一律判失败）。
func TestProbeSucceededBoundary(t *testing.T) {
	settings := probeSettings(5 * time.Second)
	if ProbeSucceeded(ProbeResult{Success: true, LatencyMS: 4999}, settings) != true {
		t.Fatal("4999ms 应判健康")
	}
	if ProbeSucceeded(ProbeResult{Success: true, LatencyMS: 5000}, settings) != false {
		t.Fatal("5000ms 应判失败（>= 阈值）")
	}
	for _, result := range []ProbeResult{
		{Unsupported: true, Success: true},
		{CredentialInvalid: true, Success: true},
		{BuildFailed: true, Success: true},
		{TransportClass: TransportTimeout},
		{StatusCode: 401, ErrorClass: contract.ErrUpstreamAuth},
	} {
		if ProbeSucceeded(result, settings) {
			t.Fatalf("非健康结果被判定为健康: %+v", result)
		}
	}
}

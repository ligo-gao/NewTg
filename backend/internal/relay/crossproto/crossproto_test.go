package crossproto

// 跨协议转发的纯函数/转换测试（M4 Task 5）。
//
// 覆盖：(a) 协议头解析与未知值；(b) OpenAI 目标保持透传；(c) Anthropic/
// Gemini 上游响应经 IR 回写为 OpenAI 形状（GC-2 双向转换）；(d) GC-6 凭据
// 零回传；(e) 渠道类型 → 协议族映射的边界。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/ir"
)

const testCredential = "sk-ant-very-secret-upstream-key"

func TestCrossProtoParseTargetProtocol(t *testing.T) {
	cases := []struct {
		value string
		want  Protocol
		ok    bool
	}{
		{"openai", ProtocolOpenAI, true},
		{"anthropic", ProtocolAnthropic, true},
		{"gemini", ProtocolGemini, true},
		{"  Anthropic ", ProtocolAnthropic, true},
		{"GEMINI", ProtocolGemini, true},
		{"claude", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, err := ParseTargetProtocol(tc.value)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Fatalf("ParseTargetProtocol(%q) = %q, %v；期望 %q", tc.value, got, err, tc.want)
			}
			continue
		}
		if !errors.Is(err, ErrUnknownProtocol) {
			t.Fatalf("ParseTargetProtocol(%q) 错误 = %v；期望 ErrUnknownProtocol", tc.value, err)
		}
	}
}

func TestCrossProtoTargetFromHeader(t *testing.T) {
	// 空值（含仅空白）= 未携带头：不得视为非法值。
	for _, value := range []string{"", "   ", "\t\n"} {
		protocol, present, err := TargetFromHeader(value)
		if present || err != nil || protocol != "" {
			t.Fatalf("TargetFromHeader(%q) = (%q, %v, %v)；期望未携带", value, protocol, present, err)
		}
	}
	protocol, present, err := TargetFromHeader("anthropic")
	if !present || err != nil || protocol != ProtocolAnthropic {
		t.Fatalf("合法头 = (%q, %v, %v)", protocol, present, err)
	}
	// 携带非法值必须返回 present=true：端点层据此在开关关闭时优先 403，
	// 而不是把「关闭 + 拼错」误判成 400。
	_, present, err = TargetFromHeader("not-a-protocol")
	if !present || !errors.Is(err, ErrUnknownProtocol) {
		t.Fatalf("非法头 = (present=%v, err=%v)；期望 present + ErrUnknownProtocol", present, err)
	}
}

func TestCrossProtoProtocolOfChannelType(t *testing.T) {
	cases := []struct {
		channelType contract.ChannelType
		want        Protocol
		ok          bool
	}{
		{contract.ChannelTypeOpenAI, ProtocolOpenAI, true},
		// Azure 是 OpenAI 兼容变体（AKOpenAI + FallbackTo）。
		{contract.ChannelTypeAzure, ProtocolOpenAI, true},
		{contract.ChannelTypeAnthropic, ProtocolAnthropic, true},
		// Moonshot 走 Anthropic 原生实现（R-01）。
		{contract.ChannelTypeMoonshot, ProtocolAnthropic, true},
		{contract.ChannelTypeGemini, ProtocolGemini, true},
		// 未实现协议族的类型不得被猜成三类中的任何一类。
		{contract.ChannelTypeBaidu, "", false},
		{contract.ChannelTypeVertexAi, "", false},
		{contract.ChannelTypeMidjourney, "", false},
		{contract.ChannelTypeUnknown, "", false},
	}
	for _, tc := range cases {
		got, ok := ProtocolOfChannelType(tc.channelType)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("ProtocolOfChannelType(%q) = (%q, %v)；期望 (%q, %v)",
				tc.channelType, got, ok, tc.want, tc.ok)
		}
		if tc.ok && !ChannelTypeSupportsProtocol(tc.channelType, tc.want) {
			t.Fatalf("ChannelTypeSupportsProtocol(%q, %q) = false", tc.channelType, tc.want)
		}
	}
	if ChannelTypeSupportsProtocol(contract.ChannelTypeAnthropic, ProtocolOpenAI) {
		t.Fatal("anthropic 渠道不得被判定支持 openai 目标协议")
	}
}

func TestForwardOpenAIPassthrough(t *testing.T) {
	raw := `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`
	resp := &contract.Response{ID: "chatcmpl-1", Raw: []byte(raw)}
	out, err := Forward(context.Background(), nil, string(ProtocolOpenAI), resp)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	// 同一指针 + 字节不变：M2 的 OpenAI 透传语义不得被重编码破坏。
	if out != resp || string(out.Raw) != raw {
		t.Fatalf("OpenAI 目标必须原样透传，得到 %p/%s", out, out.Raw)
	}
}

func TestForwardAnthropicResponseToOpenAI(t *testing.T) {
	anthropicBody := []byte(`{
	  "id": "msg_1",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-sonnet-4-5",
	  "content": [{"type": "text", "text": "hi from claude"}],
	  "stop_reason": "end_turn",
	  "usage": {"input_tokens": 4, "output_tokens": 6}
	}`)
	resp, _, err := ir.ParseAnthropicResponse(anthropicBody)
	if err != nil {
		t.Fatalf("ParseAnthropicResponse: %v", err)
	}
	rc := &contract.RelayContext{
		ChannelType: contract.ChannelTypeAnthropic,
		ChannelID:   42,
		Credential:  testCredential,
		APIBase:     "https://api.anthropic.com",
	}
	out, err := Forward(context.Background(), rc, string(ProtocolAnthropic), resp)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}

	var payload struct {
		Object  string `json:"object"`
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out.Raw, &payload); err != nil {
		t.Fatalf("回写结果不是合法 JSON: %v（%s）", err, out.Raw)
	}
	if payload.Object != "chat.completion" || payload.ID != "msg_1" {
		t.Fatalf("object/id = %q/%q", payload.Object, payload.ID)
	}
	if len(payload.Choices) != 1 || payload.Choices[0].Message.Content != "hi from claude" {
		t.Fatalf("choices = %+v", payload.Choices)
	}
	if payload.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q，期望归一化的 stop", payload.Choices[0].FinishReason)
	}
	// 原始 Anthropic 形状不得混入客户端负载。
	if strings.Contains(string(out.Raw), `"stop_reason"`) || strings.Contains(string(out.Raw), `"type":"message"`) {
		t.Fatalf("Anthropic 原生字段泄漏进 OpenAI 响应: %s", out.Raw)
	}
	// 转换链必须追加一条协议转换审计（契约 §5）。
	if len(rc.ConversionChain) != 1 || rc.ConversionChain[0].Stage != "protocol_convert" {
		t.Fatalf("ConversionChain = %+v", rc.ConversionChain)
	}
	if rc.ConversionChain[0].From != ir.FormatAnthropicMessages || rc.ConversionChain[0].To != ir.FormatOpenAIChat {
		t.Fatalf("转换方向 = %+v", rc.ConversionChain[0])
	}
	assertNoCredentialLeak(t, out.Raw)
}

func TestForwardGeminiResponseToOpenAI(t *testing.T) {
	geminiBody := []byte(`{
	  "responseId": "resp_1",
	  "modelVersion": "gemini-2.5-flash",
	  "candidates": [{
	    "index": 0,
	    "content": {"role": "model", "parts": [{"text": "hi from gemini"}]},
	    "finishReason": "STOP"
	  }],
	  "usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 4, "totalTokenCount": 7}
	}`)
	resp, _, err := ir.ParseGeminiResponse(geminiBody)
	if err != nil {
		t.Fatalf("ParseGeminiResponse: %v", err)
	}
	rc := &contract.RelayContext{
		ChannelType: contract.ChannelTypeGemini,
		ChannelID:   43,
		Credential:  testCredential,
		APIBase:     "https://generativelanguage.googleapis.com",
	}
	out, err := Forward(context.Background(), rc, string(ProtocolGemini), resp)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	var payload struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out.Raw, &payload); err != nil {
		t.Fatalf("回写结果不是合法 JSON: %v（%s）", err, out.Raw)
	}
	if payload.Object != "chat.completion" || payload.Model != "gemini-2.5-flash" {
		t.Fatalf("object/model = %q/%q", payload.Object, payload.Model)
	}
	if len(payload.Choices) != 1 || payload.Choices[0].Message.Content != "hi from gemini" ||
		payload.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices = %+v", payload.Choices)
	}
	if strings.Contains(string(out.Raw), `"candidates"`) || strings.Contains(string(out.Raw), `"usageMetadata"`) {
		t.Fatalf("Gemini 原生字段泄漏进 OpenAI 响应: %s", out.Raw)
	}
	if len(rc.ConversionChain) != 1 || rc.ConversionChain[0].From != ir.FormatGeminiGenerateContent {
		t.Fatalf("ConversionChain = %+v", rc.ConversionChain)
	}
	assertNoCredentialLeak(t, out.Raw)
}

func TestForwardRejectsUnknownProtocol(t *testing.T) {
	resp := &contract.Response{ID: "x", Raw: []byte(`{}`)}
	if _, err := Forward(context.Background(), nil, "cohere", resp); !errors.Is(err, ErrUnknownProtocol) {
		t.Fatalf("未知协议错误 = %v；期望 ErrUnknownProtocol", err)
	}
}

func TestForwardNilResponse(t *testing.T) {
	if _, err := Forward(context.Background(), nil, string(ProtocolAnthropic), nil); !errors.Is(err, ErrNilResponse) {
		t.Fatalf("nil 响应错误 = %v；期望 ErrNilResponse", err)
	}
}

func TestForwardContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Forward(ctx, nil, string(ProtocolAnthropic), &contract.Response{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文错误 = %v；期望 context.Canceled", err)
	}
}

// assertNoCredentialLeak 断言 GC-6：上游凭据与 base URL 不得出现在客户端负载里。
func assertNoCredentialLeak(t *testing.T, raw []byte) {
	t.Helper()
	for _, secret := range []string{testCredential, "api.anthropic.com", "generativelanguage.googleapis.com"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("客户端响应包含敏感值 %q: %s", secret, raw)
		}
	}
}

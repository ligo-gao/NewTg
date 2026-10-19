package crossproto

// T2 终验接缝回归（H1 / M5）：原生非流式响应经 IR 回写 OpenAI 客户端时，
// 推理内容必须走 reasoning_content；没有可采信用量时不得合成全零 usage。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
)

func nativeResponse(t *testing.T, adapter contract.Adapter, body string) *contract.Response {
	t.Helper()
	resp, _, err := adapter.ParseResponse(nil, &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	return resp
}

func forwardToOpenAI(t *testing.T, protocol Protocol, resp *contract.Response) map[string]any {
	t.Helper()
	encoded, err := Forward(context.Background(), nil, string(protocol), resp)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded.Raw, &object); err != nil {
		t.Fatalf("Forward 输出不是 JSON: %v", err)
	}
	return object
}

func firstMessage(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	choices, _ := object["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("choices 为空：%v", object)
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return message
}

// TestCrossProtoNativeThinkingNonStream 覆盖 H1：Anthropic thinking 与
// Gemini thought 经非流式跨协议回写后只出现在 reasoning_content。
func TestCrossProtoNativeThinkingNonStream(t *testing.T) {
	anthropicBody := `{"id":"m","model":"claude","content":[
	  {"type":"thinking","thinking":"claude cot","signature":"sig"},
	  {"type":"text","text":"claude answer"}
	],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`
	anthropicResp := nativeResponse(t, adapters.NewAnthropicAdapter(contract.ChannelTypeAnthropic), anthropicBody)
	message := firstMessage(t, forwardToOpenAI(t, ProtocolAnthropic, anthropicResp))
	if message["content"] != "claude answer" {
		t.Fatalf("Claude content 混入 thinking：%v", message["content"])
	}
	if message["reasoning_content"] != "claude cot" {
		t.Fatalf("Claude thinking 未走 reasoning_content：%v", message)
	}

	geminiBody := `{"responseId":"r","modelVersion":"gemini-2.5-flash","candidates":[{
	  "index":0,
	  "content":{"role":"model","parts":[{"thought":true,"text":"gemini cot"},{"text":"gemini answer"}]},
	  "finishReason":"STOP"
	}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`
	geminiResp := nativeResponse(t, adapters.NewGeminiAdapter(contract.ChannelTypeGemini), geminiBody)
	message = firstMessage(t, forwardToOpenAI(t, ProtocolGemini, geminiResp))
	if message["content"] != "gemini answer" {
		t.Fatalf("Gemini content 混入 CoT：%v", message["content"])
	}
	if message["reasoning_content"] != "gemini cot" {
		t.Fatalf("Gemini CoT 未走 reasoning_content：%v", message)
	}
}

// TestCrossProtoUnknownUsageDoesNotSynthesizeZero 覆盖 M5：原生 Raw 里的
// usage 对象没有可识别键时，不得给客户端合成一份全零 usage。
func TestCrossProtoUnknownUsageDoesNotSynthesizeZero(t *testing.T) {
	cases := []struct {
		protocol Protocol
		adapter  contract.Adapter
		body     string
	}{
		{
			protocol: ProtocolAnthropic,
			adapter:  adapters.NewAnthropicAdapter(contract.ChannelTypeAnthropic),
			body:     `{"id":"m","content":[{"type":"text","text":"ok"}],"usage":{"foo":1}}`,
		},
		{
			protocol: ProtocolGemini,
			adapter:  adapters.NewGeminiAdapter(contract.ChannelTypeGemini),
			body:     `{"responseId":"r","candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"foo":1}}`,
		},
	}
	for _, tc := range cases {
		object := forwardToOpenAI(t, tc.protocol, nativeResponse(t, tc.adapter, tc.body))
		if usage, ok := object["usage"]; ok {
			t.Fatalf("%s: 无可信 usage 时不得合成零值：%v", tc.protocol, usage)
		}
	}
}

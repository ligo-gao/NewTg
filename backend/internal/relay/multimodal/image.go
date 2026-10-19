package multimodal

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/usage"
)

// ============================================================================
// 图像端点族（relay-04 §4、§8.1、§9）
//
// 常量集中在本文件的唯一所有者处；httpapi 的入口校验通过别名引用同一组值，
// 禁止两处各写一份（relay-04 §9 通则）。
// ============================================================================

const (
	// MaxImageN 是图像张数上界（契约 §10.1 冻结 128）。
	MaxImageN = 128
	// MaxImageSidePixels / MaxImagePixels 是 size 的单边/总像素上界。
	MaxImageSidePixels = 8192
	MaxImagePixels     = 67_108_864
	// MaxImagePromptBytes 是 prompt 的入口防御上界（relay-04 未冻结数值）。
	MaxImagePromptBytes = 64 << 10
	// MaxPartialImages 是流式图像事件数的间接乘数上界。
	MaxPartialImages = 8
	// MaxImageEditInputBytes 是编辑端点输入图片的字节上界（security-04 §3.2，
	// 25 MiB）。它是「输入」上限，与 MaxImageN（张数）无关。
	MaxImageEditInputBytes = 25 << 20
)

// imageUsageEnvelope 是渠道专用上游图像计数的承载字段（relay-04 §8.1.6）：
// Ali/Bailian 的 usage.image_count → usage.output_image_count。
type imageUsageEnvelope struct {
	Usage struct {
		ImageCount       json.RawMessage `json:"image_count"`
		OutputImageCount json.RawMessage `json:"output_image_count"`
	} `json:"usage"`
}

// CountImages 实现 contract.ImageAdapter（relay-04 §8.1.1 的四形态 payload 推导）。
//
// 直接复用 T3 的 ExtractUsageMultimodal 图像计数逻辑，确保「绝不数 data.#、
// 只认非空 url/b64_json、拆分形取 max」只有一份实现：本方法只负责区分
// 「响应没有 data 键」与「有 data 但解析失败」两种 ok=false 语义。
func (a *endpointAdapter) CountImages(payload []byte) (int, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, false
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return 0, false
	}
	if _, ok := envelope["data"]; !ok {
		// 没有 data 键就不是 OpenAI 图像响应（对话/嵌入/错误体），
		// 不能按「0 张」计，交给下一级信任顺序。
		return 0, false
	}
	fact := usage.ExtractUsageMultimodal(&contract.Response{Raw: trimmed}, nil, nil, a.channelType)
	if fact.Usage.Source == contract.UsageReservedDefault {
		// 解析异常/越界：ok=false，计费会话保持预留量（§8.1.2）。
		return 0, false
	}
	return fact.ImageCount, true
}

// ImageUsage 实现 contract.ChannelImageUsageAdapter（信任顺序第 ① 步）。
//
// 只读渠道专用的有界上游计数字段；值必须是 [0, MaxImageN] 的整数，否则
// 视为不可用并落到 payload 推导（契约 §9.2）。
//
// 读取 body 后会把它复原：调用方（dispatcher）还要用同一份响应体推导
// payload 张数与透传给客户端。resp.Body 是每尝试独占的，复原不引入并发风险。
func (a *endpointAdapter) ImageUsage(_ *contract.RelayContext, resp *http.Response) (int, bool) {
	if resp == nil || resp.Body == nil {
		return 0, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	if err != nil || int64(len(body)) > maxResponseBodyBytes {
		return 0, false
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	var envelope imageUsageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, false
	}
	for _, raw := range []json.RawMessage{envelope.Usage.ImageCount, envelope.Usage.OutputImageCount} {
		if count, ok := boundedImageCount(raw); ok {
			return count, true
		}
	}
	return 0, false
}

// boundedImageCount 把 JSON 数值严格转成 [0, MaxImageN] 的整数。
// 负数/小数/字符串/越界一律不可用（饱和处理只适用于上游「测得」的量，
// 图像张数是计费乘数，越界意味着响应不可信）。
func boundedImageCount(raw json.RawMessage) (int, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || trimmed[0] == '"' {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return 0, false
	}
	if value < 0 || value > MaxImageN || value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}

// imageCompletedEvent 判断一个 SSE 事件是否为「图像完成」事件
// （relay-04 §8.1.3：真实 SSE 只数 completed 事件）。
func imageCompletedEvent(payload []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return false
	}
	return strings.HasSuffix(event.Type, "image_generation.completed") ||
		strings.HasSuffix(event.Type, "image_edit.completed")
}

// imagePayloadItems 从 `data`（对象或数组）中取出所有**有 payload** 的条目。
// 无 payload 条目（仅 revised_prompt 等）被跳过（§8.1.1/§8.1.4）。
func imagePayloadItems(payload []byte) ([]json.RawMessage, bool) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, false
	}
	trimmed := bytes.TrimSpace(envelope.Data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, false
	}
	switch trimmed[0] {
	case '{':
		if imageItemHasPayload(trimmed) {
			return []json.RawMessage{trimmed}, true
		}
		return nil, true
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, false
		}
		out := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			if imageItemHasPayload(item) {
				out = append(out, item)
			}
		}
		return out, true
	default:
		return nil, false
	}
}

func imageItemHasPayload(item json.RawMessage) bool {
	var fields struct {
		URL     json.RawMessage `json:"url"`
		B64JSON json.RawMessage `json:"b64_json"`
	}
	if err := json.Unmarshal(item, &fields); err != nil {
		return false
	}
	return nonEmptyJSONString(fields.URL) || nonEmptyJSONString(fields.B64JSON)
}

func nonEmptyJSONString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false
	}
	return strings.TrimSpace(value) != ""
}

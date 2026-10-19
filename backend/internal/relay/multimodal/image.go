package multimodal

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
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

// validateImagePayloadForUpstream 是适配器的本地二次校验（F9 / CCR-9：
// 透传/multipart 路径必须在适配器本地施加与归一化层同强度的上界）。
// 只校验计费乘数（n/size/quality/partial_images/parameters.n）；不检查
// model/prompt 的业务必填（入口已做），避免适配器与入口文案漂移。
func validateImagePayloadForUpstream(plan *RequestPlan, body []byte) error {
	if plan.multipart {
		parsed, err := parseMultipart(body, plan.contentType, false)
		if err != nil {
			return classified(contract.ErrParamInvalid, "multimodal: invalid multipart image request")
		}
		if raw, present := parsed.fields["n"]; present {
			if _, ok := parseImageN(raw); !ok {
				return classified(contract.ErrParamInvalid, "multimodal: image n out of bounds")
			}
		}
		if size := parsed.fields["size"]; size != "" && !validImageSize(size) {
			return classified(contract.ErrParamInvalid, "multimodal: image size out of bounds")
		}
		if quality := parsed.fields["quality"]; quality != "" && !validImageQuality(quality) {
			return classified(contract.ErrParamInvalid, "multimodal: image quality invalid")
		}
		if raw, present := parsed.fields["parameters"]; present {
			if strings.TrimSpace(raw) == "" {
				return classified(contract.ErrParamInvalid, "multimodal: image parameters invalid")
			}
			if n, useProvider, ok := parseImageParameters(raw); !ok || (useProvider && n == 0) {
				return classified(contract.ErrParamInvalid, "multimodal: image parameters invalid")
			}
		}
		return nil
	}

	var payload struct {
		N             json.RawMessage `json:"n"`
		Size          string          `json:"size"`
		Quality       string          `json:"quality"`
		PartialImages json.RawMessage `json:"partial_images"`
		Parameters    json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return classified(contract.ErrParamInvalid, "multimodal: invalid JSON image request")
	}
	if len(bytes.TrimSpace(payload.N)) > 0 && string(bytes.TrimSpace(payload.N)) != "null" {
		if _, ok := parseImageN(string(payload.N)); !ok {
			return classified(contract.ErrParamInvalid, "multimodal: image n out of bounds")
		}
	}
	if payload.Size != "" && !validImageSize(payload.Size) {
		return classified(contract.ErrParamInvalid, "multimodal: image size out of bounds")
	}
	if payload.Quality != "" && !validImageQuality(payload.Quality) {
		return classified(contract.ErrParamInvalid, "multimodal: image quality invalid")
	}
	if len(bytes.TrimSpace(payload.PartialImages)) > 0 && string(bytes.TrimSpace(payload.PartialImages)) != "null" {
		var value float64
		if err := json.Unmarshal(payload.PartialImages, &value); err != nil || value < 0 || value > MaxPartialImages || value != float64(int(value)) {
			return classified(contract.ErrParamInvalid, "multimodal: partial_images out of bounds")
		}
	}
	if len(bytes.TrimSpace(payload.Parameters)) > 0 && string(bytes.TrimSpace(payload.Parameters)) != "null" {
		if n, useProvider, ok := parseImageParameters(string(payload.Parameters)); !ok || (useProvider && n == 0) {
			return classified(contract.ErrParamInvalid, "multimodal: image parameters invalid")
		}
	}
	return nil
}

// validImageSize 复刻入口校验（relay-04 §9）：auto 或 {w}x{h}，ASCII 小写 x，
// 单边 ≤ MaxImageSidePixels、总像素 ≤ MaxImagePixels。
func validImageSize(size string) bool {
	if size == "auto" {
		return true
	}
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return false
	}
	width, ok := parsePositiveDecimal(parts[0])
	if !ok {
		return false
	}
	height, ok := parsePositiveDecimal(parts[1])
	if !ok {
		return false
	}
	return width <= MaxImageSidePixels && height <= MaxImageSidePixels && width*height <= MaxImagePixels
}

func parsePositiveDecimal(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

func validImageQuality(quality string) bool {
	switch quality {
	case "standard", "hd", "auto", "low", "medium", "high":
		return true
	default:
		return false
	}
}

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

// imagePayloadItems 从 `data`（对象或数组）中取出**有 payload** 的条目，
// 并按 relay-04 §8.1.1 的拆分形规则去重：同一张图分列 url + b64_json 时
// 只产生一个事件（取 max(url 数, b64 数)，而不是求和）。
//
// 返回值：
//   - items：要转成 SSE 完成事件的条目；
//   - valid：data 形状合法；
//   - malformed：存在**类型非法**的 payload 字段（如 url 为数字）。调用方
//     必须按 §8.1.2 回退到 reserved_default（保持预留量），不能按 0 张
//     静默收费。
func imagePayloadItems(payload []byte) (items []json.RawMessage, valid bool, malformed bool) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, false, false
	}
	trimmed := bytes.TrimSpace(envelope.Data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, false, false
	}
	switch trimmed[0] {
	case '{':
		hasPayload, ok := imageItemState(trimmed)
		if !ok {
			return nil, true, true
		}
		if hasPayload {
			return []json.RawMessage{trimmed}, true, false
		}
		return nil, true, false
	case '[':
		var rawItems []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawItems); err != nil {
			return nil, false, false
		}
		urlItems := make([]json.RawMessage, 0, len(rawItems))
		b64Items := make([]json.RawMessage, 0, len(rawItems))
		for _, item := range rawItems {
			var fields struct {
				URL     json.RawMessage `json:"url"`
				B64JSON json.RawMessage `json:"b64_json"`
			}
			if err := json.Unmarshal(item, &fields); err != nil {
				return nil, true, true
			}
			urlPresent, urlValid := imagePayloadField(fields.URL)
			b64Present, b64Valid := imagePayloadField(fields.B64JSON)
			if !urlValid || !b64Valid {
				return nil, true, true
			}
			if urlPresent {
				urlItems = append(urlItems, item)
			}
			if b64Present {
				b64Items = append(b64Items, item)
			}
		}
		// 拆分形去重：优先用 url 条目；b64 条目多于 url 时补足差额。
		out := urlItems
		if len(b64Items) > len(urlItems) {
			out = append(out, b64Items[len(urlItems):]...)
		}
		return out, true, false
	default:
		return nil, false, false
	}
}

// imageItemState 返回对象形条目是否有非空 payload，以及字段类型是否合法。
func imageItemState(item json.RawMessage) (hasPayload, valid bool) {
	var fields struct {
		URL     json.RawMessage `json:"url"`
		B64JSON json.RawMessage `json:"b64_json"`
	}
	if err := json.Unmarshal(item, &fields); err != nil {
		return false, false
	}
	urlPresent, urlValid := imagePayloadField(fields.URL)
	b64Present, b64Valid := imagePayloadField(fields.B64JSON)
	if !urlValid || !b64Valid {
		return false, false
	}
	return urlPresent || b64Present, true
}

// imagePayloadField 判断 url/b64_json 字段是否承载了**非空字符串** payload。
// 返回 (present, valid)：
//   - 键缺失或 null → (false, true)：无 payload 条目，计 0（§8.1.1）；
//   - JSON 字符串 → (非空, true)；
//   - 其它类型（数字/对象/数组）→ (false, false)：payload 与计数不可信，
//     走 §8.1.2 的异常回退，绝不按 0 静默放行。
func imagePayloadField(raw json.RawMessage) (present, valid bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false, true
	}
	if trimmed[0] != '"' {
		return false, false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false, false
	}
	return strings.TrimSpace(value) != "", true
}

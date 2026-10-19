package multimodal

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"path/filepath"
	"strings"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/usage"
)

// ============================================================================
// 音频端点族（relay-04 §3、§8.2、§9）
// ============================================================================

const (
	// MaxTTSTextBytes 是 TTS 输入文本上界（relay-04 §9 冻结 64 KiB）。
	MaxTTSTextBytes = 64 << 10
	// MaxAudioFileBytes 是音频上传文件上界（25 MiB，security-04 §3.2）。
	MaxAudioFileBytes = 25 << 20
	// MaxAudioMetadataBytes 是音频 metadata 的字节上界。
	MaxAudioMetadataBytes = 64 << 10
	// MaxSpeechNewTokens 是 vllm-omini 透传字段 max_new_tokens 的上界。
	MaxSpeechNewTokens = 4096
	// MaxAudioSeconds 是**测得**音频时长的饱和上界（relay-04 §9：3600s）。
	// 与 MaxRequestTaskDurationSeconds 数值巧合但语义无关（后者是用户声明量，
	// 越界 400；前者是上游/媒体测得量，饱和并记 QuotaClamp）。
	MaxAudioSeconds = 3600
	// MinTTSSpeed / MaxTTSSpeed 是 speed 的合法闭区间（甲类修正）。
	MinTTSSpeed = 0.25
	MaxTTSSpeed = 4.0

	// audioDurationPrefixBytes 是流式二进制响应中用于解析文件头的最大前缀。
	// 只保留前缀，不缓存正文（GC-6 零拷贝）。
	audioDurationPrefixBytes = 64 << 10

	// pcmSampleRate / pcmBytesPerSample 是 relay-04 §8.2.3 冻结的 pcm 参数：
	// 24000 Hz / 16 bit / 单声道。
	pcmSampleRate     = 24000
	pcmBytesPerSample = 2
)

// Speech 实现 contract.SpeechAdapter。
//
// 非流式把上游音频字节完整返回；生产路径的 HTTP 响应由 dispatcher 以
// io.Copy 直传，不经过本方法（GC-6）。
func (a *endpointAdapter) Speech(rc *contract.RelayContext, req *contract.Request) (*contract.AudioResponse, contract.Usage, error) {
	resp, body, err := a.exchange(rc, req)
	if err != nil {
		return nil, contract.Usage{}, err
	}
	u := usageForBinaryAudio(a.channelType, body, a.plan.responseFormat)
	result := &contract.AudioResponse{
		Binary:      body,
		ContentType: resp.Header.Get("Content-Type"),
		Usage:       u,
	}
	return result, u, nil
}

// Transcribe 实现 contract.TranscriptionAdapter。
func (a *endpointAdapter) Transcribe(rc *contract.RelayContext, req *contract.Request) (*contract.AudioResponse, contract.Usage, error) {
	return a.transcribeLike(rc, req)
}

// Translate 实现 contract.TranslationAdapter。
func (a *endpointAdapter) Translate(rc *contract.RelayContext, req *contract.Request) (*contract.AudioResponse, contract.Usage, error) {
	return a.transcribeLike(rc, req)
}

func (a *endpointAdapter) transcribeLike(rc *contract.RelayContext, req *contract.Request) (*contract.AudioResponse, contract.Usage, error) {
	resp, body, err := a.exchange(rc, req)
	if err != nil {
		return nil, contract.Usage{}, err
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, contract.Usage{}, classified(contract.ErrUpstreamServer, "multimodal: invalid transcription response")
	}
	// T3 的 applyAudioConversion 会消费响应顶层的 duration（若有）并换算
	// 音频 token；预扣阶段的时长来自上传文件头（见 DurationFromAudio）。
	fact := usage.ExtractUsageMultimodal(&contract.Response{Raw: body}, resp.Header, nil, a.channelType)
	result := &contract.AudioResponse{Text: payload.Text, Usage: fact.Usage}
	return result, fact.Usage, nil
}

// usageForBinaryAudio 由响应音频推导用量事实（relay-04 §8.2.1/§8.2.3）。
//
// 顺序：① 测得的响应音频时长 → pricing 系数换算；② 取不到时长时按
// 「每 KB ≈ 1 token」兜底（乙类怪癖，照搬；Source=estimated）。时长换算
// 复用 T3 的 ExtractUsageMultimodal + pricing.AudioTokensFromSeconds，
// 本包不硬编码「1 分钟 = 1000 token」。
func usageForBinaryAudio(channelType contract.ChannelType, audio []byte, formatHint string) contract.Usage {
	if seconds, ok := AudioDuration(audio, formatHint); ok {
		u := contract.Usage{
			AudioSeconds: pricing.AudioSecondsFromDuration(seconds),
			Source:       contract.UsageEstimated,
		}
		fact := usage.ExtractUsageMultimodal(&contract.Response{Binary: []byte{0}}, nil, &u, channelType)
		return fact.Usage
	}
	return fallbackAudioUsage(len(audio))
}

// fallbackAudioUsage 实现每 KB ≈ 1 token 兜底（relay-04 §8.2.3 乙类怪癖）。
// 只用于「上游既没时长也没 token」的 TTS 响应；0 字节返回空用量。
func fallbackAudioUsage(size int) contract.Usage {
	u := contract.Usage{Source: contract.UsageEstimated}
	if size > 0 {
		tokens := int64(size / 1024)
		if tokens == 0 {
			tokens = 1
		}
		u.CompletionDetail.Audio = tokens
	}
	return u
}

// AudioDuration 从音频字节推导时长（秒）。
//
// 只解析**文件头**（及必要尺寸），不依赖完整解码：
//   - pcm：固定参数 24000 Hz / 16 bit / 单声道（relay-04 §8.2.3）；
//   - wav：RIFF/WAVE 的 fmt.byteRate 与 data 块大小；
//   - mp3：首个帧头的比特率与采样率，按时长 ≈ 字节数×8/比特率；
//   - flac：STREAMINFO 的采样率与总样本数；
//   - m4a/mp4：mvhd 的 timescale/duration。
//
// 其它格式（ogg/opus/webm/aac 等）返回 ok=false，由调用方决定失败（转写）
// 或按 body 大小兜底（TTS）。
func AudioDuration(data []byte, formatHint string) (float64, bool) {
	return AudioDurationWithSize(data, int64(len(data)), formatHint)
}

// SupportsAudioDuration 报告某格式是否能从文件头/尺寸可靠推导时长。
// 转写/翻译的**预扣**依赖上传文件头时长；ogg/aiff/aac 等无法在不解码
// 全文件的情况下测得时长，按 relay-04 §8.2.3 明确失败（400），不得
// 静默按 0 秒计费。
func SupportsAudioDuration(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "pcm", "wav", "flac", "mp3", "m4a", "mp4":
		return true
	default:
		return false
	}
}

// AudioDurationWithSize 用「文件头前缀 + 已知总字节数」推导时长。
//
// 流式直传只保留 prefix（不缓存整段响应），因此按字节率估算的格式
// （mp3 等）必须使用 total，而不是 len(prefix)——否则 1 MiB 的音频会被
// 按 64 KiB 计算，系统性少计（评审 F1）。
func AudioDurationWithSize(data []byte, total int64, formatHint string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(formatHint)) {
	case "pcm":
		if total == 0 {
			return 0, false
		}
		return float64(total) / float64(pcmSampleRate*pcmBytesPerSample), true
	}
	if len(data) < 12 {
		return 0, false
	}
	switch {
	case bytes.HasPrefix(data, []byte("RIFF")) && len(data) >= 44 && string(data[8:12]) == "WAVE":
		return wavDuration(data)
	case bytes.HasPrefix(data, []byte("fLaC")):
		return flacDuration(data)
	case bytes.HasPrefix(data, []byte("ID3")):
		return mp3Duration(data, total, id3TagLength(data))
	case len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return mp3Duration(data, total, 0)
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return mp4Duration(data)
	}
	return 0, false
}

// wavDuration 解析 RIFF/WAVE：时长 = data 块字节数 / fmt.byteRate。
func wavDuration(data []byte) (float64, bool) {
	var byteRate uint32
	offset := 12
	for offset+8 <= len(data) {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		body := offset + 8
		if size < 0 || body > len(data) {
			break
		}
		switch id {
		case "fmt ":
			if size >= 12 && body+12 <= len(data) {
				byteRate = binary.LittleEndian.Uint32(data[body+8 : body+12])
			}
		case "data":
			if byteRate == 0 {
				return 0, false
			}
			// 用 RIFF 头声明的大小而不是实际可读字节数：流式直传只保留了
			// 64 KiB 前缀，但 data 块大小就在头里，声明值与容器总长一致。
			return float64(size) / float64(byteRate), true
		}
		offset = body + size
		// RIFF 块按偶数字节对齐。
		if size%2 == 1 {
			offset++
		}
	}
	return 0, false
}

// flacDuration 解析 STREAMINFO（固定紧跟 fLaC 标记后的第一个块）。
// 采样率 20 bit、总样本数 36 bit（公开 FLAC 容器规格）。
func flacDuration(data []byte) (float64, bool) {
	// fLaC(4) + 块头(4) + STREAMINFO
	if len(data) < 4+4+18 {
		return 0, false
	}
	info := data[8:]
	if len(info) < 18 {
		return 0, false
	}
	sampleRate := int64(info[10])<<12 | int64(info[11])<<4 | int64(info[12])>>4
	totalSamples := int64(info[13]&0x0F)<<32 | int64(info[14])<<24 | int64(info[15])<<16 |
		int64(info[16])<<8 | int64(info[17])
	if sampleRate == 0 || totalSamples == 0 {
		return 0, false
	}
	return float64(totalSamples) / float64(sampleRate), true
}

// mp3Duration 用首帧比特率估算时长（字节数×8/比特率）。
// 这是容器级估算，不依赖逐帧扫描；ID3v2 标签被跳过。
func mp3Duration(data []byte, total int64, start int) (float64, bool) {
	if start >= len(data) {
		return 0, false
	}
	// 找到帧同步 0xFFE。
	offset := start
	for offset+4 <= len(data) {
		if data[offset] == 0xFF && data[offset+1]&0xE0 == 0xE0 {
			break
		}
		offset++
	}
	if offset+4 > len(data) {
		return 0, false
	}
	// MPEG Audio 帧头：比特率索引 bits 12–15，采样率索引 bits 10–11。
	bitrateIndex := int(data[offset+2] >> 4)
	sampleRateIndex := int(data[offset+2]>>2) & 0x03
	// MPEG-1 Layer III 比特率表（kbps）；Layer 差异对本估算影响可忽略。
	bitrates := [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	if bitrateIndex <= 0 || bitrateIndex >= len(bitrates) || sampleRateIndex >= 3 {
		return 0, false
	}
	if bitrates[bitrateIndex] == 0 {
		return 0, false
	}
	// 时长 = 字节数×8 / 比特率（bps）。用 total（已知总长）而不是 prefix
	// 长度：流式直传只保留了文件头（评审 F1）。
	audioBytes := total - int64(offset)
	if audioBytes <= 0 {
		return 0, false
	}
	return float64(audioBytes) * 8 / float64(bitrates[bitrateIndex]*1000), true
}

// id3TagLength 返回 ID3v2 标签总长度（10 字节头 + syncsafe size）。
func id3TagLength(data []byte) int {
	if len(data) < 10 || !bytes.HasPrefix(data, []byte("ID3")) {
		return 0
	}
	size := int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F)
	total := 10 + size
	if total > len(data) {
		return 0
	}
	return total
}

// mp4Duration 解析 mvhd box 的 timescale 与 duration。
func mp4Duration(data []byte) (float64, bool) {
	index := bytes.Index(data, []byte("mvhd"))
	if index < 0 || index+20 > len(data) {
		return 0, false
	}
	payload := data[index+4:]
	version := payload[0]
	if version == 1 {
		if len(payload) < 28 {
			return 0, false
		}
		timescale := binary.BigEndian.Uint32(payload[20:24])
		duration := binary.BigEndian.Uint64(payload[24:32])
		if timescale == 0 {
			return 0, false
		}
		return float64(duration) / float64(timescale), true
	}
	if len(payload) < 16 {
		return 0, false
	}
	timescale := binary.BigEndian.Uint32(payload[12:16])
	duration := binary.BigEndian.Uint32(payload[16:20])
	if timescale == 0 {
		return 0, false
	}
	return float64(duration) / float64(timescale), true
}

// captureReader 在转发的同时保留前 limit 字节（用于解析音频头），
// 不缓存正文：io.Copy 每次只处理一个块（GC-6 零拷贝）。
type captureReader struct {
	reader io.Reader
	limit  int
	prefix bytes.Buffer
}

func newCaptureReader(reader io.Reader, limit int) *captureReader {
	return &captureReader{reader: reader, limit: limit}
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if n > 0 && c.prefix.Len() < c.limit {
		remaining := c.limit - c.prefix.Len()
		if n < remaining {
			remaining = n
		}
		_, _ = c.prefix.Write(p[:remaining])
	}
	return n, err
}

// ResponseFormatFromRequest 读取 TTS 请求体的 response_format，用于时长解析。
// 缺失时返回空串，由 Content-Type 推断。
func ResponseFormatFromRequest(body []byte) string {
	var payload struct {
		ResponseFormat string `json:"response_format"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(payload.ResponseFormat))
}

// formatFromContentType 把上游 Content-Type 映射到时长解析的格式名。
func formatFromContentType(contentType string) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch mediaType {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/flac", "audio/x-flac":
		return "flac"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "m4a"
	case "application/octet-stream":
		return ""
	default:
		return ""
	}
}

// audioSecondsForEstimate 把测得时长转成预扣用的整数秒（向上取整并饱和）。
// 负值/NaN 钳 0；超过 MaxAudioSeconds 时饱和并写入 QuotaClamp 审计标记
// （relay-04 §9 的「测得量饱和不拒请求」口径）。
func audioSecondsForEstimate(rc *contract.RelayContext, data []byte, format string) int64 {
	seconds, ok := AudioDuration(data, format)
	if !ok {
		return 0
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0
	}
	seconds = clampAudioSeconds(rc, seconds)
	return pricing.AudioSecondsFromDuration(seconds)
}

// clampAudioSeconds 对**测得**时长施加 MaxAudioSeconds 上界，并在截断时
// 写入 QuotaClamp（契约 §12 不变量 9：任何饱和都必须可审计）。
func clampAudioSeconds(rc *contract.RelayContext, seconds float64) float64 {
	if seconds <= MaxAudioSeconds {
		return seconds
	}
	if rc != nil {
		relay.WithQuotaClamp(rc, &contract.QuotaClamp{
			Op:       "AudioDuration",
			Kind:     contract.ClampOverflow,
			Original: seconds,
			Clamped:  MaxAudioSeconds,
		})
	}
	return MaxAudioSeconds
}

// validateSpeechPayloadForUpstream 是 TTS 适配器本地二次校验（F6/F9）：
// response_format 枚举、max_new_tokens、metadata 与 speed。入口已做同强度
// 校验；适配器不得因透传/本地改写而放宽（CCR-9）。
func validateSpeechPayloadForUpstream(body []byte) error {
	var payload struct {
		Speed          *float64        `json:"speed"`
		ResponseFormat string          `json:"response_format"`
		MaxNewTokens   json.RawMessage `json:"max_new_tokens"`
		Metadata       json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return classified(contract.ErrParamInvalid, "multimodal: invalid JSON speech request")
	}
	if payload.Speed != nil && (*payload.Speed < MinTTSSpeed || *payload.Speed > MaxTTSSpeed || math.IsNaN(*payload.Speed)) {
		return classified(contract.ErrParamInvalid, "multimodal: speech speed out of bounds")
	}
	if payload.ResponseFormat != "" {
		switch strings.ToLower(strings.TrimSpace(payload.ResponseFormat)) {
		case "mp3", "opus", "aac", "flac", "wav", "pcm":
		default:
			return classified(contract.ErrParamInvalid, "multimodal: invalid speech response_format")
		}
	}
	if trimmed := bytes.TrimSpace(payload.MaxNewTokens); len(trimmed) > 0 && string(trimmed) != "null" {
		var value float64
		if err := json.Unmarshal(trimmed, &value); err != nil || value < 1 || value > MaxSpeechNewTokens || value != float64(int64(value)) {
			return classified(contract.ErrParamInvalid, "multimodal: max_new_tokens out of bounds")
		}
	}
	if len(bytes.TrimSpace(payload.Metadata)) > MaxAudioMetadataBytes {
		return classified(contract.ErrParamInvalid, "multimodal: metadata exceeds the limit")
	}
	return nil
}

// rawAudioDuration 读取转写/翻译响应顶层的原始 duration（秒），用于在
// T3 静默饱和到 maxAudioSeconds 之前判断「是否真的发生了截断」，从而补写
// QuotaClamp 审计标记。
func rawAudioDuration(body []byte) (float64, bool) {
	var payload struct {
		Duration        *float64 `json:"duration"`
		DurationSeconds *float64 `json:"duration_seconds"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false
	}
	value := payload.Duration
	if value == nil {
		value = payload.DurationSeconds
	}
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0 {
		return 0, false
	}
	return *value, true
}

// binaryContentType 返回二进制响应的 Content-Type；上游缺失时按
// response_format 兜底（不猜测未知格式）。
func binaryContentType(upstream string, responseFormat string) string {
	if strings.TrimSpace(upstream) != "" {
		return upstream
	}
	switch responseFormat {
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "flac":
		return "audio/flac"
	case "m4a", "mp4":
		return "audio/mp4"
	case "opus":
		return "audio/opus"
	case "aac":
		return "audio/aac"
	case "pcm":
		return "audio/pcm"
	default:
		return "application/octet-stream"
	}
}

// formatFromFilename 由扩展名 + 声明 MIME 推断音频格式族。
// 三者一致性校验已在 httpapi 入口完成（security-04 §3.2）；这里只取解析口径。
func formatFromFilename(filename, contentType string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mp3":
		return "mp3"
	case ".wav":
		return "wav"
	case ".flac":
		return "flac"
	case ".m4a", ".mp4":
		return "m4a"
	case ".ogg", ".oga", ".opus":
		return "ogg"
	case ".aiff", ".aif", ".aifc":
		return "aiff"
	case ".aac":
		return "aac"
	}
	return formatFromContentType(contentType)
}

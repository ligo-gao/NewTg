package httpapi

// W14-SENSITIVE：把 sec.sensitive.*（modules-08 F-08-18）覆盖到多模态、任务、
// 视频与批次入口。
//
// 冻结口径（W14 任务书）：
//   - 过滤位置：请求体解析之后、PreConsume/上游/落库之前；block → 400
//     `content_blocked`（固定文案，不透露命中词）且零计费/零上游/零写库；
//     action=log → warn 放行；
//   - 只扫描**用户可控文本字段**（字段白名单）；模型名、JSON/工具参数、
//     图片音频二进制与 base64 一律不进入匹配（沿用 sensitive 包的大小写/
//     全半角/`*` 匹配规则）；
//   - 设计只定义 prompt/system 两个作用面。这些入口没有对话 IR，也没有
//     系统指令，因此按 prompt 面处理（任务书第 4 条）。
//
// 为什么过滤发生在入口校验之后：入口校验负责形状检查（缺字段/越界），先
// 得到可解析的请求体才能可靠地提取文本；也保证「非法请求」不会因为词表
// 命中而被错误地报成 content_blocked。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/sensitive"
)

// sensitiveBlockedMessage 是 F-08-18 的固定拒绝文案。chat/messages、公告与
// 本文件的新入口必须逐字一致：命中词与词表内容绝不进入响应体。
const sensitiveBlockedMessage = "request content is blocked by the content policy"

// sensitiveTextFieldNames 是自由 JSON 请求体（任务/视频/批次行/rerank 等）里
// 允许进入匹配的字段名白名单。
//
// 为什么是白名单而不是全量字符串扫描：任务 body 会原样并入插件上下文，里面
// 同时有 model、seconds、size 等路由/计费参数与 image_url/base64 参考图；
// 全量扫描会把参数值、URL 或图片数据误判成命中（W14 冻结口径第 3 条）。
// 字段名沿用上游契约：OpenAI Videos 的 prompt、Anthropic/embeddings 的
// input、rerank 的 query/documents、通用 text/content。
var sensitiveTextFieldNames = map[string]struct{}{
	"prompt":          {},
	"input":           {},
	"messages":        {},
	"text":            {},
	"content":         {},
	"query":           {},
	"documents":       {},
	"negative_prompt": {},
}

// sensitiveBinaryFieldNames 是白名单字段内部仍然必须整棵跳过的子树。
// 典型形态：`input: {"image_url": "data:..."}` 或 document 里的 base64。
// 键名统一小写后比较，避免大小写变体绕过。
var sensitiveBinaryFieldNames = map[string]struct{}{
	"image":     {},
	"image_url": {},
	"images":    {},
	"mask":      {},
	"file":      {},
	"audio":     {},
	"video":     {},
	"base64":    {},
	"b64_json":  {},
	"data_url":  {},
}

// sensitiveConfigFor 读取当前设置快照。cache 为 nil 时使用一次性零值缓存：
// 调用点（测试/降级装配）仍能得到「关闭」语义，而不是 nil panic。
func sensitiveConfigFor(cache *sensitive.Cache, reader ConfigReader) sensitive.Config {
	if cache == nil {
		cache = &sensitive.Cache{}
	}
	return cache.Load(reader)
}

// sensitiveEntriesHit 扫描 prompt 面的多段文本，任一命中即 true。
func sensitiveEntriesHit(cfg sensitive.Config, texts []string) bool {
	return sensitiveEntriesHitSurface(cfg, sensitive.SurfacePrompt, texts)
}

// sensitiveEntriesHitSurface 扫描指定作用面的多段文本，任一命中即 true。
//
// 多模态/透明输入只有 prompt 面；任务/视频/批次的 opaque JSON 在 S1 裁决
// 里需要 system 面兜底，因此把作用面参数化而不是复制一份循环。
func sensitiveEntriesHitSurface(cfg sensitive.Config, surface sensitive.Surface, texts []string) bool {
	if !cfg.Active() {
		return false
	}
	for _, text := range texts {
		if cfg.CheckText(surface, text) {
			return true
		}
	}
	return false
}

// rejectSensitiveTexts 是各入口共用的「命中→日志→拒绝/放行」决策点。
//
// 返回 true 表示已写出 400，调用方必须立即 return。日志只记事件名/动作/
// 作用面/请求与用户 id，不含命中词（F-08-18「不落明文命中词」）。
func rejectSensitiveTexts(w http.ResponseWriter, requestID string, userID int64,
	logger *slog.Logger, cfg sensitive.Config, texts []string) bool {
	if !sensitiveEntriesHit(cfg, texts) {
		return false
	}
	logSensitiveHitEvent(logger, cfg.Action, sensitive.SurfacePrompt, requestID, userID)
	if cfg.Action != sensitive.ActionBlock {
		return false
	}
	writeSensitiveBlocked(w, requestID)
	return true
}

// writeSensitiveBlocked 输出 F-08-18 的固定 400 响应。
// writeSensitiveBlocked 输出 F-08-18 的固定 400 响应（OpenAI 形出口）。
func writeSensitiveBlocked(w http.ResponseWriter, requestID string) {
	writeSensitiveBlockedWith(w, requestID, writeRelayError)
}

// writeSensitiveBlockedWith 让调用方按入站协议选择错误信封（R-W18-04）。
//
// 文案仍是同一常量：Claude 形只改变外层形状，不改「不透露命中词」的语义。
func writeSensitiveBlockedWith(w http.ResponseWriter, requestID string, writeErr relayErrorWriter) {
	writeErr(w, http.StatusBadRequest, requestID, "content_blocked", sensitiveBlockedMessage)
}

// logSensitiveHitEvent 写 security.sensitive_word_hit（warn，F-08-18）。
// 字段集合与 chat/messages 的诊断面一致，便于按 request_id 关联。
func logSensitiveHitEvent(logger *slog.Logger, action sensitive.Action,
	surface sensitive.Surface, requestID string, userID int64) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("security.sensitive_word_hit",
		"event", "security.sensitive_word_hit",
		"action", string(action),
		"surface", string(surface),
		"request_id", requestID,
		"user_id", userID)
}

// ---------- 自由 JSON 请求体 ----------

// collectSensitiveTexts 递归收集白名单字段下的文本。
//
// 只在**每个层级**进入白名单键；因此 `model`/`seconds`/`size`/`role`
// 这类非内容字段不会被扫到，而 `messages[].content`、`input[]`、
// `documents[].text` 这些契约形态仍能被覆盖。命中白名单键后遇到二进制
// 载体会被 sensitiveBinaryFieldNames 整棵跳过。
func collectSensitiveTexts(value any, out *[]string) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			*out = append(*out, typed)
		}
	case []any:
		for _, item := range typed {
			collectSensitiveTexts(item, out)
		}
	case map[string]any:
		for key, child := range typed {
			lower := strings.ToLower(key)
			if _, binary := sensitiveBinaryFieldNames[lower]; binary {
				continue
			}
			if _, allowed := sensitiveTextFieldNames[lower]; !allowed {
				continue
			}
			collectSensitiveTexts(child, out)
		}
	}
}

// taskBodySensitiveTexts 提取任务/视频提交体里的用户文本。
func taskBodySensitiveTexts(body map[string]any) []string {
	var texts []string
	collectSensitiveTexts(body, &texts)
	return texts
}

// taskBodyMessageSensitiveTexts 只收集 messages 子树里的白名单文本。
//
// 为什么单独提取：S1 裁决要求 opaque 插件 JSON 在
// `scope.prompt=false && scope.system=true` 时仍扫描 messages[].content。
// 插件 JSON 里的消息没有可区分的角色语义（既可能是用户提示，也可能是
// 系统指令），无法安全地判定作用面，因此按安全向归入 system 面兜底；
// 其它白名单字段（prompt/input/query/...）仍是透明输入，保持 prompt 面。
func taskBodyMessageSensitiveTexts(body map[string]any) []string {
	var texts []string
	collectMessageSensitiveTexts(body, &texts)
	return texts
}

// collectMessageSensitiveTexts 在任意层级寻找 messages 键，找到后交给
// collectSensitiveTexts 按既有白名单/二进制跳过规则收集文本。
//
// 只把 messages 键作为入口，不会递归扫描任意字符串叶子；二进制子树
// （base64/图片/音频）在进入前整棵跳过，避免 S1 兜底变成新的误伤面。
func collectMessageSensitiveTexts(value any, out *[]string) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectMessageSensitiveTexts(item, out)
		}
	case map[string]any:
		for key, child := range typed {
			lower := strings.ToLower(key)
			if _, binary := sensitiveBinaryFieldNames[lower]; binary {
				continue
			}
			if lower == "messages" {
				collectSensitiveTexts(child, out)
				continue
			}
			collectMessageSensitiveTexts(child, out)
		}
	}
}

// taskBodySensitiveSurface 返回任务/视频/批次 body 的命中作用面。
//
// 常规按 prompt 面处理（S1 裁决一）。prompt 面关闭、system 面开启时，
// messages[].content 仍要扫描（S1 裁决二，安全向兜底），命中记为 system。
// 返回的 Surface 只用于日志，不改变拒绝/放行语义。
func taskBodySensitiveSurface(cfg sensitive.Config, body map[string]any) (sensitive.Surface, bool) {
	if !cfg.Active() {
		return "", false
	}
	if cfg.Scope.Prompt &&
		sensitiveEntriesHitSurface(cfg, sensitive.SurfacePrompt, taskBodySensitiveTexts(body)) {
		return sensitive.SurfacePrompt, true
	}
	if !cfg.Scope.Prompt && cfg.Scope.System &&
		sensitiveEntriesHitSurface(cfg, sensitive.SurfaceSystem, taskBodyMessageSensitiveTexts(body)) {
		return sensitive.SurfaceSystem, true
	}
	return "", false
}

// ---------- 多模态入口 ----------

// multimodalSensitiveTexts 按端点提取入口校验已接受的请求体中的用户文本。
//
// 图片/音频的 multipart 文件 part 一律丢弃（drain），绝不读取内容进入匹配：
// 二进制里随机出现的字节序列不能构成「用户提示词命中」。
func multimodalSensitiveTexts(endpoint contract.Endpoint, path, contentType string, body []byte) []string {
	switch endpoint {
	case contract.EndpointRealtime:
		// WebSocket 升级请求没有可扫描的文本体。
		return nil
	case contract.EndpointImageEdit:
		if isMultipartForm(contentType) {
			return multipartSensitiveTexts(contentType, body, "prompt")
		}
	case contract.EndpointAudioTranscribe, contract.EndpointAudioTranslate:
		// 转写/翻译的可选 prompt 是用户可控文本；file part 不扫描。
		return multipartSensitiveTexts(contentType, body, "prompt")
	case contract.EndpointEmbeddings:
		if _, ok := geminiEmbeddingModel(path); ok {
			// `/v1/engines/{model}/embeddings` 虽然由 GeminiEmbeddingModel 识别，
			// 但它的 body 是 OpenAI 风格 `input`（validateGeminiEnginesJSON），
			// 不能走 Gemini `content.parts` 提取器；落到下面的通用白名单。
			if !strings.HasPrefix(path, "/v1/engines/") {
				return geminiEmbeddingSensitiveTexts(body)
			}
		}
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		// 入口校验已经成功，解析失败只可能来自扫描器与校验器的形状分歧；
		// 不因扫描器自身失败改变请求语义（校验器仍负责拒绝坏请求）。
		// 此分支仅在校验器与扫描器解析口径漂移时可达，语义为放行，
		// 登记为已知假设（M4）。
		return nil
	}
	var texts []string
	collectSensitiveTexts(payload, &texts)
	return texts
}

// geminiEmbeddingSensitiveTexts 提取 Gemini 语义嵌入的用户文本。
//
// 覆盖（M3 裁决）：`content.parts[].text`、`requests[].content.parts[].text`、
// `contents[].parts[].text` 与顶层 `input`。前三个不在通用白名单的层级里
// （parts 不是文本字段名），因此需要专用提取；`input` 是 M3 复核发现的
// 原样出站通道（Gemini 原生路径的 body 不经通用白名单），必须在此补扫。
func geminiEmbeddingSensitiveTexts(body []byte) []string {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	var payload struct {
		Content  content   `json:"content"`
		Contents []content `json:"contents"`
		Requests []struct {
			Content content `json:"content"`
		} `json:"requests"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// 此分支仅在校验器与扫描器解析口径漂移时可达，语义为放行，
		// 登记为已知假设（M4）：入口校验已先成功解析过同一 body。
		return nil
	}
	var texts []string
	appendParts := func(parts []part) {
		for _, item := range parts {
			if item.Text != "" {
				texts = append(texts, item.Text)
			}
		}
	}
	appendParts(payload.Content.Parts)
	for _, request := range payload.Requests {
		appendParts(request.Content.Parts)
	}
	for _, item := range payload.Contents {
		appendParts(item.Parts)
	}
	return append(texts, geminiInputTexts(payload.Input)...)
}

// geminiInputTexts 提取 Gemini 原生路径顶层 input 的三种契约形态：
// string、[]string、[]{text|content}。
//
// 刻意不递归扫描任意字符串叶子（M3 裁决）：input 里同样可能出现
// image/base64 载体，全量扫描会把二进制随机字节误判成命中。无法识别的
// 形态返回空（fail-open），与上面的解析失败分支同一语义。
func geminiInputTexts(raw json.RawMessage) []string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		if text != "" {
			return []string{text}
		}
		return nil
	}
	var list []string
	if err := json.Unmarshal(trimmed, &list); err == nil {
		texts := make([]string, 0, len(list))
		for _, item := range list {
			if item != "" {
				texts = append(texts, item)
			}
		}
		return texts
	}
	var objects []struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(trimmed, &objects); err == nil {
		texts := make([]string, 0, 2*len(objects))
		for _, item := range objects {
			if item.Text != "" {
				texts = append(texts, item.Text)
			}
			if item.Content != "" {
				texts = append(texts, item.Content)
			}
		}
		return texts
	}
	return nil
}

// multipartSensitiveTexts 只读取白名单文本字段；文件 part 与其它字段丢弃。
// 读取量与 handlers_multimodal.go 的入口校验保持同一上界，避免扫描面成为
// 新的内存放大点。
func multipartSensitiveTexts(contentType string, body []byte, fields ...string) []string {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil
	}
	wanted := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		wanted[field] = struct{}{}
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var texts []string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return texts
		}
		if part.FileName() != "" {
			// 图片/音频二进制：只丢弃，不读取内容。
			_, _ = io.Copy(io.Discard, io.LimitReader(part, maxMultipartFieldBytes+1))
			_ = part.Close()
			continue
		}
		if _, ok := wanted[part.FormName()]; !ok {
			_, _ = io.Copy(io.Discard, io.LimitReader(part, maxMultipartFieldBytes+1))
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxMultipartFieldBytes+1))
		_ = part.Close()
		if err != nil {
			return texts
		}
		if len(data) <= maxMultipartFieldBytes {
			texts = append(texts, string(data))
		}
	}
	return texts
}

// ---------- handler 方法 ----------

// rejectSensitiveMultimodal 是 /v1/embeddings、images、rerank、audio 族的
// F-08-18 检查点。调用点固定在入口校验与 RelayContext 之后、限流/白名单/
// 派发（含 PreConsume）之前。
func (h *MultimodalHandler) rejectSensitiveMultimodal(w http.ResponseWriter, requestID string,
	userID int64, endpoint contract.Endpoint, path, contentType string, body []byte) bool {
	cfg := sensitiveConfigFor(h.sensitive, h.config)
	if !cfg.Active() {
		return false
	}
	return rejectSensitiveTexts(w, requestID, userID, nil, cfg,
		multimodalSensitiveTexts(endpoint, path, contentType, body))
}

// rejectSensitiveTaskBody 是 /v1/tasks/{key} 与 /v1/videos 的检查点。
// 两者共用 handleSubmit：视频入口先解析 model 再委托，过滤因此只有一处实现。
func (h *TasksHandler) rejectSensitiveTaskBody(w http.ResponseWriter, requestID string,
	userID int64, body map[string]any) bool {
	cfg := sensitiveConfigFor(h.sensitive, h.config)
	surface, hit := taskBodySensitiveSurface(cfg, body)
	if !hit {
		return false
	}
	logSensitiveHitEvent(nil, cfg.Action, surface, requestID, userID)
	if cfg.Action != sensitive.ActionBlock {
		return false
	}
	writeSensitiveBlocked(w, requestID)
	return true
}

// rejectSensitiveBatchRows 在父批次落库与任何逐行提交之前检查全部行。
//
// 语义选择（设计未定义批量口径，登记于 W14 报告）：任一行 block → **整批
// 400**，与 validateBatchItems 对其它行级参数错误的处理一致，并保证
// 「父批次 + 任务行 + PreConsume」全部为零。action=log 时不在本函数重复
// 扫描——逐行的 handleSubmit 会各自记一次命中日志后放行。
func (h *TasksHandler) rejectSensitiveBatchRows(w http.ResponseWriter, requestID string,
	userID int64, items []batchItemInput) bool {
	cfg := sensitiveConfigFor(h.sensitive, h.config)
	if !cfg.Active() || cfg.Action != sensitive.ActionBlock {
		return false
	}
	for _, item := range items {
		surface, hit := taskBodySensitiveSurface(cfg, item.Body)
		if !hit {
			continue
		}
		logSensitiveHitEvent(nil, cfg.Action, surface, requestID, userID)
		writeSensitiveBlocked(w, requestID)
		return true
	}
	return false
}

package httpapi

import (
	"net/http"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/sensitive"
)

// 本文件由 relay_endpoints.go 按职责拆分（P1-10），仅移动代码，未改动逻辑。

// rejectSensitiveContent 在 IR 解析后、PreConsume/上游之前执行 F-08-18 过滤。
//
// 返回 true 表示已写出 400 响应，调用方必须立即 return。设计口径
// （modules-08 F-08-18）：命中 → 400 + 错误码 content_blocked，**不透露命中词**。
// 因此错误文案固定、日志也不含命中词正文（只记事件名/作用面/请求 id）。
//
// 计费时序是硬约束：本函数必须在 routeDeps（其 Billing 字段会创建计费会话）
// 与 relay.Route 之前调用；block 路径因此零 PreConsume、零上游请求。
// action=log 时只记 warn 日志并放行（设计「记录日志放行」）。
func (h *RelayHandler) rejectSensitiveContent(req *contract.Request, w http.ResponseWriter,
	requestID string, userID int, writeErr relayErrorWriter) bool {
	cfg := h.sensitiveConfig()
	if !cfg.Active() {
		return false
	}
	result := cfg.CheckRequest(req)
	if !result.Matched {
		return false
	}
	h.logSensitiveHit(requestID, userID, result.Surface, cfg.Action)
	if cfg.Action != sensitive.ActionBlock {
		return false
	}
	// 固定文案与 W14 的新入口共用同一个常量/写出函数，避免两处文案漂移。
	// R-W18-04：/v1/messages 传入 Claude 写出器，chat 保持 OpenAI 形。
	writeSensitiveBlockedWith(w, requestID, writeErr)
	return true
}

func (h *RelayHandler) sensitiveConfig() sensitive.Config {
	return sensitiveConfigFor(h.sensitive, h.config)
}

// logSensitiveHit 写 security.sensitive_word_hit（warn，F-08-18）。
//
// 设计要求「不落明文命中词」：这里刻意只记事件名、作用面、动作与请求/用户
// id，不携带任何文本片段。requestID 同时作为 message 之外的独立字段，便于
// 运行日志按请求关联。
func (h *RelayHandler) logSensitiveHit(requestID string, userID int,
	surface sensitive.Surface, action sensitive.Action) {
	logSensitiveHitEvent(h.log, action, surface, requestID, int64(userID))
}

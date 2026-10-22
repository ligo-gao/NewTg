// 任务插件「从 URL 导入」（modules-08 F-08-30 / R-M6D-08 / G-M6D-10）。
//
//	POST /api/plugin/task:import   settings:manage
//
// 请求体：{"url": "https://…/plugin.js", "sha256": "<64 hex，可选>"}
//
// 语义与上传完全一致：服务端编译/校验通过后才入库，失败零写入；入库行
// source=upload、enabled=true、active=false（不自动激活，F-08-30 验收点）。
// 出站是 SSRF 面：仅 https、走 internal/egress 统一守卫、不跟随重定向、
// 包体上限沿用 plugin.upload.max_bytes（默认 2 MiB）。
//
// 为什么单独放在本文件而不是并入 handlers_task_plugins.go：URL 导入的
// 差异只在「取源」这一段（出站/hash），入库路径必须与上传共用 performUpload，
// 避免出现第二条编译/落库实现（S1 的单一关口）。
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/plugin/market"
)

// RegisterTaskPluginImport 挂载 URL 导入端点。
//
// 使用普通 HandlerFunc + handler 内功能级判定（与 POST /api/plugin/task 的
// multipart 上传同构）：鉴权中间件的字段白名单只解析 JSON 对象，而本端点
// 的授权只需 settings:manage，无需为 body 内部字段登记额外策略。
func (h *TaskPluginHandlers) RegisterTaskPluginImport(mux *http.ServeMux) {
	mux.Handle("POST /api/plugin/task:import", http.HandlerFunc(h.handleImport))
}

func (h *TaskPluginHandlers) handleImport(w http.ResponseWriter, r *http.Request) {
	if !h.requireSettingsManage(w, r) {
		return
	}
	var req struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	rawURL := strings.TrimSpace(req.URL)
	if rawURL == "" {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "url is required", nil)
		return
	}
	parsed, err := market.ValidateURL(rawURL)
	if err != nil {
		h.auditImportFailure(r, http.StatusUnprocessableEntity, "url_rejected")
		h.writeMarketFetchError(w, err)
		return
	}
	expectedHash := strings.ToLower(strings.TrimSpace(req.SHA256))
	if expectedHash != "" && !market.IsSHA256Hex(expectedHash) {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "sha256 must be 64 lowercase hexadecimal characters", nil)
		return
	}

	actor := actorID(r)
	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if headerKey == "" {
		writePluginProblem(w, http.StatusBadRequest, pluginProblemInvalidRequest,
			"Invalid request", "Idempotency-Key header is required for plugin import", nil)
		return
	}
	fingerprint := settingsFingerprint([]byte(parsed.String() + "\n" + expectedHash))
	key := fmt.Sprintf("plugin-import:%d:%s %s %s:%s",
		actor, r.Method, r.URL.RequestURI(), fingerprint, headerKey)

	claim, claimErr := h.idem.Claim(r.Context(), key, IdempotencyPendingTTL)
	if claimErr != nil {
		// fail-open：幂等存储故障不阻断导入；编译校验与 (key,version) 唯一
		// 约束仍保证重复请求不会写出错误内容。
		status, body := h.performImport(r, parsed, expectedHash)
		writeJSONBytes(w, status, body)
		return
	}
	switch claim.Status {
	case IdempotencyClaimReplay:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSONBytes(w, claim.Response.Status, claim.Response.Body)
		return
	case IdempotencyClaimInFlight:
		w.Header().Set("Retry-After", "1")
		writeProblemDetail(w, http.StatusConflict, problemTypeIdempotencyInProgress,
			"Idempotency key is in progress",
			"another request with this Idempotency-Key is still being processed; retry shortly")
		return
	}

	status, body := h.performImport(r, parsed, expectedHash)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyFinalizeTimeout)
	defer cancel()
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		if err := h.idem.Complete(ctx, key, claim.Token,
			IdempotentResponse{Status: status, Body: body}, IdempotencyTTL); err != nil {
			h.log.Error("plugin: import idempotency complete failed", "err", err)
		}
	} else if err := h.idem.Release(ctx, key, claim.Token); err != nil {
		h.log.Error("plugin: import idempotency release failed", "err", err)
	}
	writeJSONBytes(w, status, body)
}

// performImport 下载 → 可选 hash 校验 → 复用上传的编译/入库路径。
//
// 任何一步失败都在触碰数据库之前返回（S3 零写入）；成功/编译失败由
// performUpload 统一审计与写响应。
func (h *TaskPluginHandlers) performImport(r *http.Request, parsed *url.URL, expectedHash string) (int, []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), market.DefaultTimeout)
	defer cancel()
	source, err := h.market.FetchSourceBytes(ctx, parsed.String(), h.maxUploadBytes())
	if err != nil {
		status, problemType, title, detail := marketFetchProblem(err)
		h.auditImportFailure(r, status, marketErrorSummary(err))
		return pluginProblemResponse(status, problemType, title, detail)
	}
	if expectedHash != "" {
		sum := sha256.Sum256(source)
		if actual := hex.EncodeToString(sum[:]); actual != expectedHash {
			h.auditImportFailure(r, http.StatusUnprocessableEntity, "source_hash_mismatch")
			return pluginProblemResponse(http.StatusUnprocessableEntity, pluginProblemHashMismatch,
				"Plugin hash mismatch",
				"the downloaded source does not match the provided sha256")
		}
	}
	return h.performUpload(r, actorID(r), string(source))
}

// auditImportFailure 记录导入在「取源」阶段的失败。
//
// 复用 plugin.upload 动作（成功的编译/入库由 performUpload 记录同一动作），
// 用 via=url 与 url_host 区分来源；不记完整 URL（可能带签名 query）与响应体。
func (h *TaskPluginHandlers) auditImportFailure(r *http.Request, status int, reason string) {
	detail := map[string]any{"via": "url", "result_reason": reason}
	if r != nil && r.URL != nil {
		// r.URL 是导入端点自身；出站主机从请求体解析成本更高，这里只保留
		// 端点事实。真正的出站 URL 已在错误分类里脱敏，不入审计。
		detail["endpoint"] = r.URL.Path
	}
	h.writePluginAudit(r, audit.ActionPluginUpload, status, detail)
}

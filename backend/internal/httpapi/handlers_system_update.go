// 版本信息与更新检查（modules-08 §3.15 F-08-42）。
//
//	GET  /api/system/update-check   admin   读缓存；无缓存时服务端抓取一次
//	POST /api/system/update-check   admin   立即检查（绕缓存，60s 最小间隔）
//
// 出站、semver 比较、缓存与 SSRF 策略都在 internal/updatecheck 中实现；
// 本文件只负责 HTTP 映射。响应绝不包含 source_url（可能含内网拓扑/查询串
// 凭据），错误文本也只按固定类别返回。
package httpapi

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"newtg/gateway/internal/updatecheck"
)

const (
	updateCheckProblemType       = "https://newtg.dev/problems/update_check_unavailable"
	updateCheckRateProblemType   = "https://newtg.dev/problems/update_check_rate_limited"
	updateCheckBusyProblemType   = "https://newtg.dev/problems/update_check_in_progress"
	updateCheckUnavailableDetail = "update check is temporarily unavailable"
	updateCheckBusyDetail        = "another instance is checking for updates"
	updateCheckRateLimitedTitle  = "Update check rate limited"
	updateCheckUnavailableTitle  = "Update check unavailable"
	updateCheckInProgressTitle   = "Update check in progress"
)

// UpdateCheckHandlerConfig 是更新检查端点的装配输入。
type UpdateCheckHandlerConfig struct {
	Checker *updatecheck.Checker
	Logger  *slog.Logger
}

// UpdateCheckHandlers 持有更新检查依赖。
type UpdateCheckHandlers struct {
	checker *updatecheck.Checker
	log     *slog.Logger
}

// NewUpdateCheckHandlers 构造端点。Checker 为 nil 时挂载的端点返回 503
// （装配错误不阻断整个管理面启动，但功能不可用是显式的）。
func NewUpdateCheckHandlers(cfg UpdateCheckHandlerConfig) *UpdateCheckHandlers {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &UpdateCheckHandlers{checker: cfg.Checker, log: log}
}

// Register 挂载到受保护子树。权限按设计 §3.15 F-08-42 为 admin 只读；
// POST 会触发出站，但设计仍把它归入同一张卡的 admin 权限（不引入新的
// 权限键；settings:manage 是设置写权限，更新检查不是设置写）。
func (h *UpdateCheckHandlers) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/system/update-check", requireAdminRole(http.HandlerFunc(h.handleGet)))
	mux.Handle("POST /api/system/update-check", requireAdminRole(http.HandlerFunc(h.handlePost)))
}

func (h *UpdateCheckHandlers) handleGet(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, false)
}

func (h *UpdateCheckHandlers) handlePost(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, true)
}

func (h *UpdateCheckHandlers) serve(w http.ResponseWriter, r *http.Request, force bool) {
	if h.checker == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, updateCheckProblemType,
			updateCheckUnavailableTitle, updateCheckUnavailableDetail)
		return
	}

	result, err := h.checker.Check(r.Context(), force)
	if err != nil {
		if limited, ok := updatecheck.IsRateLimit(err); ok {
			seconds := int(math.Ceil(limited.RetryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeProblemDetail(w, http.StatusTooManyRequests, updateCheckRateProblemType,
				updateCheckRateLimitedTitle,
				"try again in "+strconv.Itoa(seconds)+" seconds")
			return
		}
		if errors.Is(err, updatecheck.ErrInProgress) {
			writeProblemDetail(w, http.StatusServiceUnavailable, updateCheckBusyProblemType,
				updateCheckInProgressTitle, updateCheckBusyDetail)
			return
		}
		// 底层错误（可能含 Redis 地址）只进日志，响应固定文案。
		h.log.Error("update check failed", "error", err)
		writeProblemDetail(w, http.StatusServiceUnavailable, updateCheckProblemType,
			updateCheckUnavailableTitle, updateCheckUnavailableDetail)
		return
	}

	// 缓存与检查结果都不应被中间缓存复用：不同管理员的通道/版本上下文
	// 相同，但「立即检查」的 429 与最近检查时刻必须是实时的。
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

// GET /api/logs/by-key —— 按 API 令牌只读查询自身用量明细（modules-05 F-05-11）。
//
// 端点面（D2-01..08 冻结）：
//   - 挂根 mux 的精确模式，绕过 SessionAuth/CSRF/OriginRefererCheck；
//   - 认证只接受 Authorization: Bearer nkt-…，不接受 Cookie / ?key= / x-api-key；
//   - token 级数据范围（user_id + token_id），固定最近 1000 条，无分页/筛选；
//   - other / retry_chain 一律按 user 视图投影，error_message 固定打码；
//   - 限流桶 rateLimit:v2:token:RO:<tokenID> 60 次/分钟，Redis 故障 fail-open。
//
// 为什么不复用 SessionAuth：它按 tokenUsable 拒绝过期/耗尽令牌，而只读面
// 恰恰要求这两态仍可读（modules-05 F-05-11）。也不复用 RelayAuth：中继鉴权
// 会做 IP 白名单/模型限制等准入判定，与「看自己的历史」无关。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/tokenstore"
)

const (
	// logsByKeyLimit 是 F-05-11 冻结的行数上限（默认与上限同为 1000）。
	// 查询参数被忽略，不存在「传 page_size 拿更多」的旁路。
	logsByKeyLimit = 1000

	// logsByKeyRateLimit / Window 是 D2-07 冻结的只读桶口径：60 次/分钟/令牌。
	// 按令牌而不是 IP：脚本通常固定出口 IP，按 IP 会让同一台机器上的多个
	// 令牌互相挤占；按令牌也与「不消耗中继额度」的隔离目标一致。
	logsByKeyRateLimit  = 60
	logsByKeyRateWindow = time.Minute
	logsByKeyRatePrefix = "rateLimit:v2:token:RO:"
)

// logsByKeyPage 是响应信封：只有 items，刻意无 total/page/page_size（D2-04）。
type logsByKeyPage struct {
	Items []logsByKeyItem `json:"items"`
}

// logsByKeyItem = usageLogItem + other，**不含 username**。
// usageLogItem.Username 是 omitempty 且本路径永不赋值，因此回包不会出现该键；
// 也不存在 key/key_masked 字段——tokenstore.Token 的明文与掩码都不进入此投影。
type logsByKeyItem struct {
	usageLogItem
	Other json.RawMessage `json:"other"`
}

// RegisterLogsByKeyRoute 把端点挂到根 mux 的精确模式（与 GET /api/usage/token
// 同挂载方式）。由 routes.go 调用；调用方必须传根 mux，挂到受保护子树会被
// SessionAuth 提前拒绝过期/耗尽令牌，与 F-05-11 冲突。
func (h *UsageLogHandlers) RegisterLogsByKeyRoute(mux *http.ServeMux) {
	mux.Handle("GET /api/logs/by-key", http.HandlerFunc(h.handleLogsByKey))
	// 非 GET 必须 405 而不是落进 /api/ 子树变成 404（D2-06）。根 mux 上还挂着
	// "/api/" 兜底，只有方法限定模式不会匹配 POST；补一条无方法模式，
	// 由 ServeMux 按「更具体的路径优先」选中它。
	mux.HandleFunc("/api/logs/by-key", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "GET")
		writeProblemDetail(w, http.StatusMethodNotAllowed, problemTypeInvalidRequest,
			"Method not allowed", "only GET is supported")
	})
}

func (h *UsageLogHandlers) handleLogsByKey(w http.ResponseWriter, r *http.Request) {
	if h.readOnly == nil {
		// 装配缺失按服务不可用处理，不能降级为匿名——那会变成「谁都能看」。
		h.log.Error("logs by-key: read-only token lookup is not configured")
		writeInvalidTokenUnavailable(w)
		return
	}

	raw, ok := bearerCredential(r)
	if !ok {
		// 缺头/形态不对（含 sk-、Basic、Cookie、?key=）一律同一 401，
		// 不区分原因，避免成为凭据形态与存在性的探测信道。
		writeInvalidToken401(w)
		return
	}

	meta, err := h.readOnly.ResolveReadOnly(r.Context(), auth.HashTokenKey(raw))
	if err != nil {
		switch {
		case errors.Is(err, tokenstore.ErrTokenDisabled), errors.Is(err, tokenstore.ErrInvalidToken):
			// D2-06：禁用与不存在/非法共用 invalid_token，不泄露令牌是否存在。
			writeInvalidToken401(w)
		default:
			// 存储故障固定文案：绝不把 SQL/DSN/host:port 写进响应。
			h.log.Error("logs by-key: resolve token failed", "error", err)
			writeInvalidTokenUnavailable(w)
		}
		return
	}

	// 限流在认证之后、查询之前：未认证请求不消耗令牌的桶（否则攻击者可用
	// 伪造 key 打满某个真实令牌的配额），查询失败也不该白白消耗名额。
	if h.allowLogsByKey(w, r.Context(), meta.ID) {
		return
	}

	// 数据范围是 token 级而不是账户级（modules-02 I-29）：同一用户的另一枚
	// 令牌也互不可见。ORDER BY 与 LIMIT 在 SQL 内完成，不依赖 Go 侧排序。
	query := `SELECT ` + usageListColumns + `, other FROM usage_logs
		WHERE user_id = $1 AND token_id = $2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`
	rows, err := h.logDB.QueryContext(r.Context(), query, meta.UserID, meta.ID, logsByKeyLimit)
	if err != nil {
		h.log.Error("logs by-key: query failed", "error", err, "token_id", meta.ID)
		writeInvalidTokenUnavailable(w)
		return
	}
	defer func() { _ = rows.Close() }()

	items := make([]logsByKeyItem, 0, 64)
	for rows.Next() {
		var row usageLogRow
		if err := row.scanDetail(rows); err != nil {
			h.log.Error("logs by-key: scan failed", "error", err, "token_id", meta.ID)
			writeInvalidTokenUnavailable(w)
			return
		}
		items = append(items, logsByKeyItem{
			// admin=false + revealSensitive=false：持 key 不放宽投影（F-05-11
			// 「比参照侧更严的两处」之一）；error_message 因此恒为 ••••••。
			usageLogItem: row.toItem(false, false),
			Other:        projectOther(row.Other, false, false),
		})
	}
	if err := rows.Err(); err != nil {
		h.log.Error("logs by-key: rows failed", "error", err, "token_id", meta.ID)
		writeInvalidTokenUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, logsByKeyPage{Items: items})
}

// allowLogsByKey 消耗固定窗口名额；返回 true 表示已写出 429 响应。
//
// Redis 故障 fail-open + warn（D2-07）：这是只读排障面，可用性优先；
// 它既不是认证边界也不承载写操作，限流失效只意味着短时放大查询压力。
func (h *UsageLogHandlers) allowLogsByKey(w http.ResponseWriter, ctx context.Context, tokenID int64) bool {
	key := logsByKeyRatePrefix + strconv.FormatInt(tokenID, 10)
	if h.rdb == nil {
		return false
	}
	limited, retry, err := allowRequestRate(ctx, h.rdb, key, logsByKeyRateLimit, logsByKeyRateWindow)
	if err != nil {
		// 用注入的 logger（与同 handler 的其它告警同一输出/字段口径），
		// 不要退回进程级 slog.Default。
		h.log.Warn("logs by-key: rate limiter unavailable; allowing request",
			"token_id", tokenID, "error", err)
		return false
	}
	if !limited {
		return false
	}
	writeRateLimited(w, retry, "Too many requests")
	return true
}

// writeInvalidToken401 是只读面认证失败的唯一出口。
func writeInvalidToken401(w http.ResponseWriter) {
	writeTokenProblem(w, http.StatusUnauthorized, problemTypeInvalidToken, "Invalid token", "", "")
}

// writeInvalidTokenUnavailable 是只读面基础设施故障的唯一出口：固定文案，
// 不带任何底层错误细节。
func writeInvalidTokenUnavailable(w http.ResponseWriter) {
	writeTokenProblem(w, http.StatusServiceUnavailable, problemTypeUnavailable, "Service unavailable", "", "")
}

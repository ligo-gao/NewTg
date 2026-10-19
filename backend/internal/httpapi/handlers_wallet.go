// Package httpapi 的钱包摘要端点（modules-03 §F-03-01）。
//
// 端点面：
//
//	GET /api/wallet/summary   已登录用户：本人余额、冻结、累计消费与健康度
//
// 安全口径：
//   - 用户身份只来自会话 Principal，没有任何 user_id 参数；跨用户读取在
//     接口形状上就不存在，服务端不提供枚举入口。
//   - 余额/冻结/消费在同一主库读取，请求计数来自日志库；任一查询失败返回
//     500，不返回「部分 0」的伪造摘要（前端卡片宁可显示重试）。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"newtg/gateway/internal/auth"
)

// ConfigBalanceAlertThreshold 是低余额阈值的冻结键名（modules-99 H-7）：
// 默认 1,000 积分（¥10），0 表示关闭提醒。
const ConfigBalanceAlertThreshold = "billing.balance_alert_threshold_credits"

const defaultBalanceAlertThreshold int64 = 1000

// WalletHandlerConfig 是钱包摘要端点的装配输入。
type WalletHandlerConfig struct {
	DB *sql.DB
	// LogDB 是 usage_logs 所在日志库；nil 时 total_requests 记为 0（仅测试
	// 缝隙，生产装配恒提供）。
	LogDB  *sql.DB
	Config ConfigReader
	Logger *slog.Logger
}

// WalletHandlers 持有钱包摘要端点的依赖，可并发使用。
type WalletHandlers struct {
	db    *sql.DB
	logDB *sql.DB
	cfg   ConfigReader
	log   *slog.Logger
}

// NewWalletHandlers 构造钱包端点。主库必填：余额是资金读数，不能降级为 0。
func NewWalletHandlers(cfg WalletHandlerConfig) (*WalletHandlers, error) {
	if cfg.DB == nil {
		return nil, errors.New("httpapi: WalletHandlerConfig.DB is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &WalletHandlers{db: cfg.DB, logDB: cfg.LogDB, cfg: cfg.Config, log: logger}, nil
}

// RegisterWalletRoutes 注册已登录用户端点。wrap 语义同其它 Register*Routes。
func (h *WalletHandlers) RegisterWalletRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	var handler http.Handler = http.HandlerFunc(h.handleSummary)
	if wrap != nil {
		handler = wrap(handler)
	}
	mux.Handle("GET /api/wallet/summary", handler)
}

type walletSummaryView struct {
	Balance       int64  `json:"balance"`
	HeldCredits   int64  `json:"held_credits"`
	TotalConsumed int64  `json:"total_consumed"`
	TotalRequests int64  `json:"total_requests"`
	Threshold     int64  `json:"threshold"`
	Health        string `json:"health"`
	Group         string `json:"group"`
}

func (h *WalletHandlers) handleSummary(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return
	}

	summary, err := h.readSummary(r.Context(), p.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 会话指向已删除用户：按未认证处理，不泄露用户是否存在。
			writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
			return
		}
		h.log.Error("wallet summary failed", "user_id", p.UserID, "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// readSummary 读取一个用户的完整摘要。三个主库聚合串行执行：每用户单次
// 请求的量级很小，拆并行只会让连接池抖动；任一失败即整体失败。
func (h *WalletHandlers) readSummary(ctx context.Context, userID int64) (walletSummaryView, error) {
	var view walletSummaryView
	if err := h.db.QueryRowContext(ctx,
		`SELECT balance, "group" FROM users WHERE id = $1`, userID,
	).Scan(&view.Balance, &view.Group); err != nil {
		return walletSummaryView{}, err
	}

	// 冻结中：hold 流水的 amount 为负数（预扣），取相反数得到正积分。
	if err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(-amount), 0) FROM wallet_transactions
		 WHERE user_id = $1 AND status = 'hold'`, userID,
	).Scan(&view.HeldCredits); err != nil {
		return walletSummaryView{}, err
	}

	// 累计消费/请求总数按 modules-03 §F-03-01 的口径来自日志库
	// usage_logs（不是主库流水）；日志库不可用时保持 0（仅测试缝隙），
	// 生产装配恒注入 LogDB。
	if h.logDB != nil {
		if err := h.logDB.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost), 0), count(*) FROM usage_logs
			 WHERE user_id = $1`, userID,
		).Scan(&view.TotalConsumed, &view.TotalRequests); err != nil {
			return walletSummaryView{}, err
		}
	}

	view.Threshold = h.threshold()
	view.Health = walletHealth(view.Balance, view.Threshold)
	return view, nil
}

// threshold 读取站点低余额阈值；缺键/非法值回退默认 1000（配置错误不能
// 让钱包页整体不可用）。
func (h *WalletHandlers) threshold() int64 {
	if h.cfg == nil {
		return defaultBalanceAlertThreshold
	}
	raw, ok := h.cfg.Get(ConfigBalanceAlertThreshold)
	if !ok {
		return defaultBalanceAlertThreshold
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err == nil && value >= 0 {
		return value
	}
	// 配置中心可能以 JSON 字符串存数值（与其它数值键的兼容路径一致）。
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		parsed, parseErr := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if parseErr == nil && parsed >= 0 {
			return parsed
		}
	}
	return defaultBalanceAlertThreshold
}

// walletHealth 是服务端唯一判定口径（前端不重复计算）。
func walletHealth(balance, threshold int64) string {
	if balance == 0 {
		return "depleted"
	}
	if threshold > 0 && balance < threshold {
		return "low"
	}
	return "healthy"
}

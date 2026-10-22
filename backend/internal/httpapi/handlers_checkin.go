// Package httpapi 的每日签到端点（modules-01 §3.19 F-01-30；入账 modules-03 F-03-12）。
//
// 端点面（W15-C 冻结）：
//
//	POST /api/checkin                     已登录用户：签到一次（Turnstile + 限流 + 事务）
//	GET  /api/checkin/calendar?month=YYYY-MM  已登录用户：本人月历与连签/累计
//
// 安全口径：
//   - 身份只来自会话/Bearer Principal，没有任何 user_id 参数；跨用户签到在
//     接口形状上就不存在。
//   - 站点关闭签到（checkin.enabled=false）时两个端点都返回固定的
//     403 `checkin_disabled`，且在**限流/人机校验/事务之前**判定——关闭状态
//     不产生 Redis 键、不调用 siteverify、不写任何行（W15-C 裁决 1）。
//   - 限流（10 次/分钟/用户）在 Turnstile 之前，与既有 4 个接入点的
//     「限流外层 → Turnstile 内层」顺序一致；Redis 故障时资金接口 fail-closed 503。
//   - Turnstile 令牌不写日志；错误只返回固定的 problem type。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
)

const (
	// 签到限流阈值（R-W15-16 推荐的每用户 10/min；设计未冻结具体桶，
	// 这里与兑换码同一「固定窗口 + 用户键」口径，避免资金接口无限放量）。
	checkinRateLimit     = 10
	checkinRateWindow    = time.Minute
	checkinRateKeyPrefix = "newtg:checkin:rate:user:"

	// 请求体上限：签到请求只有一个可选 turnstile_token 字段，1 MiB 足够
	// 覆盖任何客户端包装，同时防止无界读取。
	checkinMaxBody = 1 << 20
)

// RFC 7807 type URI（与全库约定一致）。
const (
	problemTypeCheckinDisabled     = "https://newtg.dev/problems/checkin_disabled"
	problemTypeCheckinAlreadyDone  = "https://newtg.dev/problems/already_checked_in"
	problemTypeWalletLimitExceeded = "https://newtg.dev/problems/wallet_limit_exceeded"
	checkinMonthPattern            = `^\d{4}-(0[1-9]|1[0-2])$`
)

var checkinMonthRE = regexp.MustCompile(checkinMonthPattern)

// CheckinRunner 是签到服务的最小接口（生产 *billing.CheckinService；单测可
// 注入内存实现验证 HTTP 语义而不触真 PG）。
type CheckinRunner interface {
	Enabled() bool
	CheckIn(ctx context.Context, userID int64) (billing.CheckinResult, error)
	Calendar(ctx context.Context, userID int64, month string) (billing.CheckinCalendar, error)
}

// CheckinHandlerConfig 是签到端点的装配输入。
type CheckinHandlerConfig struct {
	Service CheckinRunner
	// Redis 是签到限流桶（必需：资金接口缺限流时不能静默放量）。
	Redis *redis.Client
	// Turnstile 是第 5 个接入点的 gate；nil 表示未配置 Turnstile
	// （与既有 4 点一致：关闭时直接放行）。
	Turnstile *turnstileGates
	Logger    *slog.Logger
	// Now 是限流窗口的时钟接缝；nil 时 time.Now。
	Now func() time.Time
}

// CheckinHandlers 持有签到端点依赖，可并发使用。
type CheckinHandlers struct {
	svc       CheckinRunner
	rdb       *redis.Client
	turnstile *turnstileGates
	log       *slog.Logger
	now       func() time.Time
}

// NewCheckinHandlers 构造签到端点。
func NewCheckinHandlers(cfg CheckinHandlerConfig) (*CheckinHandlers, error) {
	if cfg.Service == nil {
		return nil, errors.New("httpapi: CheckinHandlerConfig.Service is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: CheckinHandlerConfig.Redis is required (checkin rate limit)")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &CheckinHandlers{
		svc:       cfg.Service,
		rdb:       cfg.Redis,
		turnstile: cfg.Turnstile,
		log:       logger,
		now:       now,
	}, nil
}

// RegisterCheckinRoutes 注册签到端点。wrap 语义同其它 Register*Routes。
func (h *CheckinHandlers) RegisterCheckinRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	register := func(pattern string, handler http.HandlerFunc) {
		var hd http.Handler = handler
		if wrap != nil {
			hd = wrap(hd)
		}
		mux.Handle(pattern, hd)
	}
	register("POST /api/checkin", h.handleCheckIn)
	register("GET /api/checkin/calendar", h.handleCalendar)
}

type checkinResultView struct {
	Date           string `json:"date"`
	Awarded        int64  `json:"awarded"`
	Streak         int32  `json:"streak"`
	MonthTotal     int64  `json:"month_total"`
	TotalEarned    int64  `json:"total_earned"`
	BalanceAfter   int64  `json:"balance_after"`
	CheckedInToday bool   `json:"checked_in_today"`
}

type checkinCalendarView struct {
	Month          string               `json:"month"`
	Today          string               `json:"today"`
	Days           []billing.CheckinDay `json:"days"`
	Streak         int32                `json:"streak"`
	MonthTotal     int64                `json:"month_total"`
	TotalEarned    int64                `json:"total_earned"`
	CheckedInToday bool                 `json:"checked_in_today"`
}

func (h *CheckinHandlers) handleCheckIn(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return
	}
	// 关闭态在任何副作用之前判定：不写限流键、不调用 siteverify、不开事务。
	if !h.svc.Enabled() {
		writeProblemDetail(w, http.StatusForbidden, problemTypeCheckinDisabled,
			"Check-in is currently unavailable", "Daily check-in is disabled on this site.")
		return
	}
	if !h.allowCheckin(w, r, p.UserID) {
		return
	}

	var req struct {
		TurnstileToken string `json:"turnstile_token"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, checkinMaxBody+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request")
		return
	}
	if len(raw) > checkinMaxBody {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request")
			return
		}
	}
	// 第 5 个 Turnstile 接入点，固定位于限流之后（modules-01 §3.15）。
	if !h.turnstile.check(auth.TurnstileEndpointCheckin, w, r, req.TurnstileToken) {
		return
	}

	res, err := h.svc.CheckIn(r.Context(), p.UserID)
	if err != nil {
		h.writeCheckinError(w, p.UserID, err)
		return
	}
	if res.AlreadyCheckedIn {
		// 同日第二次：固定 409，零写入（重复抽额被唯一约束/守卫拦截）。
		writeProblemDetail(w, http.StatusConflict, problemTypeCheckinAlreadyDone,
			"Already checked in", "You have already checked in today.")
		return
	}
	writeJSON(w, http.StatusOK, checkinResultView{
		Date:           res.Date,
		Awarded:        res.Awarded,
		Streak:         res.Streak,
		MonthTotal:     res.MonthTotal,
		TotalEarned:    res.TotalEarned,
		BalanceAfter:   res.BalanceAfter,
		CheckedInToday: true,
	})
}

func (h *CheckinHandlers) handleCalendar(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return
	}
	// 关闭态固定拒绝（W15-C 裁决 1）；前端入口由 /api/status 的
	// checkin_enabled 驱动，正常不会走到这里。
	if !h.svc.Enabled() {
		writeProblemDetail(w, http.StatusForbidden, problemTypeCheckinDisabled,
			"Check-in is currently unavailable", "Daily check-in is disabled on this site.")
		return
	}
	month := strings.TrimSpace(r.URL.Query().Get("month"))
	if month != "" && !checkinMonthRE.MatchString(month) {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "month must be in YYYY-MM format.")
		return
	}

	cal, err := h.svc.Calendar(r.Context(), p.UserID, month)
	if err != nil {
		h.writeCheckinError(w, p.UserID, err)
		return
	}
	days := cal.Days
	if days == nil {
		// JSON null 与 [] 在前端语义不同：null 像「未知」，[] 才是
		// 「本月还没有签到记录」。
		days = []billing.CheckinDay{}
	}
	writeJSON(w, http.StatusOK, checkinCalendarView{
		Month:          cal.Month,
		Today:          cal.Today,
		Days:           days,
		Streak:         cal.Streak,
		MonthTotal:     cal.MonthTotal,
		TotalEarned:    cal.TotalEarned,
		CheckedInToday: cal.CheckedInToday,
	})
}

// writeCheckinError 把服务哨兵映射为固定 problem；未知错误只记日志。
func (h *CheckinHandlers) writeCheckinError(w http.ResponseWriter, userID int64, err error) {
	switch {
	case errors.Is(err, billing.ErrCheckinDisabled):
		writeProblemDetail(w, http.StatusForbidden, problemTypeCheckinDisabled,
			"Check-in is currently unavailable", "Daily check-in is disabled on this site.")
	case errors.Is(err, billing.ErrCheckinAlreadyDone):
		writeProblemDetail(w, http.StatusConflict, problemTypeCheckinAlreadyDone,
			"Already checked in", "You have already checked in today.")
	case errors.Is(err, billing.ErrWalletLimit):
		// 钱包安全整数上界：整笔签到（含签到行）已回滚，不做部分入账。
		writeProblemDetail(w, http.StatusConflict, problemTypeWalletLimitExceeded,
			"Wallet balance limit exceeded",
			"The reward cannot be credited because the wallet balance would exceed its limit.")
	case errors.Is(err, billing.ErrUserNotFound):
		// 会话指向已删除用户：按未认证处理，不泄露用户是否存在。
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
	default:
		h.log.Error("checkin failed", "user_id", userID, "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
	}
}

// allowCheckin 执行 10 次/分钟/用户的固定窗口限流。Redis 故障 fail-closed
// （503）：签到是资金入账接口，不能因为限流器不可用而无限放量。
func (h *CheckinHandlers) allowCheckin(w http.ResponseWriter, r *http.Request, userID int64) bool {
	now := h.now()
	bucket := now.Unix() / int64(checkinRateWindow/time.Second)
	key := checkinRateKeyPrefix + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(bucket, 10)

	count, err := h.rdb.Incr(r.Context(), key).Result()
	if err != nil {
		h.log.Error("checkin rate limiter unavailable", "error", err)
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Service temporarily unavailable")
		return false
	}
	if count == 1 {
		h.rdb.Expire(r.Context(), key, checkinRateWindow+time.Second)
	}
	if count > checkinRateLimit {
		retry := time.Unix((bucket+1)*int64(checkinRateWindow/time.Second), 0).Sub(now)
		seconds := int(retry / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many requests")
		return false
	}
	return true
}

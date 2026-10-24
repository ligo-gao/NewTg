// Package httpapi 的用量日志 / 统计 / 审计只读端点（M6 Task 2）。
//
// 端点面（R-M6-2 冻结；M6c 补齐 F-05-06/07/08）：
//
//	GET /api/usage-logs                  offset 分页 + 筛选（reveal_sensitive）
//	GET /api/usage-logs/export           流式导出 TXT/Markdown（≤10,000 行）
//	GET /api/usage-logs/{id}             单条详情（含 other 投影，reveal_sensitive）
//	GET /api/usage-logs/{id}/prompt      Prompt 原文（无载体，恒 404）
//	GET /api/model-perf                  模型性能看板（F-05-08；别名
//	                                     /api/usage-logs/model-performance）
//	GET /api/usage-stats/summary         概览卡片聚合
//	GET /api/usage-stats/by-model        模型分布（top 8 + other）
//	GET /api/usage-stats/timeseries      UTC 时间桶序列
//	GET /api/usage-stats/rates           RPM/TPM/消费
//	GET /api/audit-logs                  审计日志列表
//
// 数据只从日志库（DB_LOGS_DSN）读取，主库仅用于 users 的用户名快照查询；
// 不做跨库 join（R-M6-5）。所有时间按 UTC 处理。
//
// 可见性（R-M6-4）：任何已登录用户都可调用；普通用户硬过滤
// user_id = principal.UserID；仅 role=admin 且持有 `logs:read`（或 FULL）
// 的管理员可见全部行并使用 username / scope / dimension 等管理参数。
// admin 缺 `logs:read` 时按普通用户收窄，不返回 403（除管理参数外）。
package httpapi

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/triage"
)

const (
	// 日志状态 smallint ↔ 字符串（platform-01 §12）。
	usageStatusRefused  int16 = 0
	usageStatusSuccess  int16 = 1
	usageStatusError    int16 = 2
	usageStatusRefunded int16 = 3

	usageDefaultPageSize = 20
	usageMaxPageSize     = 100

	// 时间跨度硬上限：90 天（modules-05 §3.2 / R-M6-5）。分区表按
	// created_at 扫描，超过 90 天的单次请求会把多个分区全量拉进内存；
	// 这是「可预期成本」的硬边界，不是可配置项。
	usageMaxRange = 90 * 24 * time.Hour

	// 审计日志默认窗口 30 天（modules-05 §7.2）。
	auditDefaultRange = "30d"

	// exportRowLimit 是 F-05-07 冻结的单次导出行数上限（modules-05 §9.1）。
	// 超限必须在写出任何字节之前返回 422，因此先在同一只读快照里 count。
	exportRowLimit = 10000

	// maskedSensitive 是敏感开关关闭时的占位值（modules-05 F-05-06：
	// Prompt / error_message / 重试链错误原文统一打码）。
	maskedSensitive = "••••••"

	problemTypeNotFound = "https://newtg.dev/problems/not_found"
	// problemTypeExportRowLimit 是导出超限的稳定机器可读 type；detail 同时
	// 带 `EXPORT_ROW_LIMIT` 字样，前端按 422 + type 识别分批提示。
	problemTypeExportRowLimit = "https://newtg.dev/problems/export_row_limit"
)

// UsageLogHandlerConfig 是只读端点集合的装配输入。
type UsageLogHandlerConfig struct {
	// DB 是主库，仅用于 users.username 快照与权限回源；不参与 usage_logs 查询。
	DB *sql.DB
	// LogDB 是日志库（usage_logs / audit_logs）。
	LogDB *sql.DB
	// Access 是权限回源接缝；nil 时由 DB 构造 TokenRepo（同 P-9 口径）。
	Access UserAccessStore
	// Audit 是导出审计写入器；nil 时跳过（测试/未装配日志库的装配）。
	Audit  *audit.Logger
	Logger *slog.Logger
	// Now 是时间窗口的时钟接缝（测试可注入固定时间）。
	Now func() time.Time
	// Perf 是 F-05-08 模型性能指标库查询接缝（platform-05 §3.5）。nil 表示
	// 采集管道未装配，端点返回「采集未启用」闸门而绝不用 usage_logs 伪造。
	Perf ModelPerfSource
	// PerfFlushInterval 是管道刷写周期，用于陈旧度判定（3×周期无更新 = 停摆）。
	// 零值时取设计默认 5 分钟。
	PerfFlushInterval time.Duration
	// ReadOnlyTokens 是 GET /api/logs/by-key 的只读令牌认证接缝
	// （modules-05 F-05-11）。nil 时不挂载该端点；生产为 *tokenstore.Store。
	ReadOnlyTokens auth.ReadOnlyTokenLookup
	// Redis 是 by-key 限流桶（rateLimit:v2:token:RO:<id>）的连接。
	// nil 时限流失效（fail-open），只影响限流不影响认证/查询。
	Redis *redis.Client
}

// UsageLogHandlers 持有只读端点依赖，可并发使用。
type UsageLogHandlers struct {
	db        *sql.DB
	logDB     *sql.DB
	access    UserAccessStore
	audit     *audit.Logger
	log       *slog.Logger
	now       func() time.Time
	perf      ModelPerfSource
	perfFlush time.Duration
	readOnly  auth.ReadOnlyTokenLookup
	rdb       *redis.Client
}

// NewUsageLogHandlers 构造只读端点。两个数据库都是必需项：
// 日志库是数据源，主库是用户名/权限的唯一真源，缺任一都不能静默降级。
func NewUsageLogHandlers(cfg UsageLogHandlerConfig) (*UsageLogHandlers, error) {
	if cfg.DB == nil {
		return nil, errors.New("httpapi: UsageLogHandlerConfig.DB is required")
	}
	if cfg.LogDB == nil {
		return nil, errors.New("httpapi: UsageLogHandlerConfig.LogDB is required")
	}
	access := cfg.Access
	if access == nil {
		access = store.NewTokenRepo(cfg.DB)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	flush := cfg.PerfFlushInterval
	if flush <= 0 {
		// platform-05 §3.5.5：PERF_METRICS_FLUSH_INTERVAL 默认 5 分钟。
		flush = 5 * time.Minute
	}
	return &UsageLogHandlers{
		db:        cfg.DB,
		logDB:     cfg.LogDB,
		access:    access,
		audit:     cfg.Audit,
		log:       logger,
		now:       now,
		perf:      cfg.Perf,
		perfFlush: flush,
		readOnly:  cfg.ReadOnlyTokens,
		rdb:       cfg.Redis,
	}, nil
}

// RegisterUsageLogRoutes 注册只读端点。wrap 语义同其它 Register*Routes。
func (h *UsageLogHandlers) RegisterUsageLogRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	register := func(pattern string, handler http.HandlerFunc) {
		var hd http.Handler = handler
		if wrap != nil {
			hd = wrap(hd)
		}
		mux.Handle(pattern, hd)
	}
	register("GET /api/usage-logs", h.handleUsageLogList)
	register("GET /api/usage-logs/export", h.handleUsageLogExport)
	// 失败排障（admin console 后端）。`triage/recent` 是两段路径，与单段
	// `{id}` 不重叠，也与 `{id}/prompt` 不重叠（第二段字面量不同），可直接
	// 注册。但 `triage/{request_id}` 与既有 `{id}/prompt` 在
	// `/api/usage-logs/triage/prompt` 上重叠，且两者互不更具体——直接注册
	// 会让 ServeMux panic。因此详情路由走 `/api/usage-logs/` 子树兜底
	// （子树是两者的超集，故不冲突），由 handleUsageLogSubtree 精确分发；
	// 不要把它「修正」成直接注册 `triage/{request_id}`。
	register("GET /api/usage-logs/triage/recent", h.handleRecentFailures)
	register("GET /api/usage-logs/", h.handleUsageLogSubtree)
	// F-05-08 的规范路径是 GET /api/model-perf（modules-05 §5.5 / UI §8.9）；
	// 同时保留 /api/usage-logs/model-performance 别名，兼容只读端点族的
	// 直觉路径。两个路径共用同一 handler，鉴权与参数校验不可能漂移。
	register("GET /api/model-perf", h.handleModelPerf)
	register("GET /api/usage-logs/model-performance", h.handleModelPerf)
	register("GET /api/usage-logs/{id}", h.handleUsageLogDetail)
	register("GET /api/usage-logs/{id}/prompt", h.handleUsageLogPrompt)
	register("GET /api/usage-stats/summary", h.handleUsageStatsSummary)
	register("GET /api/usage-stats/by-model", h.handleUsageStatsByModel)
	register("GET /api/usage-stats/by-user", h.handleUsageStatsByUser)
	register("GET /api/usage-stats/by-group", h.handleUsageStatsByGroup)
	register("GET /api/usage-stats/timeseries", h.handleUsageStatsTimeseries)
	register("GET /api/usage-stats/rates", h.handleUsageStatsRates)
	register("GET /api/audit-logs", h.handleAuditLogList)
}

// usageViewer 是「身份 → 数据范围」的唯一判定结果。
type usageViewer struct {
	userID int64
	admin  bool
}

// resolveViewer 解析当前身份。admin 只有在 role=admin 且持有 logs:read
// （或 FULL / "*"）时才算「可见全部」；admin 缺键时收窄为本人范围，
// 而不是把管理参数静默忽略。
func (h *UsageLogHandlers) resolveViewer(w http.ResponseWriter, r *http.Request) (usageViewer, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return usageViewer{}, false
	}
	viewer := usageViewer{userID: p.UserID}
	if p.Role != rbac.RoleAdmin {
		return viewer, true
	}
	err := CheckPermission(r.Context(), h.access, rbac.PermLogsRead)
	switch {
	case err == nil:
		viewer.admin = true
	case errors.Is(err, ErrAccessStore):
		// 权限回源故障不能伪装成「无权限」：500 让前端重试。
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return usageViewer{}, false
	case errors.Is(err, ErrNoPrincipal), errors.Is(err, ErrUserDeleted):
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return usageViewer{}, false
	default:
		// ErrPermissionDenied：admin 但无 logs:read → 本人范围。
	}
	return viewer, true
}

// rejectAdminOnlyParams 在非有效管理员传入管理参数时返回 403。
// 管理参数：username（列表/统计/审计）、scope/dimension（统计）。
func rejectAdminOnlyParams(w http.ResponseWriter, q url.Values, admin bool, keys ...string) bool {
	if admin {
		return false
	}
	for _, key := range keys {
		if q.Has(key) {
			writeForbidden(w)
			return true
		}
	}
	return false
}

// parseRevealSensitive 解析 F-05-06 的敏感内容开关。缺省 = 打码（false）；
// 只接受 0/1，其它值视为非法参数而不是「按打码处理」——静默纠正会让前端
// 误以为开关已生效。
func parseRevealSensitive(q url.Values) (bool, string) {
	raw := strings.TrimSpace(q.Get("reveal_sensitive"))
	switch raw {
	case "", "0":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, "reveal_sensitive must be 0 or 1"
	}
}

// sensitiveRevealAllowed 在客户端要求显示敏感内容时服务端二次校验
// `log.prompt.view`（modules-05 F-05-06 §8.1 第 2 步 / F-05-07）。
//
// 判定结果刻意不是 403：无权限时仍返回打码字段，避免把「你没有 prompt
// 权限」变成列表/导出的硬失败。存储故障返回 500（不能把基础设施故障伪装成
// 打码，否则运维永远发现不了权限面失效）。
func (h *UsageLogHandlers) sensitiveRevealAllowed(r *http.Request, reveal bool) (bool, error) {
	if !reveal {
		return false, nil
	}
	err := CheckPermission(r.Context(), h.access, rbac.PermLogPromptView)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrNoPrincipal), errors.Is(err, ErrUserDeleted):
		// 无权限 / 身份已失效：静默降级为打码（F-05-06 明确「即使传参也返回
		// 打码字段 + 403 仅对独立 prompt 接口生效」）。
		return false, nil
	case errors.Is(err, ErrAccessStore):
		return false, err
	default:
		return false, err
	}
}

// maskSensitiveError 返回按开关处理后的错误原文。NULL 保持为空，不把「没有
// 错误」伪造成一段打码值。
func maskSensitiveError(value sql.NullString, reveal bool) *string {
	if !value.Valid {
		return nil
	}
	if !reveal {
		out := maskedSensitive
		return &out
	}
	out := value.String
	return &out
}

// parseUsageWindow 解析 range 预设或显式 start/end（RFC3339，UTC）。
// 返回空 detail 表示成功；否则调用方写 422 并在 detail 中点名规则。
func parseUsageWindow(q url.Values, defaultRange string, now time.Time) (time.Time, time.Time, string) {
	startRaw := strings.TrimSpace(q.Get("start"))
	endRaw := strings.TrimSpace(q.Get("end"))
	if startRaw != "" || endRaw != "" {
		if startRaw == "" || endRaw == "" {
			return time.Time{}, time.Time{}, "start and end must be provided together"
		}
		start, err := time.Parse(time.RFC3339, startRaw)
		if err != nil {
			return time.Time{}, time.Time{}, "start must be an RFC3339 timestamp"
		}
		end, err := time.Parse(time.RFC3339, endRaw)
		if err != nil {
			return time.Time{}, time.Time{}, "end must be an RFC3339 timestamp"
		}
		start, end = start.UTC(), end.UTC()
		if start.After(end) {
			return time.Time{}, time.Time{}, "start must be before end"
		}
		if end.Sub(start) > usageMaxRange {
			return time.Time{}, time.Time{}, "time range cannot exceed 90 days"
		}
		return start, end, ""
	}

	raw := strings.TrimSpace(q.Get("range"))
	if raw == "" {
		raw = defaultRange
	}
	var span time.Duration
	switch raw {
	case "24h":
		span = 24 * time.Hour
	case "7d":
		span = 7 * 24 * time.Hour
	case "14d":
		span = 14 * 24 * time.Hour
	case "30d":
		span = 30 * 24 * time.Hour
	default:
		return time.Time{}, time.Time{}, "range must be one of 24h, 7d, 14d, 30d"
	}
	end := now.UTC()
	return end.Add(-span), end, ""
}

// usageFilter 是列表与统计共用的筛选条件。
type usageFilter struct {
	userID      *int64
	modelPrefix string
	group       string
	tokenID     *int64
	status      *int16
	requestID   string
}

// parseUsageFilter 解析筛选参数；includeRequestID 仅列表支持。
func parseUsageFilter(q url.Values, includeRequestID bool) (usageFilter, string) {
	var f usageFilter
	if raw := strings.TrimSpace(q.Get("model")); raw != "" {
		if len(raw) > 128 {
			return f, "model must be at most 128 characters"
		}
		f.modelPrefix = raw
	}
	f.group = strings.TrimSpace(q.Get("group"))
	if raw := strings.TrimSpace(q.Get("token_id")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return f, "token_id must be an integer"
		}
		f.tokenID = &value
	}
	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		value, ok := usageStatusFromString(raw)
		if !ok {
			return f, "status must be one of success, error, refunded, refused"
		}
		f.status = &value
	}
	if includeRequestID {
		if raw := strings.TrimSpace(q.Get("request_id")); raw != "" {
			if _, err := uuid.Parse(raw); err != nil {
				return f, "request_id must be a UUID"
			}
			f.requestID = raw
		}
	}
	return f, ""
}

func usageStatusFromString(raw string) (int16, bool) {
	switch raw {
	case "refused":
		return usageStatusRefused, true
	case "success":
		return usageStatusSuccess, true
	case "error":
		return usageStatusError, true
	case "refunded":
		return usageStatusRefunded, true
	default:
		return 0, false
	}
}

func usageStatusString(value int16) string {
	switch value {
	case usageStatusRefused:
		return "refused"
	case usageStatusSuccess:
		return "success"
	case usageStatusError:
		return "error"
	case usageStatusRefunded:
		return "refunded"
	default:
		return "unknown"
	}
}

// likePrefix 转义 LIKE 通配符后再拼 `%`：模型过滤是前缀匹配，用户输入的
// `%`/`_` 不能扩大匹配范围（PostgreSQL 默认反斜杠转义）。
func likePrefix(raw string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(raw) + "%"
}

// buildUsageWhere 生成参数化的 WHERE 子句。start 闭、end 开：相邻窗口
// 不重复计数（与分区边界 [月初, 次月初) 同口径）。
func buildUsageWhere(f usageFilter, start, end time.Time) (string, []any) {
	args := []any{start, end}
	clauses := []string{"created_at >= $1", "created_at < $2"}
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if f.userID != nil {
		add("user_id = $%d", *f.userID)
	}
	if f.modelPrefix != "" {
		add("model LIKE $%d", likePrefix(f.modelPrefix))
	}
	if f.group != "" {
		add(`"group" = $%d`, f.group)
	}
	if f.tokenID != nil {
		add("token_id = $%d", *f.tokenID)
	}
	if f.status != nil {
		add("status = $%d", *f.status)
	}
	if f.requestID != "" {
		add("request_id = $%d::uuid", f.requestID)
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func parseUsagePagination(q url.Values) (page, pageSize int, detail string) {
	page, pageSize = 1, usageDefaultPageSize
	if raw := strings.TrimSpace(q.Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return 0, 0, "p must be a positive integer"
		}
		page = value
	}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > usageMaxPageSize {
			return 0, 0, "page_size must be between 1 and 100"
		}
		pageSize = value
	}
	return page, pageSize, ""
}

func writeInvalidRequest(w http.ResponseWriter, detail string) {
	writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypeInvalidRequest, "Invalid request", detail)
}

// --- 行模型与投影 ---------------------------------------------------------

// usageLogRow 是列表/详情共用的扫描目标。可空列用 sql.Null* 保真，
// 避免把 NULL 伪造成 0（例如非流式的 first_token_ms 必须是 null）。
type usageLogRow struct {
	ID           int64
	RequestID    string
	UserID       int64
	TokenID      sql.NullInt64
	TokenName    sql.NullString
	ChannelID    sql.NullInt64
	Upstream     sql.NullString
	Model        string
	Group        sql.NullString
	PromptTokens sql.NullInt64
	Completion   sql.NullInt64
	CacheRead    sql.NullInt64
	CacheWrite   sql.NullInt64
	PreDeducted  sql.NullInt64
	Cost         sql.NullInt64
	Refunded     sql.NullInt64
	LatencyMS    sql.NullInt64
	FirstTokenMS sql.NullInt64
	Stream       bool
	Status       int16
	ErrorClass   sql.NullString
	ErrorMessage sql.NullString
	RetryChain   []byte
	Other        []byte
	CreatedAt    time.Time
}

const usageListColumns = `id, request_id, user_id, token_id, token_name, channel_id,
	upstream, model, "group", prompt_tokens, completion_tokens, cache_read_tokens,
	cache_write_tokens, pre_deducted, cost, refunded, latency_ms, first_token_ms,
	stream, status, error_class, error_message, retry_chain, created_at`

type usageRowScanner interface {
	Scan(dest ...any) error
}

func (row *usageLogRow) scan(scanner usageRowScanner) error {
	var tokenID, channelID, promptTokens, completion, cacheRead, cacheWrite sql.NullInt64
	var preDeducted, cost, refunded, latency, firstToken sql.NullInt64
	var tokenName, group, upstream, errorClass, errorMessage sql.NullString
	if err := scanner.Scan(
		&row.ID, &row.RequestID, &row.UserID, &tokenID, &tokenName, &channelID,
		&upstream, &row.Model, &group, &promptTokens, &completion, &cacheRead,
		&cacheWrite, &preDeducted, &cost, &refunded, &latency, &firstToken,
		&row.Stream, &row.Status, &errorClass, &errorMessage, &row.RetryChain,
		&row.CreatedAt,
	); err != nil {
		return err
	}
	row.TokenID, row.TokenName = tokenID, tokenName
	row.ChannelID, row.Upstream = channelID, upstream
	row.PromptTokens, row.Completion = promptTokens, completion
	row.CacheRead, row.CacheWrite = cacheRead, cacheWrite
	row.PreDeducted, row.Cost, row.Refunded = preDeducted, cost, refunded
	row.LatencyMS, row.FirstTokenMS = latency, firstToken
	row.Group, row.ErrorClass, row.ErrorMessage = group, errorClass, errorMessage
	return nil
}

func (row *usageLogRow) scanDetail(scanner usageRowScanner) error {
	var tokenID, channelID, promptTokens, completion, cacheRead, cacheWrite sql.NullInt64
	var preDeducted, cost, refunded, latency, firstToken sql.NullInt64
	var tokenName, group, upstream, errorClass, errorMessage sql.NullString
	if err := scanner.Scan(
		&row.ID, &row.RequestID, &row.UserID, &tokenID, &tokenName, &channelID,
		&upstream, &row.Model, &group, &promptTokens, &completion, &cacheRead,
		&cacheWrite, &preDeducted, &cost, &refunded, &latency, &firstToken,
		&row.Stream, &row.Status, &errorClass, &errorMessage, &row.RetryChain,
		&row.CreatedAt, &row.Other,
	); err != nil {
		return err
	}
	row.TokenID, row.TokenName = tokenID, tokenName
	row.ChannelID, row.Upstream = channelID, upstream
	row.PromptTokens, row.Completion = promptTokens, completion
	row.CacheRead, row.CacheWrite = cacheRead, cacheWrite
	row.PreDeducted, row.Cost, row.Refunded = preDeducted, cost, refunded
	row.LatencyMS, row.FirstTokenMS = latency, firstToken
	row.Group, row.ErrorClass, row.ErrorMessage = group, errorClass, errorMessage
	row.CreatedAt = row.CreatedAt.UTC() // 接口时间一律 UTC（R-M6-5）
	return nil
}

// usageLogItem 是列表项契约（R-M6-2）。username 仅管理员视图出现。
type usageLogItem struct {
	ID               int64   `json:"id"`
	RequestID        string  `json:"request_id"`
	TokenID          *int64  `json:"token_id"`
	TokenName        *string `json:"token_name"`
	ChannelID        *int64  `json:"channel_id"`
	Upstream         *string `json:"upstream"`
	Model            string  `json:"model"`
	Group            *string `json:"group"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	PreDeducted      *int64  `json:"pre_deducted"`
	Cost             int64   `json:"cost"`
	Refunded         int64   `json:"refunded"`
	LatencyMS        *int64  `json:"latency_ms"`
	// 对外字段名冻结为 `ttft`（G7-R51 / platform-01 §12）：内部列名
	// first_token_ms 只是数据库事实，不得作为 API 字段泄露第二个名字。
	FirstTokenMS *int64          `json:"ttft"`
	Stream       bool            `json:"stream"`
	Status       string          `json:"status"`
	ErrorClass   *string         `json:"error_class"`
	ErrorMessage *string         `json:"error_message"`
	RetryChain   json.RawMessage `json:"retry_chain"`
	CreatedAt    time.Time       `json:"created_at"`
	Username     *string         `json:"username,omitempty"`
}

type usageLogDetail struct {
	usageLogItem
	Other json.RawMessage `json:"other"`
}

func (row *usageLogRow) toItem(admin, revealSensitive bool) usageLogItem {
	return usageLogItem{
		ID:               row.ID,
		RequestID:        row.RequestID,
		TokenID:          nullableInt(row.TokenID),
		TokenName:        nullableString(row.TokenName),
		ChannelID:        nullableInt(row.ChannelID),
		Upstream:         nullableString(row.Upstream),
		Model:            row.Model,
		Group:            nullableString(row.Group),
		PromptTokens:     nullIntValue(row.PromptTokens),
		CompletionTokens: nullIntValue(row.Completion),
		CacheReadTokens:  nullIntValue(row.CacheRead),
		CacheWriteTokens: nullIntValue(row.CacheWrite),
		PreDeducted:      nullableInt(row.PreDeducted),
		Cost:             nullIntValue(row.Cost),
		Refunded:         nullIntValue(row.Refunded),
		LatencyMS:        nullableInt(row.LatencyMS),
		FirstTokenMS:     nullableInt(row.FirstTokenMS),
		Stream:           row.Stream,
		Status:           usageStatusString(row.Status),
		ErrorClass:       nullableString(row.ErrorClass),
		// 错误原文与 Prompt 同属 F-05-06 的敏感面：缺省打码；即使客户端
		// 传 reveal_sensitive=1，也只有在服务端重校验通过后才返回原文。
		ErrorMessage: maskSensitiveError(row.ErrorMessage, revealSensitive),
		RetryChain:   projectRetryChain(row.RetryChain, admin),
		CreatedAt:    row.CreatedAt.UTC(),
	}
}

func nullableInt(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	out := value.Int64
	return &out
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	out := value.String
	return &out
}

func nullIntValue(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

// 4 个遗留敏感键（platform-01 §12.5）：user 视图必须与三个特权分区一起剥离。
var otherUserHiddenKeys = []string{
	"admin_info", "root_info", "audit_info",
	"channel_id", "channel_name", "channel_type", "reject_reason",
}

// otherSensitiveKeys 是敏感开关关闭时须打码的 error 原文键。两类角色都
// 适用：user 视图可能保留上游错误文本，admin 视图的 reject_reason 也可能
// 承载拒绝原因原文；分类字段（error_class 等）不在此列。
var otherSensitiveKeys = []string{"reject_reason", "error_message", "error"}

// projectOther 按角色做可见性投影（platform-01 §12.5）。任何解析失败都
// 返回空对象而不是原始 jsonb：宁可丢字段也不能泄露内部映射。
func projectOther(raw []byte, admin, revealSensitive bool) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return json.RawMessage("{}")
	}
	if admin {
		// 裁决（platform-01 §12.5，M6 评审 round 1）：admin 视图 =
		// public + admin_info。root_info 与 audit_info 都是 root 专属分区，
		// 即使 audit_info 非空也不得下发给管理员。
		delete(fields, "root_info")
		delete(fields, "audit_info")
	} else {
		for _, key := range otherUserHiddenKeys {
			delete(fields, key)
		}
	}
	if !revealSensitive {
		// 只替换字符串值：非字符串形态（对象/数组）原样保留更安全，
		// 因为它们的结构本身不是可直接阅读的错误原文。
		for _, key := range otherSensitiveKeys {
			value, ok := fields[key]
			if !ok {
				continue
			}
			var text string
			if err := json.Unmarshal(value, &text); err == nil {
				fields[key], _ = json.Marshal(maskedSensitive)
			}
		}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

// retryChainUserKeys 是用户可见的 8 个元素（modules-05 §4.4 / R-M6-2）；
// priority / weight / use_group 属于选路内部细节，只对 admin 展开。
var retryChainUserKeys = []string{
	"attempt", "channel_id", "channel_name", "upstream",
	"decision", "error_class", "latency_ms", "upstream_request_id",
}

func projectRetryChain(raw []byte, admin bool) json.RawMessage {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	if admin {
		// admin 保留完整 9 项（此处不重新序列化，避免丢失大整数精度）。
		return json.RawMessage(raw)
	}
	var chain []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &chain); err != nil {
		return nil
	}
	out := make([]map[string]json.RawMessage, 0, len(chain))
	for _, step := range chain {
		projected := make(map[string]json.RawMessage, len(retryChainUserKeys))
		for _, key := range retryChainUserKeys {
			if value, ok := step[key]; ok {
				projected[key] = value
			}
		}
		out = append(out, projected)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return encoded
}

// resolveUserID 把 admin 的 username 筛选解析为 user_id（不做跨库 join，
// 只是主库的一次精确查询）。用户不存在时返回 ok=false：调用方返回空集，
// 不把「查无此人」伪造成 404。
func (h *UsageLogHandlers) resolveUserID(r *http.Request, username string) (int64, bool, error) {
	var id int64
	err := h.db.QueryRowContext(r.Context(), `SELECT id FROM users WHERE username = $1`, username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// usernamesFor 批量读取用户名快照（管理员列表用一次查询，避免 N+1）。
func (h *UsageLogHandlers) usernamesFor(r *http.Request, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	uniq := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	placeholders := make([]string, len(uniq))
	args := make([]any, len(uniq))
	for i, id := range uniq {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	rows, err := h.db.QueryContext(r.Context(),
		`SELECT id, username FROM users WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var username string
		if err := rows.Scan(&id, &username); err != nil {
			return nil, err
		}
		out[id] = username
	}
	return out, rows.Err()
}

// --- 列表 -----------------------------------------------------------------

type usageLogPage struct {
	Items    []usageLogItem `json:"items"`
	Total    int64          `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
}

func (h *UsageLogHandlers) handleUsageLogList(w http.ResponseWriter, r *http.Request) {
	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if rejectAdminOnlyParams(w, q, viewer.admin, "username") {
		return
	}
	filter, detail := parseUsageFilter(q, true)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	page, pageSize, detail := parseUsagePagination(q)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	start, end, detail := parseUsageWindow(q, "24h", h.now())
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	revealSensitive, detail := parseRevealSensitive(q)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	revealSensitive, err := h.sensitiveRevealAllowed(r, revealSensitive)
	if err != nil {
		h.log.Error("usage logs: sensitive permission check failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	if !viewer.admin {
		userID := viewer.userID
		filter.userID = &userID
	} else if username := strings.TrimSpace(q.Get("username")); username != "" {
		userID, found, err := h.resolveUserID(r, username)
		if err != nil {
			h.log.Error("usage logs: resolve username failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		if !found {
			writeJSON(w, http.StatusOK, usageLogPage{Items: []usageLogItem{}, Total: 0, Page: page, PageSize: pageSize})
			return
		}
		filter.userID = &userID
	}

	where, args := buildUsageWhere(filter, start, end)
	var total int64
	if err := h.logDB.QueryRowContext(r.Context(), `SELECT count(*) FROM usage_logs`+where, args...).Scan(&total); err != nil {
		h.log.Error("usage logs: count failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	args = append(args, pageSize, (page-1)*pageSize)
	query := `SELECT ` + usageListColumns + ` FROM usage_logs` + where +
		fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := h.logDB.QueryContext(r.Context(), query, args...)
	if err != nil {
		h.log.Error("usage logs: list query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer rows.Close()

	items := make([]usageLogItem, 0, pageSize)
	userIDs := make([]int64, 0, pageSize)
	for rows.Next() {
		var row usageLogRow
		if err := row.scan(rows); err != nil {
			h.log.Error("usage logs: scan failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		items = append(items, row.toItem(viewer.admin, revealSensitive))
		userIDs = append(userIDs, row.UserID)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("usage logs: rows failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	if viewer.admin {
		names, err := h.usernamesFor(r, userIDs)
		if err != nil {
			h.log.Error("usage logs: username lookup failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		for i := range items {
			if name, ok := names[userIDs[i]]; ok {
				items[i].Username = &name
			}
		}
	}
	writeJSON(w, http.StatusOK, usageLogPage{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// --- 详情 -----------------------------------------------------------------

func parseUsageID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (h *UsageLogHandlers) handleUsageLogDetail(w http.ResponseWriter, r *http.Request) {
	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return
	}
	id, ok := parseUsageID(r)
	if !ok {
		// 非法 id 与不存在同口径：不给探测入口。
		writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found")
		return
	}
	revealSensitive, invalidDetail := parseRevealSensitive(r.URL.Query())
	if invalidDetail != "" {
		writeInvalidRequest(w, invalidDetail)
		return
	}
	revealSensitive, err := h.sensitiveRevealAllowed(r, revealSensitive)
	if err != nil {
		h.log.Error("usage log detail: sensitive permission check failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	query := `SELECT ` + usageListColumns + `, other FROM usage_logs WHERE id = $1`
	args := []any{id}
	if !viewer.admin {
		query += ` AND user_id = $2`
		args = append(args, viewer.userID)
	}
	var row usageLogRow
	if err := row.scanDetail(h.logDB.QueryRowContext(r.Context(), query, args...)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found")
			return
		}
		h.log.Error("usage log detail: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	detail := usageLogDetail{usageLogItem: row.toItem(viewer.admin, revealSensitive), Other: projectOther(row.Other, viewer.admin, revealSensitive)}
	if viewer.admin {
		if name, err := h.usernamesFor(r, []int64{row.UserID}); err == nil {
			if username, ok := name[row.UserID]; ok {
				detail.Username = &username
			}
		} else {
			h.log.Error("usage log detail: username lookup failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// --- 失败排障（admin console） -------------------------------------------

// 失败排障端点只读，且只对 admin + logs:read 开放。resolveViewer 会把
// 「admin 但缺 logs:read」收窄为普通用户视图；排障面不应把上游/协议转换
// 证据下发给普通用户，因此非 admin 一律 403（而不是收窄成本人范围）。
const (
	triageRecentDefaultLimit = 20
	triageRecentMaxLimit     = 50

	// recent 采用分页扫描而不是一次拉全窗口：窗口最长 30 天，全量装载
	// 不可接受。maxScan 是成本上限；counts 只统计**本次返回的条目**，
	// 与 UI「最近 N 条」一致，不伪造窗口总数。
	triageRecentBatchSize = 200
	triageRecentMaxScan   = 2000
)

// TriageConversionStep 是 other.admin_info.triage.conversion 的一步。
type triageConversionStep struct {
	Stage string `json:"stage"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// triageBilling 是判定时已知的计费事实（金额单位与 usage_logs 一致）。
type triageBilling struct {
	Cost        int64  `json:"cost"`
	PreDeducted *int64 `json:"pre_deducted"`
	Refunded    int64  `json:"refunded"`
}

// triageEvidence 是响应里的证据投影；与 triage.Evidence 分开，
// 因为前端还需要 endpoint / conversion 明细等引擎不消费的事实。
type triageEvidence struct {
	Endpoint         string                 `json:"endpoint"`
	RouteFrom        string                 `json:"route_from"`
	RouteTo          string                 `json:"route_to"`
	Conversion       []triageConversionStep `json:"conversion"`
	UpstreamStatus   int                    `json:"upstream_status"`
	Attempts         int                    `json:"attempts"`
	ChannelType      string                 `json:"channel_type"`
	ChannelID        *int64                 `json:"channel_id"`
	RequestStructure string                 `json:"request_structure"`
	RejectStage      string                 `json:"reject_stage"`
	Billing          triageBilling          `json:"billing"`
	ErrorClass       string                 `json:"error_class"`
	CreatedAt        time.Time              `json:"created_at"`
}

// requestTriageResponse 是 GET /api/usage-logs/triage/{request_id} 的响应。
// found=false 时只返回 found + request_id（过期删除与从未存在不可区分）。
type requestTriageResponse struct {
	Found     bool            `json:"found"`
	RequestID string          `json:"request_id"`
	Log       *usageLogDetail `json:"log,omitempty"`
	Evidence  *triageEvidence `json:"evidence,omitempty"`
	Verdict   *triage.Verdict `json:"verdict,omitempty"`
}

// triageAdminInfo 是 other.admin_info.triage 的已落库形状。upstream_status
// 只在真正收到上游响应时存在；缺省（nil）意味着 0，不是错误。
type triageAdminInfo struct {
	Endpoint         string                 `json:"endpoint"`
	ChannelType      string                 `json:"channel_type"`
	UpstreamStatus   *int                   `json:"upstream_status"`
	Attempts         int                    `json:"attempts"`
	RequestStructure string                 `json:"request_structure"`
	Stage            string                 `json:"stage"`
	Conversion       []triageConversionStep `json:"conversion"`
}

// triageAttemptEntry 只取 retry_chain 元素里判定需要的一个键。
type triageAttemptEntry struct {
	UpstreamStatus *int `json:"upstream_status"`
}

// triageStatusRank 是同一 request_id 多行时的终态优先级：值越小越优先
// （refunded > error > refused > success）。
func triageStatusRank(status int16) int {
	switch status {
	case billing.UsageLogStatusRefunded:
		return 0
	case billing.UsageLogStatusError:
		return 1
	case billing.UsageLogStatusRefused:
		return 2
	case billing.UsageLogStatusSuccess:
		return 3
	default:
		return 4
	}
}

// triageRowPicker 在同一 request_id 的多行中按终态优先级挑一行：rank 越小
// 越优先（refunded > error > refused > success），同 rank 保留先到的一行
// （查询按 created_at DESC, id DESC，即最新一行）。列表与详情共用它，
// 保证同一 id 在两个端点得到同一个判定。
type triageRowPicker struct {
	row  *usageLogRow
	rank int
}

func (p *triageRowPicker) offer(row *usageLogRow) {
	rank := triageStatusRank(row.Status)
	if p.row != nil && rank >= p.rank {
		return
	}
	copied := *row
	p.row = &copied
	p.rank = rank
}

// parseTriageRequestID 只接受 UUID 的两种常见拼写：32 位十六进制或
// 带连字符的 36 位形式（braces/urn:uuid 等其它 uuid.Parse 能接受的变体
// 一律 422，避免把「格式错误」和「查无此 ID」混为一谈）。
func parseTriageRequestID(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	switch len(raw) {
	case 32:
		if _, err := hex.DecodeString(raw); err != nil {
			return "", false
		}
	case 36:
		if raw[8] != '-' || raw[13] != '-' || raw[18] != '-' || raw[23] != '-' {
			return "", false
		}
		compact := raw[:8] + raw[9:13] + raw[14:18] + raw[19:23] + raw[24:]
		if _, err := hex.DecodeString(compact); err != nil {
			return "", false
		}
	default:
		return "", false
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

// parseTriageAdminInfo 只读 other.admin_info.triage；任何解析失败都退化成
// 零值（证据缺失不等于端点失败）。
func parseTriageAdminInfo(raw []byte) triageAdminInfo {
	var info triageAdminInfo
	if len(raw) == 0 {
		return info
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return info
	}
	adminRaw, ok := top["admin_info"]
	if !ok {
		return info
	}
	var admin map[string]json.RawMessage
	if err := json.Unmarshal(adminRaw, &admin); err != nil {
		return info
	}
	triageRaw, ok := admin["triage"]
	if !ok {
		return info
	}
	_ = json.Unmarshal(triageRaw, &info)
	return info
}

// parseTriageRetryChain 区分「没有重试链」（NULL）与「空数组」：只有 NULL
// 才回退到 triage.attempts。
func parseTriageRetryChain(raw []byte) ([]triageAttemptEntry, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, false
	}
	var chain []triageAttemptEntry
	if err := json.Unmarshal(raw, &chain); err != nil {
		return nil, false
	}
	return chain, true
}

// triageEvidenceFromRow 从一条 usage_logs 行投影出响应证据与引擎输入。
func triageEvidenceFromRow(row *usageLogRow) (triageEvidence, triage.Evidence) {
	info := parseTriageAdminInfo(row.Other)
	chain, chainPresent := parseTriageRetryChain(row.RetryChain)

	attempts := info.Attempts
	if chainPresent {
		attempts = len(chain)
	}
	// 最近一次非零上游状态码；全为零时回退到 triage.upstream_status。
	upstreamStatus := 0
	// ServerFailureSeen 记录整条重试链里是否出现过服务端故障：任一 5xx，或
	// 任一 nil/0 状态（没拿到响应）。nil/0 不区分「上游超时」与「网关本地
	// 失败（渠道解析/分派/构造请求）」——两者在证据里本就同为「没有上游
	// 响应」，宁可保守，也不能把后者漏成「没有服务端故障」从而排除渠道。
	// 末轮的 nil/0 同样计入：「最终没有响应」本身就是服务端故障信号，否则
	// `[429, 0]` 会渲染成「没有 5xx 或超时」并排除渠道，与 retry_chain 里
	// 记录的超时自相矛盾。upstream_status 仍取「最近一次非零状态码」，所以
	// `[0]` 的中性 no_upstream_response 分支不受本标记影响。
	serverFailureSeen := false
	for _, entry := range chain {
		if entry.UpstreamStatus == nil || *entry.UpstreamStatus == 0 {
			serverFailureSeen = true
			continue
		}
		upstreamStatus = *entry.UpstreamStatus
		if *entry.UpstreamStatus >= 500 {
			serverFailureSeen = true
		}
	}
	if upstreamStatus == 0 && info.UpstreamStatus != nil {
		upstreamStatus = *info.UpstreamStatus
		if *info.UpstreamStatus >= 500 {
			serverFailureSeen = true
		}
	}

	conversion := info.Conversion
	if conversion == nil {
		// 契约里 conversion 恒为数组（可为空）：缺少 admin_info.triage、
		// 解析失败或 triage 里没有 conversion 时都退化为 []，绝不向客户端
		// 下发 null（前端按 TriageConversionStep[] 直接 map）。
		conversion = []triageConversionStep{}
	}
	routeFrom, routeTo := "", ""
	if len(conversion) > 0 {
		routeFrom = conversion[0].From
		routeTo = conversion[len(conversion)-1].To
	}

	errorClass := nullStringValue(row.ErrorClass)
	engine := triage.Evidence{
		Status:            usageStatusString(row.Status),
		ErrorClass:        errorClass,
		UpstreamStatus:    upstreamStatus,
		Attempts:          attempts,
		ConversionSteps:   len(conversion),
		RouteFrom:         routeFrom,
		RouteTo:           routeTo,
		RequestStructure:  info.RequestStructure,
		RejectStage:       info.Stage,
		ServerFailureSeen: serverFailureSeen,
	}
	out := triageEvidence{
		Endpoint:         info.Endpoint,
		RouteFrom:        routeFrom,
		RouteTo:          routeTo,
		Conversion:       conversion,
		UpstreamStatus:   upstreamStatus,
		Attempts:         attempts,
		ChannelType:      info.ChannelType,
		ChannelID:        nullableInt(row.ChannelID),
		RequestStructure: info.RequestStructure,
		RejectStage:      info.Stage,
		Billing: triageBilling{
			Cost:        nullIntValue(row.Cost),
			PreDeducted: nullableInt(row.PreDeducted),
			Refunded:    nullIntValue(row.Refunded),
		},
		ErrorClass: errorClass,
		CreatedAt:  row.CreatedAt.UTC(),
	}
	return out, engine
}

// handleUsageLogSubtree 是 /api/usage-logs/ 子树的兜底分发器。它只服务
// `triage/{request_id}`；其余未命中具体路由的路径保持与标准 mux 一致的 404
// （不新增任何可见面）。之所以需要它：`triage/{request_id}` 与既有
// `{id}/prompt` 在 `/api/usage-logs/triage/prompt` 处重叠且互不更具体，
// ServeMux 会 panic；子树模式 `/api/usage-logs/` 是两者的超集，可安全共存。
func (h *UsageLogHandlers) handleUsageLogSubtree(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/usage-logs/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	requestID, isTriage := strings.CutPrefix(rest, "triage/")
	if !isTriage || requestID == "" || strings.Contains(requestID, "/") {
		writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found")
		return
	}
	// 把路径参数交给详情 handler，后者只读 PathValue("request_id")。
	r.SetPathValue("request_id", requestID)
	h.handleRequestTriage(w, r)
}

func (h *UsageLogHandlers) handleRequestTriage(w http.ResponseWriter, r *http.Request) {
	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return
	}
	if !viewer.admin {
		writeForbidden(w)
		return
	}
	if h.logDB == nil {
		// 装配缺失不能伪装成 500：前端据 503 提示「日志库未就绪，稍后重试」。
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable, "Service unavailable")
		return
	}
	requestID, ok := parseTriageRequestID(r.PathValue("request_id"))
	if !ok {
		writeInvalidRequest(w, "request_id must be a UUID")
		return
	}

	// 同一 request_id 可能有多行（结算/退款追加行），按终态优先级在 Go 侧
	// 选择，避免把状态数字面量写进 SQL。行数天然很小。
	rows, err := h.logDB.QueryContext(r.Context(),
		`SELECT `+usageListColumns+`, other FROM usage_logs
		 WHERE request_id = $1::uuid
		 ORDER BY created_at DESC, id DESC`, requestID)
	if err != nil {
		h.log.Error("request triage: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer rows.Close()

	var picker triageRowPicker
	for rows.Next() {
		var row usageLogRow
		if err := row.scanDetail(rows); err != nil {
			h.log.Error("request triage: scan failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		row.CreatedAt = row.CreatedAt.UTC()
		picker.offer(&row)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("request triage: rows failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	best := picker.row
	if best == nil {
		// 从未存在与超过 90 天保留期在此不可区分：都是 200 + found=false。
		writeJSON(w, http.StatusOK, requestTriageResponse{Found: false, RequestID: requestID})
		return
	}

	// reveal_sensitive 不开放：排障证据不需要错误原文，恒走打码分支
	// （maskSensitiveError / toItem 的默认行为）。
	log := usageLogDetail{
		usageLogItem: best.toItem(true, false),
		Other:        projectOther(best.Other, true, false),
	}
	evidence, engineInput := triageEvidenceFromRow(best)
	verdict := triage.Classify(engineInput)
	writeJSON(w, http.StatusOK, requestTriageResponse{
		Found:     true,
		RequestID: best.RequestID,
		Log:       &log,
		Evidence:  &evidence,
		Verdict:   &verdict,
	})
}

type triageRecentItem struct {
	RequestID string       `json:"request_id"`
	CreatedAt time.Time    `json:"created_at"`
	Model     string       `json:"model"`
	ChannelID *int64       `json:"channel_id"`
	Status    string       `json:"status"`
	Owner     triage.Owner `json:"owner"`
	Sub       string       `json:"sub"`
	Rule      string       `json:"rule"`
}

type triageRecentCounts struct {
	User    int `json:"user"`
	Channel int `json:"channel"`
	Gateway int `json:"gateway"`
	Unknown int `json:"unknown"`
	Total   int `json:"total"`
}

type triageRecentWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type recentFailuresResponse struct {
	Items  []triageRecentItem `json:"items"`
	Counts triageRecentCounts `json:"counts"`
	Window triageRecentWindow `json:"window"`
}

func parseTriageRecentLimit(q url.Values) (int, string) {
	raw := strings.TrimSpace(q.Get("limit"))
	if raw == "" {
		return triageRecentDefaultLimit, ""
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > triageRecentMaxLimit {
		return 0, "limit must be an integer between 1 and 50"
	}
	return value, ""
}

func parseTriageOwner(q url.Values) (triage.Owner, string) {
	switch raw := strings.TrimSpace(q.Get("owner")); raw {
	case "":
		return "", ""
	case string(triage.OwnerUser), string(triage.OwnerChannel),
		string(triage.OwnerGateway), string(triage.OwnerUnknown):
		return triage.Owner(raw), ""
	default:
		return "", "owner must be one of user, channel, gateway, unknown"
	}
}

func (h *UsageLogHandlers) handleRecentFailures(w http.ResponseWriter, r *http.Request) {
	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return
	}
	if !viewer.admin {
		writeForbidden(w)
		return
	}
	if h.logDB == nil {
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable, "Service unavailable")
		return
	}

	q := r.URL.Query()
	limit, detail := parseTriageRecentLimit(q)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	ownerFilter, detail := parseTriageOwner(q)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	start, end, detail := parseUsageWindow(q, "24h", h.now())
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}

	// 先用与详情端点相同的 triageRowPicker 为每个 request_id 选出终态行，
	// 再按首次出现（即该 id 最新一行）的顺序产出条目——列表与详情对同一
	// id 的 status/owner/rule 因此不会互相矛盾。picker 的替换规则需要看到
	// 该 id 的全部行，所以扫描到窗口结束或 maxScan 上限后才收口。
	pickers := make(map[string]*triageRowPicker, limit)
	order := make([]string, 0, limit)
	scanned, offset := 0, 0
	for scanned < triageRecentMaxScan {
		rows, err := h.logDB.QueryContext(r.Context(),
			`SELECT `+usageListColumns+`, other FROM usage_logs
			 WHERE created_at >= $1 AND created_at < $2 AND status <> $3
			 ORDER BY created_at DESC, id DESC
			 LIMIT $4 OFFSET $5`,
			start, end, billing.UsageLogStatusSuccess, triageRecentBatchSize, offset)
		if err != nil {
			h.log.Error("recent failures: query failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}

		batch := 0
		for rows.Next() {
			var row usageLogRow
			if err := row.scanDetail(rows); err != nil {
				_ = rows.Close()
				h.log.Error("recent failures: scan failed", "error", err)
				writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
				return
			}
			batch++
			picker, seenBefore := pickers[row.RequestID]
			if !seenBefore {
				picker = &triageRowPicker{}
				pickers[row.RequestID] = picker
				order = append(order, row.RequestID)
			}
			picker.offer(&row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			h.log.Error("recent failures: rows failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		_ = rows.Close()
		scanned += batch
		if batch < triageRecentBatchSize {
			break
		}
		offset += triageRecentBatchSize
	}

	items := make([]triageRecentItem, 0, limit)
	for _, requestID := range order {
		row := pickers[requestID].row
		_, engineInput := triageEvidenceFromRow(row)
		verdict := triage.Classify(engineInput)
		if ownerFilter != "" && verdict.Owner != ownerFilter {
			continue
		}
		items = append(items, triageRecentItem{
			RequestID: row.RequestID,
			CreatedAt: row.CreatedAt.UTC(),
			Model:     row.Model,
			ChannelID: nullableInt(row.ChannelID),
			Status:    usageStatusString(row.Status),
			Owner:     verdict.Owner,
			Sub:       verdict.Sub,
			Rule:      verdict.Rule,
		})
		if len(items) == limit {
			break
		}
	}

	// counts 只统计本次返回的条目：UI 文案是「最近 N 条」，不得暗示这是
	// 整个时间窗的全量总数。
	counts := triageRecentCounts{Total: len(items)}
	for _, item := range items {
		switch item.Owner {
		case triage.OwnerUser:
			counts.User++
		case triage.OwnerChannel:
			counts.Channel++
		case triage.OwnerGateway:
			counts.Gateway++
		default:
			counts.Unknown++
		}
	}
	writeJSON(w, http.StatusOK, recentFailuresResponse{
		Items:  items,
		Counts: counts,
		Window: triageRecentWindow{Start: start, End: end},
	})
}

// --- 导出（F-05-07） ------------------------------------------------------

// exportFilterDimensions 返回本次导出实际生效的筛选「维度名」——只记名字，
// 不记任何值（modules-05 §9.1 第 5 步：审计不得成为筛选值的副本）。
func exportFilterDimensions(q url.Values) []string {
	dims := make([]string, 0, 8)
	if strings.TrimSpace(q.Get("start")) != "" || strings.TrimSpace(q.Get("end")) != "" {
		dims = append(dims, "time")
	} else if strings.TrimSpace(q.Get("range")) != "" {
		dims = append(dims, "range")
	}
	for _, key := range []string{"model", "group", "token_id", "status", "request_id", "username"} {
		if strings.TrimSpace(q.Get(key)) != "" {
			dims = append(dims, key)
		}
	}
	return dims
}

func parseExportScope(q url.Values) (string, string) {
	switch strings.TrimSpace(q.Get("scope")) {
	case "", "filtered":
		return "filtered", ""
	case "all":
		return "all", ""
	default:
		return "", "scope must be filtered or all"
	}
}

func parseExportFormat(q url.Values) (string, string) {
	switch strings.TrimSpace(q.Get("format")) {
	case "", "txt":
		return "txt", ""
	case "markdown":
		return "markdown", ""
	default:
		return "", "format must be txt or markdown"
	}
}

// exportCell 中和单行文本字段的注入面（modules-05 §9.3）：
//   - TXT：TAB/CR/LF 一律替换为空格，避免伪造列/行；
//   - Markdown：`|` 转义，CR/LF 折为空格，避免伪造表格列；
//   - 两种格式都以 `= + - @` 开头的文本前置单引号，防电子表格公式注入。
//
// 数值列不经过这里（数值本身不可能是公式前缀，也不应被引号污染）。
func exportCell(raw, format string) string {
	if format == "markdown" {
		raw = strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(raw)
		// 先转义反斜杠再转义管道：否则输入中的 `\|` 会被拼成 `\\|`，
		// 管道仍会结束单元格（Markdown 的转义只认一个反斜杠）。
		raw = strings.ReplaceAll(raw, `\`, `\\`)
		raw = strings.ReplaceAll(raw, "|", `\|`)
	} else {
		raw = strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(raw)
	}
	if raw != "" && strings.ContainsRune("=+-@", rune(raw[0])) {
		return "'" + raw
	}
	return raw
}

func exportOptionalInt(value sql.NullInt64) string {
	if !value.Valid {
		return ""
	}
	return strconv.FormatInt(value.Int64, 10)
}

// exportUsername 按需解析用户快照。导出最多 1 万行，但不同用户通常很少；
// 用惰性缓存避免 N+1，同时绝不做跨库 join（R-M6-5）。
func (h *UsageLogHandlers) exportUsername(r *http.Request, cache map[int64]string, userID int64) string {
	if name, ok := cache[userID]; ok {
		return name
	}
	name := ""
	if names, err := h.usernamesFor(r, []int64{userID}); err == nil {
		name = names[userID]
	} else {
		h.log.Warn("usage log export: username lookup failed", "user_id", userID, "error", err)
	}
	cache[userID] = name
	return name
}

func (h *UsageLogHandlers) handleUsageLogExport(w http.ResponseWriter, r *http.Request) {
	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if rejectAdminOnlyParams(w, q, viewer.admin, "username") {
		return
	}
	scope, detail := parseExportScope(q)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	format, detail := parseExportFormat(q)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	filter, detail := parseUsageFilter(q, true)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	revealSensitive, detail := parseRevealSensitive(q)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	// 参数即使在 scope=all 下被忽略也要先校验：非法 range 不应因为
	// 「全部范围」而静默通过，否则同一 URL 在两种 scope 下判定不一致。
	start, end, detail := parseUsageWindow(q, "24h", h.now())
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	revealSensitive, err := h.sensitiveRevealAllowed(r, revealSensitive)
	if err != nil {
		h.log.Error("usage log export: sensitive permission check failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	dims := exportFilterDimensions(q)
	if scope == "all" {
		// 「全部」= 角色可见全量：忽略所有筛选（含时间窗），只保留角色范围。
		filter = usageFilter{}
		dims = nil
	}
	if !viewer.admin {
		userID := viewer.userID
		filter.userID = &userID
	} else if scope == "filtered" {
		if username := strings.TrimSpace(q.Get("username")); username != "" {
			userID, found, err := h.resolveUserID(r, username)
			if err != nil {
				h.log.Error("usage log export: resolve username failed", "error", err)
				writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
				return
			}
			if !found {
				// 查无此人 = 空集；不生成空文件（modules-05 §9.1）。
				w.WriteHeader(http.StatusNoContent)
				return
			}
			filter.userID = &userID
		}
	}

	where, args := buildUsageWhere(filter, start, end)
	if scope == "all" {
		// scope=all 不带时间条件：WHERE 必须完全由角色范围构成。
		where, args = buildUsageWhereAll(filter)
	}

	// 只读 + REPEATABLE READ：count 与流式查询必须看同一快照，否则两查
	// 之间新增的行可能让「已判定 ≤10000」的导出悄悄多出行（或反之）。
	tx, err := h.logDB.BeginTx(r.Context(), &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		h.log.Error("usage log export: begin tx failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var rowCount int64
	if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM usage_logs`+where, args...).Scan(&rowCount); err != nil {
		h.log.Error("usage log export: count failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	if rowCount == 0 {
		// 不生成空文件：204 明确表示「没有内容可导出」，且不写审计（没有
		// 任何数据离开系统）。
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if rowCount > exportRowLimit {
		// 必须在写出任何字节之前拒绝：422 + EXPORT_ROW_LIMIT，响应体是
		// problem+json 而不是附件，天然不存在「部分文件」。
		writeProblemDetail(w, http.StatusUnprocessableEntity,
			problemTypeExportRowLimit, "Export row limit exceeded",
			fmt.Sprintf("EXPORT_ROW_LIMIT: export is limited to %d rows; narrow the filters or export in batches", exportRowLimit))
		return
	}

	rows, err := tx.QueryContext(r.Context(),
		`SELECT `+usageListColumns+` FROM usage_logs`+where+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		h.log.Error("usage log export: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer rows.Close()

	filename := fmt.Sprintf("usage-logs-%s-%s.%s", scope, h.now().UTC().Format("20060102-1504"), exportFileExt(format))
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if format == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	// 导出内容包含错误原文（开关打开时）属于敏感面，任何层都不得缓存。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	writeErr := writeExportHeader(w, format, scope, exportFilterSummary(q, scope, format), rowCount, viewer.admin, h.now().UTC())
	usernameCache := make(map[int64]string)
	written := int64(0)
	for writeErr == nil && rows.Next() {
		var row usageLogRow
		if err := row.scan(rows); err != nil {
			h.log.Error("usage log export: scan failed", "error", err)
			writeErr = err
			break
		}
		username := ""
		if viewer.admin {
			username = h.exportUsername(r, usernameCache, row.UserID)
		}
		writeErr = writeExportRow(w, format, &row, username, viewer.admin, revealSensitive)
		written++
		if flusher != nil && written%200 == 0 {
			flusher.Flush()
		}
	}
	if err := rows.Err(); err != nil && writeErr == nil {
		h.log.Error("usage log export: rows failed", "error", err)
		writeErr = err
	}
	_ = rows.Close()
	if writeErr == nil {
		if err := tx.Commit(); err != nil {
			// 提交失败无妨：这只是一个只读快照，响应已经完整写出。
			h.log.Warn("usage log export: commit read-only tx failed", "error", err)
		}
	}

	if writeErr != nil {
		// 客户端中途断开或扫描失败：响应已开始，无法改状态码；不写成功审计。
		h.log.Warn("usage log export: stream aborted", "error", writeErr)
		return
	}
	// 审计是响应之后的 best-effort 写（可能等待最多 3s）；先 flush 已
	// 生成的字节，避免小文件在客户端侧被审计写入拖住。
	if flusher != nil {
		flusher.Flush()
	}
	h.writeExportAudit(r, viewer.userID, scope, format, rowCount, dims)
}

// exportFilterSummary 生成 TXT/Markdown 元信息里的筛选摘要。与审计不同，
// 这里展示**值**（用户需要知道导出了哪个筛选结果），因此逐个值按格式
// 转义；审计仍然只记录维度名。
func exportFilterSummary(q url.Values, scope, format string) string {
	if scope != "filtered" {
		return "none"
	}
	parts := make([]string, 0, 8)
	add := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, key+"="+exportCell(value, format))
		}
	}
	if strings.TrimSpace(q.Get("start")) != "" || strings.TrimSpace(q.Get("end")) != "" {
		add("start", q.Get("start"))
		add("end", q.Get("end"))
	} else {
		add("range", q.Get("range"))
	}
	for _, key := range []string{"model", "group", "token_id", "status", "request_id", "username"} {
		add(key, q.Get(key))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// buildUsageWhereAll 生成不带时间窗的 WHERE（scope=all 专用）。参数编号从
// $1 起，避免先构造时间条件再前移编号（前移在 $10+ 时会与已替换的 $1
// 发生二次替换）。
func buildUsageWhereAll(f usageFilter) (string, []any) {
	args := make([]any, 0, 6)
	clauses := make([]string, 0, 6)
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if f.userID != nil {
		add("user_id = $%d", *f.userID)
	}
	if f.modelPrefix != "" {
		add("model LIKE $%d", likePrefix(f.modelPrefix))
	}
	if f.group != "" {
		add(`"group" = $%d`, f.group)
	}
	if f.tokenID != nil {
		add("token_id = $%d", *f.tokenID)
	}
	if f.status != nil {
		add("status = $%d", *f.status)
	}
	if f.requestID != "" {
		add("request_id = $%d::uuid", f.requestID)
	}
	if len(clauses) == 0 {
		// 管理员导出全量：仍然保持 WHERE 语法，便于与其它分支共用 SQL 拼接。
		return " WHERE 1 = 1", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func exportFileExt(format string) string {
	if format == "markdown" {
		return "md"
	}
	return "txt"
}

func (h *UsageLogHandlers) writeExportAudit(r *http.Request, actor int64, scope, format string, rowCount int64, dims []string) {
	if h.audit == nil {
		return
	}
	payload := map[string]any{
		"scope":     scope,
		"format":    format,
		"row_count": rowCount,
	}
	if len(dims) > 0 {
		payload["filters"] = dims
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		h.log.Warn("usage log export: encode audit detail failed", "error", err)
		return
	}
	event := audit.Event{
		Action:    audit.ActionLogExport,
		Actor:     actor,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Method:    r.Method,
		Route:     r.Pattern,
		Detail:    raw,
	}
	if name, err := h.usernamesFor(r, []int64{actor}); err == nil {
		event.Username = name[actor]
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Warn("usage log export: audit write failed", "actor", actor, "error", err)
	}
}

// exportColumns 是固定字段集（modules-05 §9.3），不受前端列显隐影响。
// 用户列仅 admin 出现；Prompt 原文永不出现在导出中（无载体，也无字段）。
func exportColumns(admin bool) []string {
	cols := []string{
		"time", "status", "model", "token_name",
		"prompt_tokens", "completion_tokens", "cache_read_tokens", "cache_write_tokens",
		"cost_credits", "latency_ms", "ttft_ms", "channel",
	}
	if admin {
		cols = append(cols, "username")
	}
	return append(cols, "stream", "request_id", "error_class", "error_message")
}

func exportRowValues(row *usageLogRow, username string, admin, revealSensitive bool) []string {
	values := []string{
		row.CreatedAt.UTC().Format(time.RFC3339),
		usageStatusString(row.Status),
		row.Model,
		nullStringValue(row.TokenName),
		strconv.FormatInt(nullIntValue(row.PromptTokens), 10),
		strconv.FormatInt(nullIntValue(row.Completion), 10),
		strconv.FormatInt(nullIntValue(row.CacheRead), 10),
		strconv.FormatInt(nullIntValue(row.CacheWrite), 10),
		strconv.FormatInt(nullIntValue(row.Cost), 10),
		exportOptionalInt(row.LatencyMS),
		"",
		nullStringValue(row.Upstream),
	}
	// TTFT 只对 SSE 流式请求有意义（非流式即使列有值也不导出）。
	if row.Stream {
		values[10] = exportOptionalInt(row.FirstTokenMS)
	}
	if admin {
		values = append(values, username)
	}
	stream := "no"
	if row.Stream {
		stream = "yes"
	}
	values = append(values,
		stream,
		row.RequestID,
		nullStringValue(row.ErrorClass),
	)
	if value := maskSensitiveError(row.ErrorMessage, revealSensitive); value != nil {
		values = append(values, *value)
	} else {
		values = append(values, "")
	}
	return values
}

func nullStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func writeExportHeader(w io.Writer, format, scope, filterSummary string, rowCount int64, admin bool, now time.Time) error {
	cols := exportColumns(admin)
	filters := filterSummary
	if filters == "" {
		filters = "none"
	}
	var b strings.Builder
	if format == "markdown" {
		b.WriteString("# Usage Logs Export\n\n")
		fmt.Fprintf(&b, "- Exported at: %s\n", now.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Scope: %s\n", scope)
		fmt.Fprintf(&b, "- Filters: %s\n", filters)
		fmt.Fprintf(&b, "- Rows: %d\n", rowCount)
		b.WriteString("- Timezone: UTC\n\n")
		b.WriteString("| " + strings.Join(cols, " | ") + " |\n")
		b.WriteString("|")
		for range cols {
			b.WriteString(" --- |")
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "# Exported at: %s\n", now.Format(time.RFC3339))
		fmt.Fprintf(&b, "# Scope: %s\n", scope)
		fmt.Fprintf(&b, "# Filters: %s\n", filters)
		fmt.Fprintf(&b, "# Rows: %d / Timezone: UTC\n", rowCount)
		b.WriteString(strings.Join(cols, "\t") + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeExportRow(w io.Writer, format string, row *usageLogRow, username string, admin, revealSensitive bool) error {
	values := exportRowValues(row, username, admin, revealSensitive)
	// 仅文本列经过注入中和；数值列（token/cost/latency/ttft）保持原样。
	numeric := map[int]bool{4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true}
	var line strings.Builder
	if format == "markdown" {
		line.WriteString("| ")
	}
	for i, value := range values {
		if !numeric[i] {
			value = exportCell(value, format)
		}
		if i > 0 {
			if format == "markdown" {
				line.WriteString(" | ")
			} else {
				line.WriteString("\t")
			}
		}
		line.WriteString(value)
	}
	if format == "markdown" {
		line.WriteString(" |")
	}
	line.WriteString("\n")
	_, err := io.WriteString(w, line.String())
	return err
}

// --- Prompt（无载体） -----------------------------------------------------

// handleUsageLogPrompt 实现 R-M6-2 / platform-01 §21 U-12 / G7-R30：
// **Prompt 没有任何落库载体**（usage_logs 无 prompt 列、无独立表、无 Redis
// 缓存），因此即使权限通过、行可见，也永远返回 404。
//
// 为什么保留端点：前端契约已冻结（按钮/权限位 `log.prompt.view` 仍在），
// 保留端点让「无权限 403」与「有权限但无内容 404」两个语义稳定可测；
// 日后若硬需求引入载体，只需替换 404 分支，不动授权面。
func (h *UsageLogHandlers) handleUsageLogPrompt(w http.ResponseWriter, r *http.Request) {
	// 权限先于行查询：无 `log.prompt.view` 一律 403，不泄露行是否存在。
	err := CheckPermission(r.Context(), h.access, rbac.PermLogPromptView)
	switch {
	case errors.Is(err, ErrAccessStore):
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	case errors.Is(err, ErrNoPrincipal), errors.Is(err, ErrUserDeleted):
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return
	case err != nil:
		writeForbidden(w)
		return
	}

	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return
	}
	id, ok := parseUsageID(r)
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found")
		return
	}
	// 可见性检查：他人行与不存在同口径 404（不泄露存在性）。
	query := `SELECT 1 FROM usage_logs WHERE id = $1`
	args := []any{id}
	if !viewer.admin {
		query += ` AND user_id = $2`
		args = append(args, viewer.userID)
	}
	var exists int
	if err := h.logDB.QueryRowContext(r.Context(), query, args...).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.Header().Set("Cache-Control", "no-store")
			writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found")
			return
		}
		h.log.Error("usage log prompt: lookup failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	// 无载体：即使行存在也返回 404；显式 no-store 因为这是 T2 级数据路径，
	// 未来承载 Prompt 明文的响应必须在任何层都不被缓存（security-09 §2.4）。
	w.Header().Set("Cache-Control", "no-store")
	writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found")
}

// --- 统计 ----------------------------------------------------------------

func (h *UsageLogHandlers) statsWhere(w http.ResponseWriter, r *http.Request, q url.Values) (usageViewer, string, []any, bool) {
	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return usageViewer{}, "", nil, false
	}
	if rejectAdminOnlyParams(w, q, viewer.admin, "username", "scope", "dimension") {
		return usageViewer{}, "", nil, false
	}
	filter, detail := parseUsageFilter(q, false)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return usageViewer{}, "", nil, false
	}
	start, end, detail := parseUsageWindow(q, "24h", h.now())
	if detail != "" {
		writeInvalidRequest(w, detail)
		return usageViewer{}, "", nil, false
	}

	if !viewer.admin {
		userID := viewer.userID
		filter.userID = &userID
	} else if username := strings.TrimSpace(q.Get("username")); username != "" {
		userID, found, err := h.resolveUserID(r, username)
		if err != nil {
			h.log.Error("usage stats: resolve username failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return usageViewer{}, "", nil, false
		}
		if !found {
			// 不存在的用户名 → 空集（卡片显示 0），不是 404。
			return viewer, ` WHERE 1 = 0`, nil, true
		}
		filter.userID = &userID
	}
	where, args := buildUsageWhere(filter, start, end)
	return viewer, where, args, true
}

type usageSummaryResponse struct {
	CostTotal             int64   `json:"cost_total"`
	RequestCount          int64   `json:"request_count"`
	SuccessRate           float64 `json:"success_rate"`
	PromptTokensTotal     int64   `json:"prompt_tokens_total"`
	CompletionTokensTotal int64   `json:"completion_tokens_total"`
	PreDeductedTotal      int64   `json:"pre_deducted_total"`
	RefundedTotal         int64   `json:"refunded_total"`
	// 成功请求的延迟分位（毫秒）。只统计 status=1 且 latency_ms>0 的行；
	// 无样本时为 0，绝不返回 NaN 或伪造值。
	P50LatencyMS   int64 `json:"p50_latency_ms"`
	P95LatencyMS   int64 `json:"p95_latency_ms"`
	LatencySamples int64 `json:"latency_samples"`
	// 窗口内去重用户数。非管理员按 statsWhere 收窄后恒为 1（或 0 行时为 0）。
	ActiveUsers int64 `json:"active_users"`
}

func (h *UsageLogHandlers) handleUsageStatsSummary(w http.ResponseWriter, r *http.Request) {
	_, where, args, ok := h.statsWhere(w, r, r.URL.Query())
	if !ok {
		return
	}
	var (
		out     usageSummaryResponse
		success int64
	)
	err := h.logDB.QueryRowContext(r.Context(), `
		SELECT COALESCE(SUM(cost), 0), count(*),
		       COALESCE(SUM(CASE WHEN status = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(pre_deducted), 0), COALESCE(SUM(refunded), 0)
		FROM usage_logs`+where, args...).Scan(
		&out.CostTotal, &out.RequestCount, &success,
		&out.PromptTokensTotal, &out.CompletionTokensTotal,
		&out.PreDeductedTotal, &out.RefundedTotal,
	)
	if err != nil {
		h.log.Error("usage stats summary: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	// 0 行时成功率定义为 0（绝不是 NaN）：前端直接渲染百分比。
	if out.RequestCount > 0 {
		out.SuccessRate = float64(success) / float64(out.RequestCount)
	}

	// 延迟分位只取成功请求：失败请求的 latency 是“失败得多快”，混进来会
	// 让 P95 看起来变好。percentile_cont 忽略 NULL，仍显式过滤 0 值。
	const latencyCond = `status = 1 AND latency_ms IS NOT NULL AND latency_ms > 0`
	latencyWhere := where
	if strings.TrimSpace(latencyWhere) == "" {
		latencyWhere = " WHERE " + latencyCond
	} else {
		latencyWhere += " AND " + latencyCond
	}
	if err := h.logDB.QueryRowContext(r.Context(), `
		SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), 0)::bigint,
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::bigint,
		       count(*)
		FROM usage_logs`+latencyWhere, args...).Scan(
		&out.P50LatencyMS, &out.P95LatencyMS, &out.LatencySamples,
	); err != nil {
		h.log.Error("usage stats summary: latency query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	if err := h.logDB.QueryRowContext(r.Context(),
		`SELECT count(DISTINCT user_id) FROM usage_logs`+where, args...).Scan(&out.ActiveUsers); err != nil {
		h.log.Error("usage stats summary: active users query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type usageByModelItem struct {
	Model   string  `json:"model"`
	Cost    int64   `json:"cost"`
	Tokens  int64   `json:"tokens"`
	Percent float64 `json:"percent"`
}

func (h *UsageLogHandlers) handleUsageStatsByModel(w http.ResponseWriter, r *http.Request) {
	_, where, args, ok := h.statsWhere(w, r, r.URL.Query())
	if !ok {
		return
	}
	rows, err := h.logDB.QueryContext(r.Context(), `
		SELECT model, COALESCE(SUM(cost), 0),
		       COALESCE(SUM(COALESCE(prompt_tokens, 0) + COALESCE(completion_tokens, 0)), 0)
		FROM usage_logs`+where+`
		GROUP BY model ORDER BY SUM(cost) DESC, model ASC`, args...)
	if err != nil {
		h.log.Error("usage stats by-model: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer rows.Close()

	all := make([]usageByModelItem, 0, 16)
	var totalCost int64
	for rows.Next() {
		var item usageByModelItem
		if err := rows.Scan(&item.Model, &item.Cost, &item.Tokens); err != nil {
			h.log.Error("usage stats by-model: scan failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		totalCost += item.Cost
		all = append(all, item)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("usage stats by-model: rows failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	items := all
	if len(all) > 8 {
		other := usageByModelItem{Model: "other"}
		for _, item := range all[8:] {
			other.Cost += item.Cost
			other.Tokens += item.Tokens
		}
		items = append(append([]usageByModelItem{}, all[:8]...), other)
	}
	assignPercentBasisPoints(items, totalCost)
	writeJSON(w, http.StatusOK, items)
}

// assignPercentBasisPoints 用十进制基点（1% = 100 bps）分配百分比，
// 四舍五入后的残差补到第一大项，保证各扇区合计恰好 100.00。
// 金额是 bigint，不能用 float64 乘除（大额会丢精度，R-M6-5）。
type usageByUserItem struct {
	UserID   int64   `json:"user_id"`
	Username string  `json:"username"`
	Cost     int64   `json:"cost"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Percent  float64 `json:"percent"`
}

type usageByUserResponse struct {
	Items []usageByUserItem `json:"items"`
	Total int64             `json:"total"`
}

// handleUsageStatsByUser 按用户聚合消费。非管理员由 statsWhere 收窄为本人，
// 因此只会返回自己的一行；管理员 + logs:read 才看得到全站。
func (h *UsageLogHandlers) handleUsageStatsByUser(w http.ResponseWriter, r *http.Request) {
	_, where, args, ok := h.statsWhere(w, r, r.URL.Query())
	if !ok {
		return
	}
	rows, err := h.logDB.QueryContext(r.Context(), `
		SELECT user_id, COALESCE(SUM(cost), 0), count(*),
		       COALESCE(SUM(COALESCE(prompt_tokens, 0) + COALESCE(completion_tokens, 0)), 0)
		FROM usage_logs`+where+`
		GROUP BY user_id ORDER BY SUM(cost) DESC, user_id ASC`, args...)
	if err != nil {
		h.log.Error("usage stats by-user: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer rows.Close()

	all := make([]usageByUserItem, 0, 16)
	var totalCost int64
	for rows.Next() {
		var item usageByUserItem
		if err := rows.Scan(&item.UserID, &item.Cost, &item.Requests, &item.Tokens); err != nil {
			h.log.Error("usage stats by-user: scan failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		totalCost += item.Cost
		all = append(all, item)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("usage stats by-user: rows failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	items := all
	if len(all) > 8 {
		other := usageByUserItem{Username: "other"}
		for _, item := range all[8:] {
			other.Cost += item.Cost
			other.Requests += item.Requests
			other.Tokens += item.Tokens
		}
		items = append(append([]usageByUserItem{}, all[:8]...), other)
	}

	ids := make([]int64, 0, len(items))
	for _, item := range items {
		if item.UserID > 0 {
			ids = append(ids, item.UserID)
		}
	}
	names, err := h.usernamesFor(r, ids)
	if err != nil {
		// 用户名只是展示信息：主库查询失败时保留空串（前端显示 #id），
		// 不让整个仪表台因为一次快照查询失败而 500。
		h.log.Warn("usage stats by-user: username lookup failed", "error", err)
	}
	for i := range items {
		if items[i].UserID > 0 {
			items[i].Username = names[items[i].UserID]
		}
	}

	costs := make([]int64, len(items))
	for i := range items {
		costs[i] = items[i].Cost
	}
	percents := percentBasisPoints(costs, totalCost)
	for i := range items {
		items[i].Percent = percents[i]
	}
	writeJSON(w, http.StatusOK, usageByUserResponse{Items: items, Total: totalCost})
}

type usageByGroupItem struct {
	Group    string  `json:"group"`
	Cost     int64   `json:"cost"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Percent  float64 `json:"percent"`
}

type usageByGroupResponse struct {
	Items []usageByGroupItem `json:"items"`
	Total int64              `json:"total"`
}

// handleUsageStatsByGroup 按用户分组聚合消费；空分组归入 default。
func (h *UsageLogHandlers) handleUsageStatsByGroup(w http.ResponseWriter, r *http.Request) {
	_, where, args, ok := h.statsWhere(w, r, r.URL.Query())
	if !ok {
		return
	}
	rows, err := h.logDB.QueryContext(r.Context(), `
		SELECT COALESCE(NULLIF("group", ''), 'default') AS group_name,
		       COALESCE(SUM(cost), 0), count(*),
		       COALESCE(SUM(COALESCE(prompt_tokens, 0) + COALESCE(completion_tokens, 0)), 0)
		FROM usage_logs`+where+`
		GROUP BY 1 ORDER BY SUM(cost) DESC, 1 ASC`, args...)
	if err != nil {
		h.log.Error("usage stats by-group: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer rows.Close()

	all := make([]usageByGroupItem, 0, 16)
	var totalCost int64
	for rows.Next() {
		var item usageByGroupItem
		if err := rows.Scan(&item.Group, &item.Cost, &item.Requests, &item.Tokens); err != nil {
			h.log.Error("usage stats by-group: scan failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		totalCost += item.Cost
		all = append(all, item)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("usage stats by-group: rows failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	items := all
	if len(all) > 8 {
		other := usageByGroupItem{Group: "other"}
		for _, item := range all[8:] {
			other.Cost += item.Cost
			other.Requests += item.Requests
			other.Tokens += item.Tokens
		}
		items = append(append([]usageByGroupItem{}, all[:8]...), other)
	}

	costs := make([]int64, len(items))
	for i := range items {
		costs[i] = items[i].Cost
	}
	percents := percentBasisPoints(costs, totalCost)
	for i := range items {
		items[i].Percent = percents[i]
	}
	writeJSON(w, http.StatusOK, usageByGroupResponse{Items: items, Total: totalCost})
}

func assignPercentBasisPoints(items []usageByModelItem, totalCost int64) {
	costs := make([]int64, len(items))
	for i := range items {
		costs[i] = items[i].Cost
	}
	percents := percentBasisPoints(costs, totalCost)
	for i := range items {
		items[i].Percent = percents[i]
	}
}

// percentBasisPoints 是百分比分配的共用实现；调用方按自己的元素类型写回。
func percentBasisPoints(costs []int64, totalCost int64) []float64 {
	out := make([]float64, len(costs))
	if totalCost <= 0 {
		return out
	}
	total := decimal.NewFromInt(totalCost)
	bps := make([]int64, len(costs))
	used := int64(0)
	for i, cost := range costs {
		bps[i] = decimal.NewFromInt(cost).
			Mul(decimal.NewFromInt(10000)).
			DivRound(total, 0).IntPart()
		used += bps[i]
	}
	// 残差在整数基点上补齐，最后才转回百分数：先转 float64 再重建
	// int64 会因二进制表示截断（如 17.58 → 1757.99…）而丢掉残差。
	if residual := int64(10000) - used; residual != 0 && len(bps) > 0 {
		bps[0] += residual
	}
	for i := range bps {
		out[i] = float64(bps[i]) / 100
	}
	return out
}

type usageTimeseriesItem struct {
	Bucket   string `json:"bucket"`
	Cost     int64  `json:"cost"`
	Requests int64  `json:"requests"`
}

func (h *UsageLogHandlers) handleUsageStatsTimeseries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, where, args, ok := h.statsWhere(w, r, q)
	if !ok {
		return
	}
	granularity := strings.TrimSpace(q.Get("granularity"))
	if granularity == "" {
		granularity = "hour"
	}
	var bucketExpr string
	switch granularity {
	case "hour":
		bucketExpr = `to_char(date_trunc('hour', created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`
	case "day":
		bucketExpr = `to_char(date_trunc('day', created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`
	default:
		writeInvalidRequest(w, "granularity must be hour or day")
		return
	}
	// 只返回存在的桶，不补零：缺桶由前端按同一 UTC 规则零填充（R-M6-2）。
	rows, err := h.logDB.QueryContext(r.Context(), `
		SELECT `+bucketExpr+`, COALESCE(SUM(cost), 0), count(*)
		FROM usage_logs`+where+`
		GROUP BY 1 ORDER BY 1 ASC`, args...)
	if err != nil {
		h.log.Error("usage stats timeseries: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer rows.Close()
	items := make([]usageTimeseriesItem, 0, 24)
	for rows.Next() {
		var item usageTimeseriesItem
		if err := rows.Scan(&item.Bucket, &item.Cost, &item.Requests); err != nil {
			h.log.Error("usage stats timeseries: scan failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("usage stats timeseries: rows failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

type usageRatesResponse struct {
	RPMAvg           float64 `json:"rpm_avg"`
	RPMPeak          int64   `json:"rpm_peak"`
	TPMAvg           float64 `json:"tpm_avg"`
	CostTotal        int64   `json:"cost_total"`
	PreDeductedTotal int64   `json:"pre_deducted_total"`
	RefundedTotal    int64   `json:"refunded_total"`
}

func (h *UsageLogHandlers) handleUsageStatsRates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// 窗口需要原始 start/end 计算分钟数；statsWhere 已做同一套校验，
	// 这里再解析一次是纯函数调用，不产生额外查询。
	_, where, args, ok := h.statsWhere(w, r, q)
	if !ok {
		return
	}
	start, end, detail := parseUsageWindow(q, "24h", h.now())
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}

	var out usageRatesResponse
	var tokens int64
	if err := h.logDB.QueryRowContext(r.Context(), `
		SELECT COALESCE(SUM(cost), 0),
		       COALESCE(SUM(COALESCE(prompt_tokens, 0) + COALESCE(completion_tokens, 0)), 0),
		       COALESCE(SUM(pre_deducted), 0), COALESCE(SUM(refunded), 0)
		FROM usage_logs`+where, args...).Scan(
		&out.CostTotal, &tokens, &out.PreDeductedTotal, &out.RefundedTotal,
	); err != nil {
		h.log.Error("usage stats rates: totals failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	var requests int64
	if err := h.logDB.QueryRowContext(r.Context(), `SELECT count(*) FROM usage_logs`+where, args...).Scan(&requests); err != nil {
		h.log.Error("usage stats rates: count failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	// 峰值 = 任一分钟窗口内的最大请求数（UTC minute bucket）。
	if err := h.logDB.QueryRowContext(r.Context(), `
		SELECT COALESCE(MAX(bucket_count), 0) FROM (
			SELECT count(*) AS bucket_count FROM usage_logs`+where+`
			GROUP BY date_trunc('minute', created_at AT TIME ZONE 'UTC')
		) AS buckets`, args...).Scan(&out.RPMPeak); err != nil {
		h.log.Error("usage stats rates: peak failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	minutes := end.Sub(start).Minutes()
	if minutes > 0 {
		// RPM = 请求数 / 窗口分钟数；TPM = (输入+输出) tokens / 窗口分钟数
		//（modules-05 §6.1 与 UI 设计：TPM 即 tokens per minute，前端以 K 缩写）。
		out.RPMAvg = float64(requests) / minutes
		out.TPMAvg = float64(tokens) / minutes
	}
	writeJSON(w, http.StatusOK, out)
}

// --- 审计日志 -------------------------------------------------------------

type auditLogItem struct {
	ID         int64           `json:"id"`
	UserID     int64           `json:"user_id"`
	Username   *string         `json:"username,omitempty"`
	Category   string          `json:"category"`
	IP         *string         `json:"ip"`
	UserAgent  *string         `json:"user_agent"`
	Method     *string         `json:"method"`
	Route      *string         `json:"route"`
	StatusCode *int64          `json:"status_code"`
	Result     string          `json:"result"`
	Detail     json.RawMessage `json:"detail"`
	Before     json.RawMessage `json:"before"`
	After      json.RawMessage `json:"after"`
	CreatedAt  time.Time       `json:"created_at"`
}

type auditLogPage struct {
	Items    []auditLogItem `json:"items"`
	Total    int64          `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
}

func (h *UsageLogHandlers) handleAuditLogList(w http.ResponseWriter, r *http.Request) {
	viewer, ok := h.resolveViewer(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if rejectAdminOnlyParams(w, q, viewer.admin, "username") {
		return
	}
	category := strings.TrimSpace(q.Get("category"))
	switch category {
	case "", "login", "security", "operation", "access_token":
	default:
		writeInvalidRequest(w, "category must be one of login, security, operation, access_token")
		return
	}
	result := strings.TrimSpace(q.Get("result"))
	switch result {
	case "", "success", "failed":
	default:
		writeInvalidRequest(w, "result must be one of success, failed")
		return
	}
	page, pageSize, detail := parseUsagePagination(q)
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}
	start, end, detail := parseUsageWindow(q, auditDefaultRange, h.now())
	if detail != "" {
		writeInvalidRequest(w, detail)
		return
	}

	args := []any{start, end}
	clauses := []string{"created_at >= $1", "created_at < $2"}
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if !viewer.admin {
		add("user_id = $%d", viewer.userID)
	} else if username := strings.TrimSpace(q.Get("username")); username != "" {
		// audit_logs.username 是写入时快照，直接精确过滤（用户删除后仍可审计）。
		add("username = $%d", username)
	}
	if category != "" {
		add("category = $%d", category)
	}
	if result != "" {
		add("result = $%d", result)
	}
	where := " WHERE " + strings.Join(clauses, " AND ")

	var total int64
	if err := h.logDB.QueryRowContext(r.Context(), `SELECT count(*) FROM audit_logs`+where, args...).Scan(&total); err != nil {
		h.log.Error("audit logs: count failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := h.logDB.QueryContext(r.Context(), `
		SELECT id, user_id, username, category, ip, user_agent, method, route,
		       status_code, result, detail, before, after, created_at
		FROM audit_logs`+where+fmt.Sprintf(`
		ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		h.log.Error("audit logs: query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	defer rows.Close()

	items := make([]auditLogItem, 0, pageSize)
	for rows.Next() {
		var (
			item                              auditLogItem
			username                          string
			ip, userAgent, method, route      sql.NullString
			statusCode                        sql.NullInt64
			detailJSON, beforeJSON, afterJSON []byte
		)
		if err := rows.Scan(&item.ID, &item.UserID, &username, &item.Category,
			&ip, &userAgent, &method, &route, &statusCode, &item.Result,
			&detailJSON, &beforeJSON, &afterJSON, &item.CreatedAt); err != nil {
			h.log.Error("audit logs: scan failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
			return
		}
		item.IP, item.UserAgent = nullableString(ip), nullableString(userAgent)
		item.Method, item.Route = nullableString(method), nullableString(route)
		item.StatusCode = nullableInt(statusCode)
		if viewer.admin {
			// 用户视图隐藏 username 列（M05 §7.4：仅管理员渲染）。
			item.Username = &username
		}
		item.Detail = rawJSONOrNull(detailJSON)
		item.Before = rawJSONOrNull(beforeJSON)
		item.After = rawJSONOrNull(afterJSON)
		item.CreatedAt = item.CreatedAt.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("audit logs: rows failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, auditLogPage{Items: items, Total: total, Page: page, PageSize: pageSize})
}

func rawJSONOrNull(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

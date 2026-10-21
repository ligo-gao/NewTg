// 钱包流水读取面（W7/M3，modules-03 §F-03-02/F-03-03/F-03-04）。
//
//	GET /api/wallet/transactions       列表：11 类 type、5 状态、方向、时间窗、
//	                                   |amount| 区间、关联号筛选 + offset 分页
//	GET /api/wallet/transactions/{id}  详情：行字段 + 服务端一次组装
//	                                   usage/order/redemption/adjustment/segments/
//	                                   corrects_transaction_id（R-M3-04）
//
// 数据源分工：wallet_transactions / orders / redemption_codes 在主库；
// usage_logs 在日志库。日志库只用于「辅助上下文」，查询失败降级为
// usage=null，绝不把资金行变成 500 或 404（R-M3-04）。
//
// 安全口径：
//   - 身份只取 auth.PrincipalFrom；接口形状不含 user_id，传了也忽略；
//   - 列表/详情/日志上下文/关联反查的每条 SQL 都带 user_id（IDOR 硬线）；
//   - 详情「不存在 / 他人 / 非法 id」统一 404，不区分原因；
//   - admin_adjustment 的机器指纹 note（`admin adjustment: <mode> idem:<64hex>`）
//     只解析 mode，指纹段绝不回显（R-M3-05）；redemption 不返回 code_hash。
package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/store"
)

const (
	// 钱包流水默认时间窗 30 天、硬上限 90 天：与模块 05 的
	// MAX_RANGE_DAYS=90 同一口径（modules-03 F-03-02）。
	walletDefaultWindow = 30 * 24 * time.Hour
	walletMaxWindow     = 90 * 24 * time.Hour

	// 共享 offset 分页契约（components/pagination.yaml）：page_size 1..100。
	// UI 只暴露 10/20/50（R-M3-10），API 不拒绝其它合法值。
	walletDefaultPageSize = 20
	walletMaxPageSize     = 100

	// walletRefMaxRunes 对齐 wallet_transactions.reference_id varchar(64)。
	walletRefMaxRunes = 64

	// R-M3-11：用户维读限流 60 次/分钟，列表与详情共用同一桶。键取用户维
	// 而不是 IP 维：登录用户可能共享出口 IP，按 IP 会让同一办公网用户互相
	// 挤占；用户维也与「只看得到自己的流水」的数据范围一致。
	WalletReadRateLimit  = 60
	WalletReadRateWindow = time.Minute
	WalletReadRatePrefix = "rateLimit:v2:user:WT:"

	// 详情 404 统一出口文案（auth 域已有 401 helper，此处只补 404 语义）。
	walletTransactionTypeAdminAdjustment = "admin_adjustment"
	walletTransactionTypeReversal        = "reversal"
	walletTransactionTypePurchase        = "subscription_purchase"
)

// 枚举真源来自 store.EnumChecks（与 DB CHECK 同表维护），避免读取面
// 另抄一份后漂移。subscription_purchase 由迁移 20261020230000 放开，
// 已登记 docs/未完成清单 G-DOM-48。
var (
	walletTransactionTypes    = enumValueSet(store.EnumChecks["wallet_transactions.type"])
	walletTransactionStatuses = enumValueSet(store.EnumChecks["wallet_transactions.status"])
)

func enumValueSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// UUID 形状判定与前端 features/logs/search.ts 的 isUuid 同口径：32 位十六
// 进制（网关 mint 的 requestID）或 8-4-4-4-12。R-M3-07 要求后端也做去连字
// 符归一，否则用户粘贴带连字符的 requestID 永远查不到。
var (
	walletCompactUUID    = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	walletHyphenatedUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func walletUUIDShaped(value string) bool {
	return walletCompactUUID.MatchString(value) || walletHyphenatedUUID.MatchString(value)
}

func walletNormalizeRef(value string) string {
	return strings.ToLower(strings.ReplaceAll(value, "-", ""))
}

// walletTransactionView 是列表行投影：不含 note / user_id。note 可能带
// 内部标记（billing:settle / billing:refund），用户侧列表没有展示语义。
type walletTransactionView struct {
	ID            int64     `json:"id"`
	Type          string    `json:"type"`
	Amount        int64     `json:"amount"`
	BalanceAfter  int64     `json:"balance_after"`
	Status        string    `json:"status"`
	ReferenceType *string   `json:"reference_type"`
	ReferenceID   *string   `json:"reference_id"`
	CreatedAt     time.Time `json:"created_at"`
}

type walletTransactionPage struct {
	Items    []walletTransactionView `json:"items"`
	Total    int64                   `json:"total"`
	Page     int                     `json:"page"`
	PageSize int                     `json:"page_size"`
}

type walletUsageContextView struct {
	LogID            int64  `json:"log_id"`
	RequestID        string `json:"request_id"`
	Model            string `json:"model"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	Cost             int64  `json:"cost"`
	// pre_deducted / refunded 在 usage_logs 可为 NULL（无预扣的跨组补扣、
	// 未结算请求）；保持 null 而不是伪造成 0。
	PreDeducted *int64 `json:"pre_deducted"`
	Refunded    *int64 `json:"refunded"`
}

type walletOrderContextView struct {
	ID       int64  `json:"id"`
	Status   string `json:"status"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Provider string `json:"provider"`
	BizKind  string `json:"biz_kind"`
}

// walletRedemptionContextView 刻意不含 code_hash：兑换码只以批次名与
// 积分数呈现，哈希不进入用户侧投影。
type walletRedemptionContextView struct {
	ID        int64   `json:"id"`
	BatchName *string `json:"batch_name"`
	Credits   int64   `json:"credits"`
}

type walletAdjustmentContextView struct {
	Mode string `json:"mode"`
}

type walletTransactionSegmentsView struct {
	Held     int64 `json:"held"`
	Charged  int64 `json:"charged"`
	Refunded int64 `json:"refunded"`
}

type walletTransactionDetailView struct {
	walletTransactionView
	// Note 仅 admin_adjustment 且非机器格式时返回；机器格式的指纹段一律
	// 剥离（R-M3-05）。其它类型的内部标记 note 恒为 null。
	Note                  *string                        `json:"note"`
	Usage                 *walletUsageContextView        `json:"usage"`
	Order                 *walletOrderContextView        `json:"order"`
	Redemption            *walletRedemptionContextView   `json:"redemption"`
	Adjustment            *walletAdjustmentContextView   `json:"adjustment"`
	Segments              *walletTransactionSegmentsView `json:"segments"`
	CorrectsTransactionID *int64                         `json:"corrects_transaction_id"`
}

// walletTransactionFilter 是解析后的查询条件；userID 由 Principal 注入，
// 不存在任何外部传参路径。
type walletTransactionFilter struct {
	userID int64
	from   time.Time
	to     time.Time
	typ    string
	status string
	dir    string
	min    *int64
	max    *int64
	ref    string
}

// whereClause 返回列表与计数共享的 WHERE 子句。参数按 $1..$n 顺序追加，
// 调用方只在末尾追加 LIMIT/OFFSET，保证 count 与 page 两查询条件完全一致。
func (f walletTransactionFilter) whereClause() (string, []any) {
	clauses := []string{"user_id = $1", "created_at >= $2", "created_at < $3"}
	args := []any{f.userID, f.from, f.to}
	add := func(format string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(format, len(args)))
	}
	if f.typ != "" {
		add("type = $%d", f.typ)
	}
	if f.status != "" {
		add("status = $%d", f.status)
	}
	switch f.dir {
	case "in":
		clauses = append(clauses, "amount > 0")
	case "out":
		clauses = append(clauses, "amount < 0")
	}
	// R-M3-09：min/max 作用于 ABS(amount)，与 dir 正交；闭区间。
	if f.min != nil {
		add("ABS(amount) >= $%d", *f.min)
	}
	if f.max != nil {
		add("ABS(amount) <= $%d", *f.max)
	}
	if f.ref != "" {
		if walletUUIDShaped(f.ref) {
			// R-M3-07：原样精确匹配优先（可走 (reference_type, reference_id)
			// 索引），再补一次去连字符/大小写归一比较，让带连字符的 UUID
			// 也能命中 32-hex 存储值。
			args = append(args, f.ref)
			exact := len(args)
			args = append(args, walletNormalizeRef(f.ref))
			normalized := len(args)
			clauses = append(clauses, fmt.Sprintf(
				"(reference_id = $%d OR lower(replace(reference_id, '-', '')) = $%d)",
				exact, normalized))
		} else {
			// 非 UUID 关联号（订单/兑换码/admin 幂等键）保持原样精确匹配。
			add("reference_id = $%d", f.ref)
		}
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// nowTime 返回 UTC 时钟；测试可用 Now 注入固定时刻验证默认窗口。
func (h *WalletHandlers) nowTime() time.Time {
	if h.now != nil {
		return h.now().UTC()
	}
	return time.Now().UTC()
}

// walletPrincipal 是钱包三个端点共用的身份出口：只认 Principal，绝无
// user_id 查询参数旁路。
func walletPrincipal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return nil, false
	}
	return p, true
}

// allowWalletRead 消耗用户维读桶；返回 true 表示已写出 429。
// Redis 故障 fail-open + warn：这是只读面，限流失效只放大查询压力。
func (h *WalletHandlers) allowWalletRead(w http.ResponseWriter, r *http.Request, userID int64) bool {
	if h.rdb == nil {
		return false
	}
	key := WalletReadRatePrefix + strconv.FormatInt(userID, 10)
	limited, retry, err := allowRequestRate(r.Context(), h.rdb, key,
		WalletReadRateLimit, WalletReadRateWindow)
	if err != nil {
		h.log.Warn("wallet transactions: rate limiter unavailable; allowing request",
			"user_id", userID, "error", err)
		return false
	}
	if !limited {
		return false
	}
	writeRateLimited(w, retry, "Too many requests")
	return true
}

// --- 列表 -----------------------------------------------------------------

func (h *WalletHandlers) handleTransactionList(w http.ResponseWriter, r *http.Request) {
	p, ok := walletPrincipal(w, r)
	if !ok {
		return
	}
	if h.allowWalletRead(w, r, p.UserID) {
		return
	}
	filter, page, pageSize, detail := parseWalletTransactionQuery(r.URL.Query(), h.nowTime())
	if detail != "" {
		// R-M3-02：非法查询参数统一 422（与 /api/usage-logs 同口径）。
		writeInvalidRequest(w, detail)
		return
	}
	filter.userID = p.UserID

	view, err := h.readTransactionPage(r.Context(), filter, page, pageSize)
	if err != nil {
		h.log.Error("wallet transactions: list failed", "user_id", p.UserID, "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *WalletHandlers) readTransactionPage(
	ctx context.Context,
	filter walletTransactionFilter,
	page, pageSize int,
) (walletTransactionPage, error) {
	where, args := filter.whereClause()

	var total int64
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM wallet_transactions`+where, args...,
	).Scan(&total); err != nil {
		return walletTransactionPage{}, fmt.Errorf("count: %w", err)
	}

	// 设计验收（modules-03 F-03-02）：p 越界回落最后一页，而不是返回空页。
	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize

	args = append(args, pageSize, offset)
	query := `SELECT id, type, amount, balance_after, status, reference_type,
		reference_id, created_at FROM wallet_transactions` + where +
		fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
			len(args)-1, len(args))
	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		return walletTransactionPage{}, fmt.Errorf("list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 空结果必须是 []，不能是 null（契约 items 恒为数组；变异 G）。
	items := make([]walletTransactionView, 0, pageSize)
	for rows.Next() {
		var item walletTransactionView
		var referenceType, referenceID sql.NullString
		if err := rows.Scan(
			&item.ID, &item.Type, &item.Amount, &item.BalanceAfter, &item.Status,
			&referenceType, &referenceID, &item.CreatedAt,
		); err != nil {
			return walletTransactionPage{}, fmt.Errorf("scan: %w", err)
		}
		item.ReferenceType = walletNullableString(referenceType)
		item.ReferenceID = walletNullableString(referenceID)
		item.CreatedAt = item.CreatedAt.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return walletTransactionPage{}, fmt.Errorf("rows: %w", err)
	}
	return walletTransactionPage{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// parseWalletTransactionQuery 解析并校验全部查询参数。返回非空 detail 表示
// 422；错误文案是固定英文串，不回显原始输入（避免把用户输入反射回响应）。
func parseWalletTransactionQuery(q url.Values, now time.Time) (
	walletTransactionFilter, int, int, string,
) {
	var filter walletTransactionFilter
	page, pageSize := 1, walletDefaultPageSize

	if raw := strings.TrimSpace(q.Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return filter, 0, 0, "p must be a positive integer"
		}
		page = value
	}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > walletMaxPageSize {
			return filter, 0, 0, "page_size must be between 1 and 100"
		}
		pageSize = value
	}

	if raw := strings.TrimSpace(q.Get("type")); raw != "" {
		if _, ok := walletTransactionTypes[raw]; !ok {
			return filter, 0, 0, "type is not a supported transaction type"
		}
		filter.typ = raw
	}
	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		if _, ok := walletTransactionStatuses[raw]; !ok {
			return filter, 0, 0, "status is not a supported transaction status"
		}
		filter.status = raw
	}
	if raw := strings.TrimSpace(q.Get("dir")); raw != "" {
		if raw != "in" && raw != "out" {
			return filter, 0, 0, "dir must be in or out"
		}
		filter.dir = raw
	}

	// 时间窗：to 默认 now；from 缺省时相对 to 回退 30 天（只给 to 的请求
	// 仍描述「以 to 结尾的 30 天窗口」）。区间左闭右开，全部按 UTC。
	to := now
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, 0, 0, "to must be an RFC3339 timestamp"
		}
		to = parsed.UTC()
	}
	from := to.Add(-walletDefaultWindow)
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, 0, 0, "from must be an RFC3339 timestamp"
		}
		from = parsed.UTC()
	}
	if to.Before(from) {
		return filter, 0, 0, "to must not be before from"
	}
	if to.Sub(from) > walletMaxWindow {
		return filter, 0, 0, "time range must not exceed 90 days"
	}
	filter.from, filter.to = from, to

	minValue, err := parseWalletAmountBound(q.Get("min"))
	if err != "" {
		return filter, 0, 0, "min must be a non-negative integer"
	}
	maxValue, err := parseWalletAmountBound(q.Get("max"))
	if err != "" {
		return filter, 0, 0, "max must be a non-negative integer"
	}
	if minValue != nil && maxValue != nil && *maxValue < *minValue {
		return filter, 0, 0, "max must not be less than min"
	}
	filter.min, filter.max = minValue, maxValue

	// ref 按原样比较（不 trim）：UUID 形状才做归一（R-M3-07）；超过
	// varchar(64) 的值不可能命中，直接 422。
	if ref := q.Get("ref"); ref != "" {
		if utf8.RuneCountInString(ref) > walletRefMaxRunes {
			return filter, 0, 0, "ref must not exceed 64 characters"
		}
		filter.ref = ref
	}
	return filter, page, pageSize, ""
}

// parseWalletAmountBound 解析 min/max：空串 → nil；负数/非整数 → 错误串。
func parseWalletAmountBound(raw string) (*int64, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return nil, "invalid"
	}
	return &value, ""
}

// --- 详情 -----------------------------------------------------------------

func (h *WalletHandlers) handleTransactionDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := walletPrincipal(w, r)
	if !ok {
		return
	}
	if h.allowWalletRead(w, r, p.UserID) {
		return
	}
	// 非法 id 与不存在/他人同口径 404：不提供存在性探测入口。
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found")
		return
	}

	view, err := h.readTransactionDetail(r.Context(), p.UserID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found")
			return
		}
		h.log.Error("wallet transaction detail failed", "user_id", p.UserID, "id", id, "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// walletTransactionRow 是详情扫描目标（列表不读 note）。
type walletTransactionRow struct {
	ID            int64
	Type          string
	Amount        int64
	BalanceAfter  int64
	Status        string
	ReferenceType sql.NullString
	ReferenceID   sql.NullString
	Note          sql.NullString
	CreatedAt     time.Time
}

func (row walletTransactionRow) view() walletTransactionView {
	return walletTransactionView{
		ID:            row.ID,
		Type:          row.Type,
		Amount:        row.Amount,
		BalanceAfter:  row.BalanceAfter,
		Status:        row.Status,
		ReferenceType: walletNullableString(row.ReferenceType),
		ReferenceID:   walletNullableString(row.ReferenceID),
		CreatedAt:     row.CreatedAt.UTC(),
	}
}

// readTransactionDetail 一次组装详情（R-M3-04）。主库查询失败原样上抛
// （500）；日志上下文失败降级为 null，由 readUsageContext 内部吞掉。
func (h *WalletHandlers) readTransactionDetail(
	ctx context.Context, userID, id int64,
) (walletTransactionDetailView, error) {
	var row walletTransactionRow
	// IDOR 硬线：id 与 user_id 必须同时出现在 WHERE 里（变异 A）。
	err := h.db.QueryRowContext(ctx, `
		SELECT id, type, amount, balance_after, status, reference_type,
		       reference_id, note, created_at
		FROM wallet_transactions
		WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(
		&row.ID, &row.Type, &row.Amount, &row.BalanceAfter, &row.Status,
		&row.ReferenceType, &row.ReferenceID, &row.Note, &row.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return walletTransactionDetailView{}, sql.ErrNoRows
		}
		return walletTransactionDetailView{}, fmt.Errorf("detail row: %w", err)
	}

	view := walletTransactionDetailView{walletTransactionView: row.view()}
	referenceType, referenceID := "", ""
	if row.ReferenceType.Valid {
		referenceType = row.ReferenceType.String
	}
	if row.ReferenceID.Valid {
		referenceID = row.ReferenceID.String
	}

	if row.Type == walletTransactionTypeAdminAdjustment && row.Note.Valid {
		mode, human := parseAdminAdjustmentNote(row.Note.String)
		if human != nil {
			view.Note = human
		}
		if mode != "" {
			view.Adjustment = &walletAdjustmentContextView{Mode: mode}
		}
	}

	switch referenceType {
	case "usage_log":
		segments, err := h.readUsageSegments(ctx, userID, referenceID)
		if err != nil {
			return walletTransactionDetailView{}, err
		}
		view.Segments = &segments
		// 日志库不可用/缺行 → usage=null；资金行仍 200。
		view.Usage = h.readUsageContext(ctx, userID, referenceID)
	case "order":
		order, err := h.readOrderContext(ctx, userID, referenceID)
		if err != nil {
			return walletTransactionDetailView{}, err
		}
		view.Order = order
	case "redemption":
		redemption, err := h.readRedemptionContext(ctx, userID, referenceID)
		if err != nil {
			return walletTransactionDetailView{}, err
		}
		view.Redemption = redemption
	}

	// R-M3-06：reversal 在实现里是订单级冲正（reference_type='order'、
	// reference_id=order.id）。反查同订单的 subscription_purchase 行作为
	// 「被修正行」，不改历史数据、不改写入路径。
	if row.Type == walletTransactionTypeReversal && referenceType == "order" {
		corrects, err := h.readCorrectsTransactionID(ctx, userID, referenceID)
		if err != nil {
			return walletTransactionDetailView{}, err
		}
		view.CorrectsTransactionID = corrects
	}
	return view, nil
}

// readUsageSegments 按同一 (user_id, requestID) 聚合三段金额（F-03-03）：
//
//	held     = −Σ(consume_pre)
//	charged  = held + (−Σ(consume)) − Σ(release)
//	refunded = Σ(release)
//
// 例：预扣 12,000、退回 2,760 → held=12,000 / charged=9,240 / refunded=2,760；
// 失败全退 → 0/0/12,000；少补 500 → 12,000/12,500/0；无预扣跨组补扣 →
// 0/1,000/0。三段都是界面金额，钳到非负以防异常历史行渲染出负的「预扣」。
func (h *WalletHandlers) readUsageSegments(
	ctx context.Context, userID int64, requestID string,
) (walletTransactionSegmentsView, error) {
	var held, consumed, refunded int64
	err := h.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN type = 'consume_pre' THEN -amount ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN type = 'consume' THEN -amount ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN type = 'release' THEN amount ELSE 0 END), 0)
		FROM wallet_transactions
		WHERE user_id = $1 AND reference_type = 'usage_log' AND reference_id = $2`,
		userID, requestID,
	).Scan(&held, &consumed, &refunded)
	if err != nil {
		return walletTransactionSegmentsView{}, fmt.Errorf("segments: %w", err)
	}
	segments := walletTransactionSegmentsView{
		Held:     held,
		Charged:  held + consumed - refunded,
		Refunded: refunded,
	}
	if segments.Held < 0 {
		segments.Held = 0
	}
	if segments.Charged < 0 {
		segments.Charged = 0
	}
	if segments.Refunded < 0 {
		segments.Refunded = 0
	}
	return segments, nil
}

// readUsageContext 查日志库中的调用摘要。任何失败（连接、缺行、非法 UUID
// 形状）都返回 nil：usage 是辅助信息，不能让资金行不可读。
func (h *WalletHandlers) readUsageContext(
	ctx context.Context, userID int64, requestID string,
) *walletUsageContextView {
	if h.logDB == nil || !walletUUIDShaped(requestID) {
		return nil
	}
	var view walletUsageContextView
	var preDeducted, refunded sql.NullInt64
	// 安全硬线：request_id 与 user_id 同时过滤，防止同 requestID 的异常跨
	// 用户日志行泄露。时间上不设窗口（钱包流水可能早于日志端点的 90 天）。
	err := h.logDB.QueryRowContext(ctx, `
		SELECT id, request_id, model,
		       COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0),
		       COALESCE(cost, 0), pre_deducted, refunded
		FROM usage_logs
		WHERE request_id = $1::uuid AND user_id = $2
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, requestID, userID,
	).Scan(
		&view.LogID, &view.RequestID, &view.Model,
		&view.PromptTokens, &view.CompletionTokens, &view.Cost,
		&preDeducted, &refunded,
	)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			h.log.Warn("wallet transaction detail: usage context unavailable",
				"user_id", userID, "request_id", requestID, "error", err)
		}
		return nil
	}
	if preDeducted.Valid {
		value := preDeducted.Int64
		view.PreDeducted = &value
	}
	if refunded.Valid {
		value := refunded.Int64
		view.Refunded = &value
	}
	return &view
}

// readOrderContext 返回关联订单的只读摘要。reference_id 不是合法订单号时
// 视为缺行（null），不发起查询。
func (h *WalletHandlers) readOrderContext(
	ctx context.Context, userID int64, referenceID string,
) (*walletOrderContextView, error) {
	orderID, err := strconv.ParseInt(referenceID, 10, 64)
	if err != nil || orderID <= 0 {
		return nil, nil
	}
	var view walletOrderContextView
	err = h.db.QueryRowContext(ctx, `
		SELECT id, status, amount, currency, provider, biz_kind
		FROM orders
		WHERE id = $1 AND user_id = $2`, orderID, userID,
	).Scan(&view.ID, &view.Status, &view.Amount, &view.Currency, &view.Provider, &view.BizKind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("order context: %w", err)
	}
	// char(3) 列在部分驱动下会带尾部空格。
	view.Currency = strings.TrimSpace(view.Currency)
	return &view, nil
}

// readRedemptionContext 返回兑换码的批次名与积分数。刻意不查 code_hash；
// 额外的 used_by 过滤是纵深防御：即使 reference_id 被污染，也不会看到
// 别人核销的码。
func (h *WalletHandlers) readRedemptionContext(
	ctx context.Context, userID int64, referenceID string,
) (*walletRedemptionContextView, error) {
	codeID, err := strconv.ParseInt(referenceID, 10, 64)
	if err != nil || codeID <= 0 {
		return nil, nil
	}
	var view walletRedemptionContextView
	var batchName sql.NullString
	err = h.db.QueryRowContext(ctx, `
		SELECT id, batch_name, credits
		FROM redemption_codes
		WHERE id = $1 AND used_by = $2`, codeID, userID,
	).Scan(&view.ID, &batchName, &view.Credits)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("redemption context: %w", err)
	}
	view.BatchName = walletNullableString(batchName)
	return &view, nil
}

// readCorrectsTransactionID 反查同订单的余额购买流水（取最新 id）。
// 找不到返回 nil（例如订单不是余额购买）：详情仍可读，只是没有可跳转行。
func (h *WalletHandlers) readCorrectsTransactionID(
	ctx context.Context, userID int64, orderID string,
) (*int64, error) {
	var id int64
	err := h.db.QueryRowContext(ctx, `
		SELECT id FROM wallet_transactions
		WHERE type = $1 AND reference_type = 'order' AND reference_id = $2
		  AND user_id = $3
		ORDER BY id DESC
		LIMIT 1`, walletTransactionTypePurchase, orderID, userID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("corrects transaction: %w", err)
	}
	return &id, nil
}

// parseAdminAdjustmentNote 剥离 machine note 的指纹段（R-M3-05）。
//
// 写路径（handlers_users.go）落库格式为
// `admin adjustment: <add|subtract|override> idem:<64hex>`；指纹是幂等键与
// 请求语义的 SHA-256，绝不能出现在用户侧响应。返回：
//   - mode：可识别的调整方式，失败为空串；
//   - human：可展示的人类理由（仅当 note 不是机器格式）。
func parseAdminAdjustmentNote(note string) (mode string, human *string) {
	const prefix = "admin adjustment: "
	if idx := strings.Index(note, adminAdjustmentFingerprintMarker); idx >= 0 {
		head := note[:idx]
		if strings.HasPrefix(head, prefix) {
			candidate := strings.TrimSpace(strings.TrimPrefix(head, prefix))
			if validAdminAdjustmentMode(candidate) {
				mode = candidate
			}
		}
		// 机器格式：指纹段与机器前缀都不回显；理由/操作人当前无落库（M3
		// 登记 module 07 写路径缺口）。
		return mode, nil
	}
	if strings.HasPrefix(note, prefix) {
		candidate := strings.TrimSpace(strings.TrimPrefix(note, prefix))
		if validAdminAdjustmentMode(candidate) {
			// 无指纹的机器前缀同样不当作人类理由展示。
			return candidate, nil
		}
	}
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return "", nil
	}
	return "", &trimmed
}

func validAdminAdjustmentMode(mode string) bool {
	switch mode {
	case "add", "subtract", "override":
		return true
	default:
		return false
	}
}

func walletNullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

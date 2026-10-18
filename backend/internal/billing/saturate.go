package billing

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
)

// DBLogger 是 usage_logs 的落库实现（Task 3 的 billing.Saturator）。
//
// 日志库独立于主库（DB_LOGS_DSN）：本类型只持有日志库句柄，**不做也不宣称
// 任何跨库事务**。写入失败返回 error，由 Terminate 决定「不结算、保留 hold」
// 或由 worker 重试（见 engine.go 中 Saturator 的注释）。
type DBLogger struct {
	logsDB *sql.DB
}

// NewDBLogger 构造日志落库器。logsDB 为 nil 时 Saturate 返回错误而不是 panic。
func NewDBLogger(logsDB *sql.DB) *DBLogger { return &DBLogger{logsDB: logsDB} }

// 编译期断言：DBLogger 必须实现结算日志接缝。
var _ Saturator = (*DBLogger)(nil)

// usageLogReservedPublicKeys 是 public 区禁止出现的键（billing-07 §7.1）。
//
// 三个分区名本身 + 四个遗留敏感键。本文件只写自己构造的键，这里仍做一次
// 防御性拦截：写入方一旦被复用/扩展，也不会把特权或渠道敏感信息平铺到
// 普通用户可见的 public 区。
var usageLogReservedPublicKeys = map[string]struct{}{
	"admin_info":    {},
	"root_info":     {},
	"audit_info":    {},
	"channel_id":    {},
	"channel_name":  {},
	"channel_type":  {},
	"reject_reason": {},
}

// usageLogRow 是一条 usage_logs 行的写投影（id/created_at 由数据库生成）。
//
// token_name/upstream/retry_chain 在 M3 没有可靠数据源：RelayContext 不携带
// 令牌名与上游显示名，重试链历史也只有中继逐次尝试才能组装。三列可空，
// 因此显式写 NULL 而不是编造占位值（platform-01 §12）。
type usageLogRow struct {
	requestID        string
	userID           int64
	tokenID          sql.NullInt64
	channelID        sql.NullInt64
	model            string
	group            sql.NullString
	promptTokens     int
	completionTokens int
	cacheReadTokens  int
	cacheWriteTokens int
	preDeducted      int64
	cost             int64
	refunded         int64
	latencyMS        sql.NullInt64
	firstTokenMS     sql.NullInt64
	stream           bool
	status           int16
	errorClass       sql.NullString
	errorMessage     sql.NullString
	other            string
}

// Saturate 写入一条 usage_logs 记录（platform-01 §12 的 25 列）。
//
// 【顺序】Terminate 固定「先 Saturate、后主库结算」（有 hold 路径）；本方法
// 因此必须幂等、可重试。无 hold 的补扣路径反过来「先扣款、后写日志」，
// 失败时钱已收、日志缺失，属已登记的残余风险（R-38）。
//
// 【幂等与跨库竞态 R-25】usage_logs 的 request_id 只有普通索引（R5），
// 没有唯一约束，重复 INSERT 不会被数据库拦住。写终态前先在事务内加
// request_id 作用域的**事务级咨询锁**并读取已有行：
//   - 已有终态行且本次不是 refunded → 直接跳过（幂等命中；绝不把 refunded
//     覆盖成 success）；
//   - 本次是 refunded（worker 兜底退款）→ 允许把已有行**单向收敛**为
//     refunded、cost=0（billing-06 §5.3 第 6 步），refunded 金额只增不减；
//   - 没有任何行 → 插入。
//
// 咨询锁随事务提交/回滚自动释放；所有写 usage_logs 的执行者（Terminate 与
// T5 worker 的退款占位）都必须经本方法写，才能共享同一把锁。
func (l *DBLogger) Saturate(ctx context.Context, fact *contract.UsageFact,
	rc *contract.RelayContext, outcome SettlementOutcome) error {

	if l == nil || l.logsDB == nil {
		return errors.New("billing: DBLogger has no logs database")
	}
	row, err := buildUsageLogRow(fact, rc, outcome)
	if err != nil {
		return err
	}
	if err := l.writeUsageLog(ctx, row); err != nil {
		return fmt.Errorf("billing: saturate usage log for request %s: %w", row.requestID, err)
	}
	return nil
}

// buildUsageLogRow 把用量事实/请求上下文/结算结果投影成一行日志。
//
// 校验失败返回 error（调用方据此不结算）：负金额、未知 status、缺 requestID
// 都会破坏日志与账本的对账语义，不能静默落库。
func buildUsageLogRow(fact *contract.UsageFact,
	rc *contract.RelayContext, outcome SettlementOutcome) (usageLogRow, error) {

	if rc == nil || strings.TrimSpace(rc.RequestID) == "" {
		return usageLogRow{}, fmt.Errorf("%w: usage log requires a request id", ErrMissingRequestID)
	}
	if outcome.Status < UsageLogStatusRefused || outcome.Status > UsageLogStatusRefunded {
		return usageLogRow{}, fmt.Errorf("billing: invalid usage log status %d", outcome.Status)
	}
	if outcome.Cost < 0 || outcome.PreDeducted < 0 || outcome.Refunded < 0 {
		return usageLogRow{}, fmt.Errorf(
			"billing: negative usage log amounts (cost=%d pre_deducted=%d refunded=%d)",
			outcome.Cost, outcome.PreDeducted, outcome.Refunded)
	}

	// status 语义（platform-01 §12）：只有 success 的 cost 是实际消耗；
	// refused（预扣前拒绝）、error（上游失败全退）、refunded（worker 兜底）
	// 的 cost 一律为 0。这里强制归零而不是信任调用方，避免出现
	// 「status=error 但 cost>0」的对账矛盾。
	cost := outcome.Cost
	if outcome.Status != UsageLogStatusSuccess {
		cost = 0
	}

	stream := rc.Streaming
	latency := sql.NullInt64{}
	firstToken := sql.NullInt64{}
	if fact != nil {
		stream = fact.Stream
		if fact.LatencyMS >= 0 {
			latency = sql.NullInt64{Int64: int64(fact.LatencyMS), Valid: true}
		}
		// 非流式按契约不得有 TTFT（platform-01 §12：非流式为 null）。
		if stream && fact.FirstTokenMS != nil && *fact.FirstTokenMS >= 0 {
			firstToken = sql.NullInt64{Int64: int64(*fact.FirstTokenMS), Valid: true}
		}
	}

	model := ""
	if fact != nil {
		model = fact.Model
	}
	if model == "" {
		model = rc.OriginModel
	}
	if model == "" {
		model = rc.UpstreamModel
	}
	// varchar(128)/varchar(32)：超长模型名/分组名若原样写入会让整条日志
	// 插入失败，从而阻塞结算。日志列是展示/统计用途，截断是安全方向。
	model = truncateLogText(model, 128)

	group := rc.UsingGroup
	if group == "" {
		group = rc.UserGroup
	}

	row := usageLogRow{
		requestID:        strings.TrimSpace(rc.RequestID),
		userID:           int64(rc.UserID),
		tokenID:          nullInt64(int64(rc.TokenID)),
		channelID:        nullInt64(int64(rc.ChannelID)),
		model:            model,
		group:            nullString(truncateLogText(group, 32)),
		promptTokens:     clampTokenCount(int64(usageField(fact, func(u *contract.Usage) int64 { return u.Prompt }))),
		completionTokens: clampTokenCount(int64(usageField(fact, func(u *contract.Usage) int64 { return u.Completion }))),
		cacheReadTokens:  clampTokenCount(int64(usageField(fact, func(u *contract.Usage) int64 { return u.CacheRead }))),
		cacheWriteTokens: clampTokenCount(int64(usageField(fact, func(u *contract.Usage) int64 { return u.CacheWrite }))),
		preDeducted:      outcome.PreDeducted,
		cost:             cost,
		refunded:         outcome.Refunded,
		latencyMS:        latency,
		firstTokenMS:     firstToken,
		stream:           stream,
		status:           outcome.Status,
		errorClass:       nullString(truncateLogText(outcome.ErrorClass, 255)),
		errorMessage:     nullString(truncateLogText(outcome.ErrorMessage, 500)),
	}

	other, err := json.Marshal(buildUsageLogOther(rc, model, firstToken))
	if err != nil {
		// other 只由本文件构造（字符串/数字/嵌套 map），Marshal 失败只可能
		// 来自 QuotaClamp.Original 之类的非有限值；ClampAuditMap 已做 JSON
		// 安全化，此分支是最后一道防线，不把半成品日志写进库。
		return usageLogRow{}, fmt.Errorf("billing: marshal usage log other: %w", err)
	}
	row.other = string(other)
	return row, nil
}

// usageField 安全读取 Usage 的某个 int64 字段（fact 为 nil 时返回 0）。
func usageField(fact *contract.UsageFact, pick func(*contract.Usage) int64) int64 {
	if fact == nil {
		return 0
	}
	return pick(&fact.Usage)
}

// buildUsageLogOther 构造 usage_logs.other 的四分区投影（billing-07 §6/§7）。
//
// 本任务只写有可靠来源的键，不臆造字段：
//   - public 顶层平铺：request_path / frt / upstream_model_name /
//     is_model_mapped / applied_discount / billing_source /
//     usage_semantic / billing_mode / expr_b64；
//   - admin_info：quota_saturation（4 键 op/kind/original/clamped）。
//
// **禁止 estimated_* 键**（billing-00 §11、billing-07 §6.9）：估算值在结果
// 缺失时复用同名键，不引入新前缀。本函数不写任何 estimated_ 前缀键。
//
// 【已知契约缺口】contract.RelayContext 只有 Discount float64，没有
// DiscountTarget 字段（billing-99 R-5）。因此日志只能记录 0<rate<1 的
// applied_discount；rate=0 的合法全免与「未命中」在上下文里无法区分，
// discount_target 也无处取值。该缺口登记在 task-6-report，待契约补字段后
// 由本函数补写（billing-04 §2.6 要求的两个 public 键）。
func buildUsageLogOther(rc *contract.RelayContext,
	model string, firstToken sql.NullInt64) map[string]any {

	other := make(map[string]any, 6)

	if rc.RequestPath != "" {
		setPublicKey(other, "request_path", rc.RequestPath)
	}
	if firstToken.Valid {
		setPublicKey(other, "frt", firstToken.Int64)
	}
	// 上游模型与请求模型不同才写映射标记（billing-07 §6.5）。
	if rc.UpstreamModel != "" && rc.UpstreamModel != model {
		setPublicKey(other, "upstream_model_name", rc.UpstreamModel)
		setPublicKey(other, "is_model_mapped", true)
	}
	// M3 只有钱包资金源（订阅属 M5）；写明来源供对账。
	setPublicKey(other, "billing_source", "wallet")

	// 折扣日志：0<rate<1 才可判定为「命中且未全免」；rate=0 与零值无法
	// 区分（见上方契约缺口注释），不在本函数猜测。
	if rc.Discount > 0 && rc.Discount < 1 && !math.IsNaN(rc.Discount) {
		setPublicKey(other, "applied_discount", rc.Discount)
	}

	if snap := rc.PriceSnapshot; snap != nil {
		if snap.Mode == contract.BillingModeTieredExpr && snap.ExprString != "" {
			setPublicKey(other, "billing_mode", "tiered_expr")
			setPublicKey(other, "expr_b64", base64.StdEncoding.EncodeToString([]byte(snap.ExprString)))
		}
		if snap.IsClaudeUsageSemantic {
			setPublicKey(other, "usage_semantic", "anthropic")
		}
	}

	adminInfo := make(map[string]any, 1)
	if rc.QuotaClamp != nil {
		// 钳制标记必须只出现在 admin_info：普通用户视图会整体剥离该分区
		// （billing-07 §7.2），这使「管理员可见、非管理员不可见」成为结构
		// 性质而不是读取侧的额外判断。
		adminInfo["quota_saturation"] = pricing.ClampAuditMap(rc.QuotaClamp)
	}
	if len(adminInfo) > 0 {
		other["admin_info"] = adminInfo
	}

	return other
}

// setPublicKey 写入 public 顶层键，拒绝保留键（billing-07 §7.1）。
func setPublicKey(dst map[string]any, key string, value any) {
	if _, reserved := usageLogReservedPublicKeys[key]; reserved {
		return
	}
	dst[key] = value
}

// writeUsageLog 在单个日志库事务内完成「加锁 → 查重 → 插入/收敛」。
//
// 为什么必须用事务级咨询锁：request_id 无唯一约束，两个执行者（Terminate
// 与 worker 退款）并发写时，单纯 SELECT-then-INSERT 在 READ COMMITTED 下
// 互相看不到对方未提交的行，会写出两行。锁按 request_id 的哈希串行化
// 同一请求的写者；不同请求之间只有极小概率的哈希碰撞（仅多一次串行）。
func (l *DBLogger) writeUsageLog(ctx context.Context, row usageLogRow) error {
	tx, err := l.logsDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage log tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, row.requestID); err != nil {
		return fmt.Errorf("lock usage log request id: %w", err)
	}

	existing, err := existingUsageLogStatuses(ctx, tx, row.requestID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		if row.status != UsageLogStatusRefunded {
			// 已有终态 → 幂等命中。尤其不得把 refunded 覆盖成 success
			// （R-25：worker 在 Saturate 与主库结算之间已判定悬挂并退款）。
			return tx.Commit()
		}
		// 单向收敛到 refunded：billing-06 §5.3 第 6 步要求「若存在日志行，
		// 置 status=refunded、cost=0」。refunded 金额取调用方新值与既有值
		// 的较大者，重复退款不会把已记录金额改小。
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_logs
			   SET status = $2,
			       cost = 0,
			       refunded = GREATEST(COALESCE(refunded, 0), $3)
			 WHERE request_id = $1::uuid`,
			row.requestID, row.status, row.refunded); err != nil {
			return fmt.Errorf("converge usage log to refunded: %w", err)
		}
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, usageLogInsertSQL,
		row.requestID, row.userID, row.tokenID, row.channelID, row.model,
		row.group, row.promptTokens, row.completionTokens, row.cacheReadTokens,
		row.cacheWriteTokens, row.preDeducted, row.cost, row.refunded,
		row.latencyMS, row.firstTokenMS, row.stream, row.status,
		row.errorClass, row.errorMessage, row.other); err != nil {
		return fmt.Errorf("insert usage log: %w", err)
	}
	return tx.Commit()
}

// usageLogInsertSQL 显式列出 platform-01 §12 的 25 列。
//
// id 由 IDENTITY 生成，created_at 由数据库 now() 生成；token_name/upstream/
// retry_chain 在 M3 无可靠来源，显式 NULL。列清单保持与迁移逐列同名，
// 便于后续补数据源时按列增补，而不是改成 map/反射写入。
const usageLogInsertSQL = `
INSERT INTO usage_logs (
    request_id, user_id, token_id, token_name, channel_id, upstream,
    model, "group", prompt_tokens, completion_tokens, cache_read_tokens,
    cache_write_tokens, pre_deducted, cost, refunded, latency_ms,
    first_token_ms, stream, status, error_class, error_message,
    retry_chain, other)
VALUES ($1::uuid, $2, $3, NULL, $4, NULL,
        $5, $6, $7, $8, $9,
        $10, $11, $12, $13, $14,
        $15, $16, $17, $18, $19,
        NULL, $20::jsonb)`

// existingUsageLogStatuses 读取该 request_id 已存在的日志状态。
//
// FOR UPDATE 锁住已有行，配合事务级咨询锁构成「单行状态机」的写侧屏障。
// 读取后立即关闭 rows，避免 pgx 连接在事务内被结果集占用。
func existingUsageLogStatuses(ctx context.Context, tx *sql.Tx, requestID string) ([]int16, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT status FROM usage_logs WHERE request_id = $1::uuid
		 ORDER BY created_at, id FOR UPDATE`, requestID)
	if err != nil {
		return nil, fmt.Errorf("query existing usage log: %w", err)
	}
	defer rows.Close()

	var statuses []int16
	for rows.Next() {
		var s int16
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan existing usage log status: %w", err)
		}
		statuses = append(statuses, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate existing usage log rows: %w", err)
	}
	return statuses, nil
}

// nullInt64 把 0 视为 NULL（users/tokens/channels 的 id 由 IDENTITY 从 1 起，
// 零值表示「未绑定」而不是一个合法主键）。
func nullInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

// nullString 把空串视为 NULL（可空文本列，空值不渲染）。
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// clampTokenCount 把上游 token 数钳到 PG int 域并拒绝负值。
//
// 这里不是配额换算（quota 换算只走 pricing），但 PG 的 int 是 32 位：
// 直接把 int64 转 int 在 32 位宿主上会回绕。token 数不可为负，负数按 0
// 处理（上游解析怪癖不污染统计）。
func clampTokenCount(v int64) int {
	if v <= 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(v)
}

// truncateLogText 清洗并截断日志文本列。
//
// 两件事：
//   - 去掉 NUL：PostgreSQL 的 text/varchar 不接受 \u0000，否则整条日志
//     插入失败并阻塞结算；
//   - 按 **rune** 截断：按字节截断会把多字节 UTF-8 切成非法序列，同样
//     导致被数据库拒绝。max<=0 表示不截断。
func truncateLogText(s string, max int) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "�")
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

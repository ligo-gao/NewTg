package store

// request_pricing 仓储：冻结「本次请求按哪个时间、哪个时段倍率计价」（spec §5.2）。
//
// 【为什么需要持久化】预扣在请求开始时、结算在响应结束后；跨整点边界会得到
// 两个不同的时段倍率。同步请求可以复用内存快照，但任务轮询、崩溃恢复、
// handoff 结算只拿得到 requestID——它们必须从这张表读回请求开始的时间与
// 倍率，而不是用 time.Now() 重新解析当前策略。
//
// 【保留期】行数 = 7 天内请求数，与预扣对账同窗口（request_pricing_created_idx
// 支持按 created_at 批量删除）。DeleteBefore 由 billing 的 preconsume_reconcile
// 周期调用。
//
// 【request_id 是 uuid】生产 RequestID 是 32 位十六进制（relay.newRequestID），
// PostgreSQL 的 uuid 类型直接接受；非 UUID 的内部测试 id 在写入前被拒绝，
// 由调用方决定降级（见 billing.Session.persistRequestPricing）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RequestPricingRetention 是冻结计价事实的保留期（spec §5.2 / plan Task 4）。
const RequestPricingRetention = 7 * 24 * time.Hour

// ErrInvalidRequestPricingID 表示 request_id 不是合法的 UUID，无法写入 uuid 列。
var ErrInvalidRequestPricingID = errors.New("store: request_pricing.request_id 不是合法 UUID")

// RequestPricing 是一条冻结的请求计价事实。
//
// DiscountRate / DiscountTarget 是预扣阶段解析一次并冻结的客户折扣
// （spec §5.2 / §5.4）：任务轮询、崩溃恢复、handoff 结算只拿得到
// requestID，读不回折扣就会按原价结算，把预扣时已让利的部分再追回来。
// 未命中规则的会话由 Upsert 归一为 Rate=1 + 空 Target，与迁移默认值及
// 存量行同一拼写；全免由 Rate=0 + 非空 Target 表达，与未命中可区分。
type RequestPricing struct {
	RequestID      string
	RequestTime    time.Time
	TimeMultiplier float64
	TimeWindow     string
	DiscountRate   float64
	DiscountTarget string
}

// RequestPricingRepo 是 request_pricing 的 PostgreSQL 实现。
type RequestPricingRepo struct {
	db *sql.DB
}

// NewRequestPricingRepo 构造仓储。db 必须非 nil：冻结事实没有内存兜底。
func NewRequestPricingRepo(db *sql.DB) *RequestPricingRepo {
	return &RequestPricingRepo{db: db}
}

// ValidRequestPricingID 判断请求 id 能否写进 uuid 主键。
//
// 生产 RequestID 恒为 32 位十六进制；内部测试/演示可能用 "req-xxx" 这类
// 非 UUID 字符串，调用方据此跳过持久化而不是让预扣失败。
func ValidRequestPricingID(requestID string) bool {
	_, err := ParseRequestPricingID(requestID)
	return err == nil
}

// ParseRequestPricingID 规范化 request_id（接受带/不带连字符的 32 位十六进制）。
func ParseRequestPricingID(requestID string) (string, error) {
	trimmed := strings.TrimSpace(requestID)
	if trimmed == "" {
		return "", ErrInvalidRequestPricingID
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidRequestPricingID, requestID)
	}
	return parsed.String(), nil
}

// Upsert 写入（或覆盖）一条冻结事实。
//
// 同一 requestID 的重复预扣（重试）写入相同值；用 DO UPDATE 而不是 DO NOTHING：
// 首个成功预扣之后若请求时间被修正（同一会话重试），以最后一次成功预扣为准，
// 与内存快照一致。
func (r *RequestPricingRepo) Upsert(ctx context.Context, rec RequestPricing) error {
	if r == nil || r.db == nil {
		return errors.New("store: request_pricing 仓储未绑定数据库")
	}
	id, err := ParseRequestPricingID(rec.RequestID)
	if err != nil {
		return err
	}
	if rec.RequestTime.IsZero() {
		return errors.New("store: request_pricing.request_time 不能为零值")
	}
	if math.IsNaN(rec.TimeMultiplier) || math.IsInf(rec.TimeMultiplier, 0) || rec.TimeMultiplier <= 0 {
		return fmt.Errorf("store: request_pricing.time_multiplier 非法: %v", rec.TimeMultiplier)
	}
	// 折扣率是防脏数据落库的护栏（结算侧 ApplyDiscount / discountFacts 已有
	// 两层 fail-closed）：NaN/Inf 与 [0,1] 之外一律按 1（原价）写入并告警，
	// 不返回错误——让预扣失败等于拒绝服务，代价远大于一条被归一的行。
	discountRate := rec.DiscountRate
	if math.IsNaN(discountRate) || math.IsInf(discountRate, 0) || discountRate < 0 || discountRate > 1 {
		slog.Warn("store: request_pricing.discount_rate 非法；按原价 1 写入",
			"request_id", id, "discount_rate", discountRate)
		discountRate = 1
	}
	// DiscountTarget 空串合法（未命中），形态校验属于 API 层。空 Target 时
	// 强制 Rate=1：空串是「无折扣」的唯一判别字段，若保留会话零值 Rate=0，
	// 同一状态在库里会有 0/"" 与 1/"" 两种拼写（迁移默认值与存量行都是 1）。
	// 归一放在仓储写入侧这一条路径上，未来任何调用方都自动获得同一口径。
	if rec.DiscountTarget == "" {
		discountRate = 1
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO request_pricing (request_id, request_time, time_multiplier, time_window, discount_rate, discount_target)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6)
		 ON CONFLICT (request_id) DO UPDATE
		 SET request_time = EXCLUDED.request_time,
		     time_multiplier = EXCLUDED.time_multiplier,
		     time_window = EXCLUDED.time_window,
		     discount_rate = EXCLUDED.discount_rate,
		     discount_target = EXCLUDED.discount_target`,
		id, rec.RequestTime, rec.TimeMultiplier, rec.TimeWindow, discountRate, rec.DiscountTarget)
	if err != nil {
		return fmt.Errorf("store: 写入 request_pricing(%s): %w", id, err)
	}
	return nil
}

// Get 读取一条冻结事实。第二个返回值为 false 表示没有该请求的记录
// （不是错误）—— 例如本次上线前的在飞请求，结算方按既有行为继续。
func (r *RequestPricingRepo) Get(ctx context.Context, requestID string) (RequestPricing, bool, error) {
	if r == nil || r.db == nil {
		return RequestPricing{}, false, errors.New("store: request_pricing 仓储未绑定数据库")
	}
	id, err := ParseRequestPricingID(requestID)
	if err != nil {
		return RequestPricing{}, false, err
	}
	var rec RequestPricing
	err = r.db.QueryRowContext(ctx,
		`SELECT request_id::text, request_time, time_multiplier, time_window, discount_rate, discount_target
		 FROM request_pricing WHERE request_id = $1::uuid`, id).
		Scan(&rec.RequestID, &rec.RequestTime, &rec.TimeMultiplier, &rec.TimeWindow,
			&rec.DiscountRate, &rec.DiscountTarget)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestPricing{}, false, nil
	}
	if err != nil {
		return RequestPricing{}, false, fmt.Errorf("store: 读取 request_pricing(%s): %w", id, err)
	}
	return rec, true, nil
}

// DeleteBefore 删除 created_at 早于 cutoff 的行，返回删除数量。
//
// 按 created_at 而不是 request_time 删除：保留语义是「预扣后 7 天」，与
// 迁移里的扫描索引一致；时钟回拨/手工改 request_time 不会留下永久的行。
func (r *RequestPricingRepo) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("store: request_pricing 仓储未绑定数据库")
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM request_pricing WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: 清理 request_pricing: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取 request_pricing 清理行数: %w", err)
	}
	return n, nil
}

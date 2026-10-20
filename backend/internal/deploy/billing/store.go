package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// store 是两张计费表的唯一 SQL 访问层。
//
// 为什么不用 GORM：平台迁移与受控 SQL 是 platform-04 的既定口径（应用不跑
// DDL，查询保持显式）；这里的 append-only 账本与 ON CONFLICT 幂等都需要
// 精确控制语句形状。
type store struct {
	db *sql.DB
}

// queryer 抽象 *sql.DB / *sql.Tx：预扣、结算、释放都必须在同一事务里
// 写段状态与事实行，避免「段已 settled 但 settlement 事实没落库」。
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const segmentColumns = `segment_id, deployment_id, segment_kind, idempotency_key,
	provider_request_id, name, hardware_id, location_ids::text, gpus_per_container,
	replica_count, planned_seconds, started_at, planned_end_at, quote_usd::text,
	unit_price_usd_per_gpu_second::text, price_source, preconsumed_credits,
	settled_credits, state, billing_heartbeat_at, last_provider_observed_at,
	created_at, updated_at, state_changed_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSegment(row rowScanner) (Segment, error) {
	var (
		seg               Segment
		locationsText     string
		quoteText         string
		unitPriceText     string
		plannedEnd        sql.NullTime
		startedAt         sql.NullTime
		providerRequestID sql.NullString
		settledCredits    sql.NullInt64
		providerObserved  sql.NullTime
	)
	if err := row.Scan(
		&seg.SegmentID, &seg.DeploymentID, &seg.SegmentKind, &seg.IdempotencyKey,
		&providerRequestID, &seg.Name, &seg.HardwareID, &locationsText,
		&seg.GPUsPerContainer, &seg.ReplicaCount, &seg.PlannedSeconds, &startedAt,
		&plannedEnd, &quoteText, &unitPriceText, &seg.PriceSource,
		&seg.PreconsumedCredits, &settledCredits, &seg.State, &seg.BillingHeartbeatAt,
		&providerObserved, &seg.CreatedAt, &seg.UpdatedAt, &seg.StateChangedAt,
	); err != nil {
		return Segment{}, err
	}
	locations, err := parseBigintArray(locationsText)
	if err != nil {
		return Segment{}, fmt.Errorf("deploy billing: parse location_ids: %w", err)
	}
	quote, err := decimal.NewFromString(quoteText)
	if err != nil {
		return Segment{}, fmt.Errorf("deploy billing: parse quote_usd: %w", err)
	}
	unitPrice, err := decimal.NewFromString(unitPriceText)
	if err != nil {
		return Segment{}, fmt.Errorf("deploy billing: parse unit price: %w", err)
	}
	seg.LocationIDs = locations
	seg.QuoteUSD = quote
	seg.UnitPriceUSDPerGPUSecond = unitPrice
	if startedAt.Valid {
		v := startedAt.Time
		seg.StartedAt = &v
	}
	if plannedEnd.Valid {
		v := plannedEnd.Time
		seg.PlannedEndAt = &v
	}
	if providerRequestID.Valid {
		v := providerRequestID.String
		seg.ProviderRequestID = &v
	}
	if settledCredits.Valid {
		v := settledCredits.Int64
		seg.SettledCredits = &v
	}
	if providerObserved.Valid {
		v := providerObserved.Time
		seg.LastProviderObservedAt = &v
	}
	return seg, nil
}

const factColumns = `fact_id, fact_key, segment_id, deployment_id, fact_type,
	period_start, period_end, gpus_per_container, replica_count, gpu_seconds,
	unit, usage_source, estimated_credits, confirmed_credits, state,
	provider_observed_at, audit_ref, created_at, provider_cumulative_seconds`

func scanFact(row rowScanner) (CostFact, error) {
	var (
		f                 CostFact
		periodStart       sql.NullTime
		periodEnd         sql.NullTime
		confirmed         sql.NullInt64
		providerObserved  sql.NullTime
		auditRef          sql.NullString
		cumulativeSeconds sql.NullInt64
	)
	if err := row.Scan(
		&f.FactID, &f.FactKey, &f.SegmentID, &f.DeploymentID, &f.FactType,
		&periodStart, &periodEnd, &f.GPUsPerContainer, &f.ReplicaCount,
		&f.GPUSeconds, &f.Unit, &f.UsageSource, &f.EstimatedCredits,
		&confirmed, &f.State, &providerObserved, &auditRef, &f.CreatedAt,
		&cumulativeSeconds,
	); err != nil {
		return CostFact{}, err
	}
	if periodStart.Valid {
		v := periodStart.Time
		f.PeriodStart = &v
	}
	if periodEnd.Valid {
		v := periodEnd.Time
		f.PeriodEnd = &v
	}
	if confirmed.Valid {
		v := confirmed.Int64
		f.ConfirmedCredits = &v
	}
	if providerObserved.Valid {
		v := providerObserved.Time
		f.ProviderObservedAt = &v
	}
	if auditRef.Valid {
		v := auditRef.String
		f.AuditRef = &v
	}
	if cumulativeSeconds.Valid {
		v := cumulativeSeconds.Int64
		f.ProviderCumulativeSeconds = &v
	}
	return f, nil
}

// insertSegment 以 idempotency_key 为幂等锚；冲突时返回既有段（created=false）。
func (s *store) insertSegment(ctx context.Context, q queryer, seg Segment) (Segment, bool, error) {
	row := q.QueryRowContext(ctx, `
		INSERT INTO deployment_billing_segments (
			segment_id, deployment_id, segment_kind, idempotency_key,
			provider_request_id, name, hardware_id, location_ids, gpus_per_container,
			replica_count, planned_seconds, started_at, planned_end_at, quote_usd,
			unit_price_usd_per_gpu_second, price_source, preconsumed_credits,
			settled_credits, state, billing_heartbeat_at, last_provider_observed_at,
			created_at, updated_at, state_changed_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8::bigint[], $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19, $20, $21, $22, $23, $24
		)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+segmentColumns,
		seg.SegmentID, seg.DeploymentID, seg.SegmentKind, seg.IdempotencyKey,
		seg.ProviderRequestID, seg.Name, seg.HardwareID, formatBigintArray(seg.LocationIDs),
		seg.GPUsPerContainer, seg.ReplicaCount, seg.PlannedSeconds, seg.StartedAt,
		seg.PlannedEndAt, seg.QuoteUSD.String(), seg.UnitPriceUSDPerGPUSecond.String(),
		seg.PriceSource, seg.PreconsumedCredits, seg.SettledCredits, seg.State,
		seg.BillingHeartbeatAt, seg.LastProviderObservedAt, seg.CreatedAt,
		seg.UpdatedAt, seg.StateChangedAt)
	inserted, err := scanSegment(row)
	if err == nil {
		return inserted, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Segment{}, false, err
	}
	existing, err := s.segmentByIdempotencyKey(ctx, q, seg.IdempotencyKey)
	if err != nil {
		return Segment{}, false, err
	}
	return existing, false, nil
}

func (s *store) segmentByIdempotencyKey(ctx context.Context, q queryer, key string) (Segment, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+segmentColumns+` FROM deployment_billing_segments WHERE idempotency_key = $1`, key)
	seg, err := scanSegment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Segment{}, ErrSegmentNotFound
	}
	return seg, err
}

func (s *store) segmentByID(ctx context.Context, id string) (Segment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+segmentColumns+` FROM deployment_billing_segments WHERE segment_id = $1`, id)
	seg, err := scanSegment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Segment{}, ErrSegmentNotFound
	}
	return seg, err
}

// activeSegmentByDeployment 返回该部署最近的未终态段（续期/对账都以最新段为准）。
func (s *store) activeSegmentByDeployment(ctx context.Context, deploymentID string) (Segment, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+segmentColumns+` FROM deployment_billing_segments
		WHERE deployment_id = $1 AND state NOT IN ('settled', 'abandoned', 'failed')
		ORDER BY created_at DESC LIMIT 1`, deploymentID)
	seg, err := scanSegment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Segment{}, ErrSegmentNotFound
	}
	return seg, err
}

// listRunningSegmentsAfter 是按 (started_at, segment_id) 的 keyset 分页。
//
// 为什么不用 OFFSET：Accrue 会持续写入事实并刷新心跳，单轮扫描可能跨越
// 数分钟；OFFSET 在并发写入下会跳段/重扫。keyset 的游标只前进，能保证
// 「所有运行段都被处理到」（M4 复核）。
func (s *store) listRunningSegmentsAfter(ctx context.Context, afterStartedAt time.Time, afterSegmentID string, limit int) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+segmentColumns+` FROM deployment_billing_segments
		WHERE state = 'running' AND started_at IS NOT NULL
		  AND (started_at > $1 OR (started_at = $1 AND segment_id > $2))
		ORDER BY started_at, segment_id
		LIMIT $3`, afterStartedAt, afterSegmentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSegments(rows)
}

// listStaleSegments 实现 N16 的扫描条件：非终态且心跳超过租约 TTL。
func (s *store) listStaleSegments(ctx context.Context, now time.Time, ttl time.Duration, limit int) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+segmentColumns+` FROM deployment_billing_segments
		WHERE state IN ('pending_confirm', 'running', 'deleting_unknown')
		  AND billing_heartbeat_at < $1
		ORDER BY billing_heartbeat_at
		LIMIT $2`, now.Add(-ttl), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSegments(rows)
}

func collectSegments(rows *sql.Rows) ([]Segment, error) {
	var out []Segment
	for rows.Next() {
		seg, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

func (s *store) heartbeat(ctx context.Context, q queryer, segmentID string, now time.Time, observedAt *time.Time) error {
	_, err := q.ExecContext(ctx, `
		UPDATE deployment_billing_segments
		SET billing_heartbeat_at = $2, updated_at = $2,
		    last_provider_observed_at = COALESCE($3, last_provider_observed_at)
		WHERE segment_id = $1 AND state NOT IN ('settled', 'abandoned', 'failed')`,
		segmentID, now, observedAt)
	return err
}

// markRunning 把 pending_confirm/deleting_unknown 段收敛为 running。
// state_changed_at 在这里重置：对账窗口只适用于「未知结果」状态，进入
// running 后窗口不再有意义（billing-03 §5.9.3）。
func (s *store) markRunning(ctx context.Context, q queryer, segmentID string, startedAt, plannedEndAt, now time.Time) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE deployment_billing_segments
		SET state = 'running', started_at = COALESCE(started_at, $2),
		    planned_end_at = COALESCE(planned_end_at, $3),
		    billing_heartbeat_at = $4, updated_at = $4, state_changed_at = $4
		WHERE segment_id = $1
		  AND state IN ('pending_confirm', 'deleting_unknown', 'running')`,
		segmentID, startedAt, plannedEndAt, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// settleSegment 条件更新为 settled；已终态返回 false（重复结算 no-op）。
func (s *store) settleSegment(ctx context.Context, q queryer, segmentID string, credits int64, now time.Time) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE deployment_billing_segments
		SET state = 'settled', settled_credits = $2, billing_heartbeat_at = $3,
		    updated_at = $3, state_changed_at = $3
		WHERE segment_id = $1
		  AND state IN ('pending_confirm', 'running', 'deleting_unknown')`,
		segmentID, credits, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// abandonSegment 条件更新为 abandoned；成本归零。
func (s *store) abandonSegment(ctx context.Context, q queryer, segmentID string, now time.Time) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE deployment_billing_segments
		SET state = 'abandoned', settled_credits = 0, billing_heartbeat_at = $2,
		    updated_at = $2, state_changed_at = $2
		WHERE segment_id = $1
		  AND state IN ('pending_confirm', 'running', 'deleting_unknown')`,
		segmentID, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// adjustSettledCredits 把结算确认成本替换为对账后的金额（事实本身只追加）。
func (s *store) adjustSettledCredits(ctx context.Context, q queryer, segmentID string, credits int64, now time.Time) error {
	_, err := q.ExecContext(ctx, `
		UPDATE deployment_billing_segments
		SET settled_credits = $2, updated_at = $3
		WHERE segment_id = $1 AND state = 'settled'`, segmentID, credits, now)
	return err
}

// insertFact 以 fact_key 唯一约束兜底幂等；冲突即忽略（billing-05 §4.4.6）。
func (s *store) insertFact(ctx context.Context, q queryer, f CostFact) (bool, error) {
	res, err := q.ExecContext(ctx, `
		INSERT INTO deployment_cost_facts (
			fact_key, segment_id, deployment_id, fact_type, period_start, period_end,
			gpus_per_container, replica_count, gpu_seconds, unit, usage_source,
			estimated_credits, confirmed_credits, state, provider_observed_at,
			audit_ref, created_at, provider_cumulative_seconds
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		          $15, $16, $17, $18)
		ON CONFLICT (fact_key) DO NOTHING`,
		f.FactKey, f.SegmentID, f.DeploymentID, f.FactType, f.PeriodStart, f.PeriodEnd,
		f.GPUsPerContainer, f.ReplicaCount, f.GPUSeconds, f.Unit, f.UsageSource,
		f.EstimatedCredits, f.ConfirmedCredits, f.State, f.ProviderObservedAt,
		f.AuditRef, f.CreatedAt, f.ProviderCumulativeSeconds)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *store) listFacts(ctx context.Context, segmentID string) ([]CostFact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+factColumns+` FROM deployment_cost_facts WHERE segment_id = $1 ORDER BY fact_id`, segmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostFact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// priorProviderCumulativeSeconds 取同一部署**更早段**的最大 provider 累计值。
//
// provider 返回的是部署级累计；若直接当段内值，续期后的新段会把上一段
// 重复计入（billing-05 §4.4.2）。首段没有更早段时返回 0。
func (s *store) priorProviderCumulativeSeconds(ctx context.Context, seg Segment) (int64, error) {
	var value sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(f.provider_cumulative_seconds)
		FROM deployment_cost_facts f
		JOIN deployment_billing_segments s ON s.segment_id = f.segment_id
		WHERE f.deployment_id = $1
		  AND f.provider_cumulative_seconds IS NOT NULL
		  AND (s.created_at < $2 OR (s.created_at = $2 AND s.segment_id <> $3))`,
		seg.DeploymentID, seg.CreatedAt, seg.SegmentID).Scan(&value)
	if err != nil {
		return 0, err
	}
	if !value.Valid {
		return 0, nil
	}
	return value.Int64, nil
}

// CostTotals 是成本汇总（按 fact_type 分桶）。
//
// 口径：
//   - preconsume/accrual 是过程量，取 estimated_credits；
//   - released 取 confirmed_credits（=0，用于把预扣归零）；
//   - ConfirmedCredits 是**每段最终确认成本**之和：有 reconciliation_adjustment
//     时取最后一条调整（绝对金额），否则取 settlement。把 settlement 与
//     adjustment 直接相加会把同一成本统计两次。
type CostTotals struct {
	PreconsumeCredits int64
	AccrualCredits    int64
	SettlementCredits int64
	ReleasedCredits   int64
	AdjustmentCredits int64
	ConfirmedCredits  int64
}

// ConfirmedCost 是站点成本账的确认金额（每段最终值之和）。
func (t CostTotals) ConfirmedCost() int64 { return t.ConfirmedCredits }

// costTotals 按部署（deploymentID 为空时全站）汇总事实账本。
func (s *store) costTotals(ctx context.Context, deploymentID string) (CostTotals, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT fact_type,
		       COALESCE(SUM(CASE WHEN fact_type IN ('preconsume', 'accrual')
		                         THEN estimated_credits ELSE confirmed_credits END), 0)
		FROM deployment_cost_facts
		WHERE ($1 = '' OR deployment_id = $1)
		GROUP BY fact_type`, deploymentID)
	if err != nil {
		return CostTotals{}, err
	}
	var out CostTotals
	for rows.Next() {
		var kind string
		var credits int64
		if err := rows.Scan(&kind, &credits); err != nil {
			rows.Close()
			return CostTotals{}, err
		}
		switch kind {
		case FactPreconsume:
			out.PreconsumeCredits = credits
		case FactAccrual:
			out.AccrualCredits = credits
		case FactSettlement:
			out.SettlementCredits = credits
		case FactReleased:
			out.ReleasedCredits = credits
		case FactReconciliationAdjustment:
			out.AdjustmentCredits = credits
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CostTotals{}, err
	}
	rows.Close()

	// 每段最终确认成本：
	//   - 有调整事实 → 外部账单是最终真源，按账单周期取每组最后一条
	//     （fact_id 升序覆盖）后求和；多个账单周期相加，不能互相覆盖；
	//   - 没有调整事实 → 取 settlement。
	settlements := make(map[string]int64)
	adjustments := make(map[string]map[string]int64)
	finalRows, err := s.db.QueryContext(ctx, `
		SELECT segment_id, fact_type, period_start, COALESCE(confirmed_credits, 0)
		FROM deployment_cost_facts
		WHERE fact_type IN ('settlement', 'reconciliation_adjustment')
		  AND ($1 = '' OR deployment_id = $1)
		ORDER BY fact_id`, deploymentID)
	if err != nil {
		return CostTotals{}, err
	}
	defer finalRows.Close()
	for finalRows.Next() {
		var segmentID, factType string
		var periodStart sql.NullTime
		var credits int64
		if err := finalRows.Scan(&segmentID, &factType, &periodStart, &credits); err != nil {
			return CostTotals{}, err
		}
		if factType == FactSettlement {
			settlements[segmentID] = credits
			continue
		}
		group := adjustments[segmentID]
		if group == nil {
			group = make(map[string]int64)
			adjustments[segmentID] = group
		}
		group[periodKey(periodStart)] = credits
	}
	if err := finalRows.Err(); err != nil {
		return CostTotals{}, err
	}
	for segmentID, settlement := range settlements {
		group, ok := adjustments[segmentID]
		if !ok {
			out.ConfirmedCredits += settlement
			continue
		}
		for _, credits := range group {
			out.ConfirmedCredits += credits
		}
	}
	// 只有调整、没有 settlement 的段（理论上不应出现）也要计入。
	for segmentID, group := range adjustments {
		if _, ok := settlements[segmentID]; ok {
			continue
		}
		for _, credits := range group {
			out.ConfirmedCredits += credits
		}
	}
	return out, nil
}

// lockSegment 在事务内对该段行加锁，串行化同段的发票调整。
//
// 为什么需要：两个不同账单周期的调整并发提交时，各自按本事务可见的
// 事实重算 settled_credits，可能用「缺了对方调整」的值覆盖；加行锁后
// 第二个事务能看到第一个已提交的调整，最终金额正确。
func (s *store) lockSegment(ctx context.Context, q queryer, segmentID string) error {
	var one int
	if err := q.QueryRowContext(ctx,
		`SELECT 1 FROM deployment_billing_segments WHERE segment_id = $1 FOR UPDATE`,
		segmentID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSegmentNotFound
		}
		return err
	}
	return nil
}

// adjustmentExists 判断「同一账单引用 + 同一金额」的调整是否已落账。
//
// 发票重读必须幂等：同一张发票重复对账不能每轮追加一条调整。账单引用
// 为空时无法判断（不用金额猜测），交给 keyset 序号与金额相等短路处理。
func (s *store) adjustmentExists(ctx context.Context, q queryer, segmentID, billReference string, credits int64) (bool, error) {
	if billReference == "" {
		return false, nil
	}
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM deployment_cost_facts
		WHERE segment_id = $1 AND fact_type = 'reconciliation_adjustment'
		  AND audit_ref = $2 AND confirmed_credits = $3
		LIMIT 1`, segmentID, billReference, credits).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// nextAdjustmentSequence 返回该 (段, 周期, 账单引用) 基键下的下一个序号。
//
// fact_key 里嵌入序号，保证：
//   - PeriodStart 为空时不会与其它调整共用 "-" 键；
//   - 同一周期收到更正发票（金额变化）时写新事实，而不是被唯一约束静默丢弃。
func (s *store) nextAdjustmentSequence(ctx context.Context, q queryer, segmentID, base string) (int, error) {
	prefix := base + ":"
	var count int
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM deployment_cost_facts
		WHERE segment_id = $1 AND fact_type = 'reconciliation_adjustment'
		  AND left(fact_key, length($2)) = $2`, segmentID, prefix).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count + 1, nil
}

// segmentConfirmedCredits 计算段的最终确认成本。
//
// 口径（与 CostTotals 一致）：
//   - 没有调整事实 → 取 settlement 的确认金额；
//   - 有调整事实 → 外部账单是最终真源：按账单周期分组，取每组最后一条
//     调整（fact_id 升序），再求和。多个账单周期因此是相加而不是互相覆盖。
func (s *store) segmentConfirmedCredits(ctx context.Context, q queryer, segmentID string) (int64, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT fact_type, period_start, COALESCE(confirmed_credits, 0)
		FROM deployment_cost_facts
		WHERE segment_id = $1 AND fact_type IN ('settlement', 'reconciliation_adjustment')
		ORDER BY fact_id`, segmentID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var settlement int64
	adjustments := make(map[string]int64)
	for rows.Next() {
		var factType string
		var periodStart sql.NullTime
		var credits int64
		if err := rows.Scan(&factType, &periodStart, &credits); err != nil {
			return 0, err
		}
		if factType == FactSettlement {
			settlement = credits
			continue
		}
		adjustments[periodKey(periodStart)] = credits
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(adjustments) > 0 {
		var total int64
		for _, credits := range adjustments {
			total += credits
		}
		return total, nil
	}
	return settlement, nil
}

// periodKey 把可空周期起点转成稳定的分组键。
func periodKey(periodStart sql.NullTime) string {
	if !periodStart.Valid {
		return "-"
	}
	return periodStart.Time.UTC().Format(time.RFC3339)
}

// newSegmentID 用 UUID 文本作为本地段 ID（schema 为 text，不依赖 PG 扩展）。
func newSegmentID() string { return uuid.NewString() }

// formatBigintArray 把 []int64 写成 PG 数组字面量；空切片用 '{}'。
func formatBigintArray(values []int64) string {
	if len(values) == 0 {
		return "{}"
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.FormatInt(v, 10)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// parseBigintArray 解析 PG 的 `{1,2}` 数组文本（location_ids 是 bigint[]，
// 用 ::text 读出以避免额外引入 pgtype 依赖）。
func parseBigintArray(text string) ([]int64, error) {
	text = strings.TrimSpace(text)
	if len(text) < 2 || text[0] != '{' || text[len(text)-1] != '}' {
		return nil, fmt.Errorf("unexpected array text %q", text)
	}
	body := strings.TrimSpace(text[1 : len(text)-1])
	if body == "" {
		return []int64{}, nil
	}
	parts := strings.Split(body, ",")
	out := make([]int64, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "NULL" {
			continue
		}
		v, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

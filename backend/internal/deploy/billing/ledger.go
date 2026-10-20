package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/deploy/ionet"
)

// 审计动作（billing-03 §5.9.4 冻结名）。
const (
	AuditActionSettle    = "deploy.billing_settle"
	AuditActionReconcile = "deploy.billing_reconcile"
)

// AuditFunc 写审计轨迹。detail 不得含 provider 密钥/仓库密钥/秘密环境变量。
type AuditFunc func(ctx context.Context, action string, detail map[string]any) error

// LedgerConfig 是站点成本账的依赖。
type LedgerConfig struct {
	// DB 必填：主库连接池。
	DB *sql.DB
	// Now 为空时使用 time.Now（测试注入固定时刻）。
	Now func() time.Time
	// QuotaPerUnit 为空时读取进程级 billing.quota_per_unit。
	QuotaPerUnit func() int64
	// Provider 是 provider 客户端接缝；nil 表示未配置（只做内部记账）。
	Provider ProviderFunc
	// Invoice 是 provider 账单接缝；nil 时 ReconcileInvoice 返回 ErrInvoiceUnavailable。
	Invoice InvoiceReader
	// Logger 为空时使用 slog.Default。
	Logger *slog.Logger
	// Audit 可为 nil（例如未接线审计时的单测）。
	Audit AuditFunc
}

// Ledger 实现 billing-00 §6.6 的 DeploymentBillingLedger：
// Hold / Settle / Release / Reconcile，外加本批的周期累计 Accrue。
//
// 所有金额换算都走 pricing 的 decimal 族；本类型不触碰任何用户资金。
type Ledger struct {
	store        *store
	now          func() time.Time
	quotaPerUnit func() int64
	provider     ProviderFunc
	invoice      InvoiceReader
	log          *slog.Logger
	audit        AuditFunc
}

// NewLedger 构造账本。DB 为 nil 时报错，避免把「未装配」伪装成 no-op。
func NewLedger(cfg LedgerConfig) (*Ledger, error) {
	if cfg.DB == nil {
		return nil, errors.New("deploy billing: DB is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	qpu := cfg.QuotaPerUnit
	if qpu == nil {
		qpu = pricing.CurrentQuotaPerUnit
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Ledger{
		store:        &store{db: cfg.DB},
		now:          now,
		quotaPerUnit: qpu,
		provider:     cfg.Provider,
		invoice:      cfg.Invoice,
		log:          log,
		audit:        cfg.Audit,
	}, nil
}

// SetProvider 在运行期替换 provider 工厂（密钥轮换/配置变化由 worker 每轮
// 重新构造；保留 setter 便于测试先建账本再注入 fake）。
func (l *Ledger) SetProvider(provider ProviderFunc) { l.provider = provider }

// Hold 按冻结报价预扣并返回段。同一 Idempotency-Key 重入返回既有段，
// 不重复预扣（billing-00 §6.6 冻结约束 1）。
func (l *Ledger) Hold(ctx context.Context, est SegmentEstimate) (Segment, bool, error) {
	est = normalizeEstimate(est)
	if err := ValidateEstimate(est); err != nil {
		return Segment{}, false, err
	}
	plannedGPU := PlannedGPUSeconds(est)
	preconsumed, err := pricing.WalletQuotaFromDecimalStrict(
		decimal.NewFromInt(plannedGPU).
			Mul(est.UnitPriceUSDPerGPUSecond).
			Mul(decimal.NewFromInt(l.quotaPerUnit())))
	if err != nil {
		return Segment{}, false, fmt.Errorf("deploy billing: preconsume credits: %w", err)
	}
	now := l.now().UTC()
	seg := Segment{
		SegmentID:                newSegmentID(),
		DeploymentID:             est.DeploymentID,
		SegmentKind:              est.SegmentKind,
		IdempotencyKey:           est.IdempotencyKey,
		ProviderRequestID:        stringPtr(est.ProviderRequestID),
		Name:                     est.Name,
		HardwareID:               est.HardwareID,
		LocationIDs:              est.LocationIDs,
		GPUsPerContainer:         est.GPUsPerContainer,
		ReplicaCount:             est.ReplicaCount,
		PlannedSeconds:           est.PlannedSeconds,
		StartedAt:                est.StartedAt,
		QuoteUSD:                 est.QuoteUSD,
		UnitPriceUSDPerGPUSecond: est.UnitPriceUSDPerGPUSecond,
		PriceSource:              est.PriceSource,
		PreconsumedCredits:       preconsumed,
		State:                    est.State,
		BillingHeartbeatAt:       now,
		CreatedAt:                now,
		UpdatedAt:                now,
		StateChangedAt:           now,
	}
	if seg.StartedAt != nil {
		end := seg.StartedAt.Add(time.Duration(seg.PlannedSeconds) * time.Second)
		seg.PlannedEndAt = &end
	}

	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Segment{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	inserted, created, err := l.store.insertSegment(ctx, tx, seg)
	if err != nil {
		return Segment{}, false, err
	}
	if !created {
		if err := tx.Commit(); err != nil {
			return Segment{}, false, err
		}
		return inserted, false, nil
	}
	preFact := CostFact{
		FactKey:          FactKey(FactPreconsume, inserted.SegmentID, nil),
		SegmentID:        inserted.SegmentID,
		DeploymentID:     inserted.DeploymentID,
		FactType:         FactPreconsume,
		GPUsPerContainer: inserted.GPUsPerContainer,
		ReplicaCount:     inserted.ReplicaCount,
		GPUSeconds:       plannedGPU,
		Unit:             UnitGPUSecond,
		UsageSource:      UsageEstimated,
		EstimatedCredits: inserted.PreconsumedCredits,
		State:            FactHeld,
		CreatedAt:        now,
	}
	if _, err := l.store.insertFact(ctx, tx, preFact); err != nil {
		return Segment{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Segment{}, false, err
	}
	return inserted, true, nil
}

// Accrue 把一个 running 段在已闭合小时内的用量写成 accrual 事实。
//
// 幂等：fact_key 由整点边界 + segment_id 固定；重复运行/重启补算只会在
// ON CONFLICT 上 no-op。当前未闭合的小时不在中途写入（否则这个小时的
// 事实会永远停在首次运行时的短时长），由结算时的终态累计补齐。
func (l *Ledger) Accrue(ctx context.Context, segmentID string) (int, error) {
	seg, err := l.store.segmentByID(ctx, segmentID)
	if err != nil {
		return 0, err
	}
	if seg.State != SegmentRunning || seg.StartedAt == nil {
		return 0, nil
	}
	now := l.now().UTC()
	end := l.billableEnd(seg, now)
	if !end.After(*seg.StartedAt) {
		return 0, nil
	}

	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	n, err := l.writeAccrualsTx(ctx, tx, seg, end, false, UsageInternalAccounting, nil)
	if err != nil {
		return 0, err
	}
	if err := l.store.heartbeat(ctx, tx, seg.SegmentID, now, nil); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// Settle 在停止/删除/续期边界按确认用量结算差额。
//
// 重复调用：段已终态时直接 no-op；并发调用由条件 UPDATE + fact_key 唯一
// 双重兜底（billing-03 §5.9.2）。
func (l *Ledger) Settle(ctx context.Context, segmentID string, fact GPUUsageFact) (int64, error) {
	seg, err := l.store.segmentByID(ctx, segmentID)
	if err != nil {
		return 0, err
	}
	if SegmentTerminal(seg.State) {
		return 0, nil
	}
	usage, err := l.normalizeUsage(seg, fact)
	if err != nil {
		return 0, err
	}
	confirmed, clamp := pricing.WalletQuotaFromDecimalChecked(
		decimal.NewFromInt(usage.GPUSeconds).
			Mul(seg.UnitPriceUSDPerGPUSecond).
			Mul(decimal.NewFromInt(l.quotaPerUnit())))
	if clamp != nil {
		l.log.Warn("deploy billing: quota saturated at settlement",
			"segment_id", seg.SegmentID, "kind", clamp.Kind, "op", clamp.Op)
	}
	delta := confirmed - seg.PreconsumedCredits
	now := l.now().UTC()
	if usage.ProviderObservedAt == nil {
		observed := now
		usage.ProviderObservedAt = &observed
	}

	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 先把未闭合小时的累计补齐（终态才允许写当前小时）；上界含 300s
	// 停止宽限，与 billing-05 §4.4.4 的段上界算式一致。
	if seg.StartedAt != nil {
		end := now
		deadline := seg.StartedAt.Add(time.Duration(seg.PlannedSeconds+DeploymentStopGraceSeconds) * time.Second)
		if deadline.Before(end) {
			end = deadline
		}
		if usage.PeriodEnd != nil && usage.PeriodEnd.Before(end) {
			end = *usage.PeriodEnd
		}
		if end.After(*seg.StartedAt) {
			if _, err := l.writeAccrualsTx(ctx, tx, seg, end, true, usage.Source, usage.ProviderCumulativeSeconds); err != nil {
				return 0, err
			}
		}
	}

	settled, err := l.store.settleSegment(ctx, tx, seg.SegmentID, confirmed, now)
	if err != nil {
		return 0, err
	}
	if !settled {
		return 0, nil
	}
	settlement := CostFact{
		FactKey:                   FactKey(FactSettlement, seg.SegmentID, nil),
		SegmentID:                 seg.SegmentID,
		DeploymentID:              seg.DeploymentID,
		FactType:                  FactSettlement,
		PeriodStart:               usage.PeriodStart,
		PeriodEnd:                 usage.PeriodEnd,
		GPUsPerContainer:          usage.GPUsPerContainer,
		ReplicaCount:              usage.ReplicaCount,
		GPUSeconds:                usage.GPUSeconds,
		Unit:                      UnitGPUSecond,
		UsageSource:               usage.Source,
		EstimatedCredits:          seg.PreconsumedCredits,
		ConfirmedCredits:          &confirmed,
		State:                     FactConfirmed,
		ProviderObservedAt:        usage.ProviderObservedAt,
		ProviderCumulativeSeconds: usage.ProviderCumulativeSeconds,
		AuditRef:                  nullableString(usage.AuditRef),
		CreatedAt:                 now,
	}
	if _, err := l.store.insertFact(ctx, tx, settlement); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	l.recordAudit(ctx, AuditActionSettle, map[string]any{
		"deployment_id":     seg.DeploymentID,
		"segment_id":        seg.SegmentID,
		"estimated_credits": seg.PreconsumedCredits,
		"confirmed_credits": confirmed,
		"delta_credits":     delta,
		"usage_source":      usage.Source,
		"result":            "settled",
	})
	return delta, nil
}

// Release 释放从未运行/创建失败的段的全部预扣（写 released 事实，成本 0）。
func (l *Ledger) Release(ctx context.Context, segmentID, reason string) error {
	seg, err := l.store.segmentByID(ctx, segmentID)
	if err != nil {
		return err
	}
	if SegmentTerminal(seg.State) {
		return nil
	}
	now := l.now().UTC()
	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	abandoned, err := l.store.abandonSegment(ctx, tx, seg.SegmentID, now)
	if err != nil {
		return err
	}
	if !abandoned {
		return nil
	}
	zero := int64(0)
	released := CostFact{
		FactKey:          FactKey(FactReleased, seg.SegmentID, nil),
		SegmentID:        seg.SegmentID,
		DeploymentID:     seg.DeploymentID,
		FactType:         FactReleased,
		GPUsPerContainer: seg.GPUsPerContainer,
		ReplicaCount:     seg.ReplicaCount,
		GPUSeconds:       0,
		Unit:             UnitGPUSecond,
		UsageSource:      UsageEstimated,
		EstimatedCredits: seg.PreconsumedCredits,
		ConfirmedCredits: &zero,
		State:            FactReleasedState,
		AuditRef:         nullableString(reason),
		CreatedAt:        now,
	}
	if _, err := l.store.insertFact(ctx, tx, released); err != nil {
		return err
	}
	return tx.Commit()
}

// Reconcile 按 provider 实际状态收敛未知结果（billing-03 §5.9.3）。
//
// 不静默丢单：provider 不可达时保持当前状态、写回观测心跳后返回错误/当前
// 状态，下一轮继续；只有 provider 可达且明确 404 才按 900s 窗口放弃。
func (l *Ledger) Reconcile(ctx context.Context, segmentID string) (string, error) {
	seg, err := l.store.segmentByID(ctx, segmentID)
	if err != nil {
		return "", err
	}
	if SegmentTerminal(seg.State) {
		return seg.State, nil
	}
	if l.provider == nil {
		return seg.State, nil
	}
	provider, ok, err := l.provider(ctx)
	if err != nil {
		return seg.State, err
	}
	if !ok {
		return seg.State, nil
	}
	detail, err := provider.GetDeployment(ctx, seg.DeploymentID)
	if err != nil {
		var apiErr *ionet.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return l.handleProviderMissing(ctx, seg)
		}
		// 审计带上 provider 的重试提示与业务码，便于运维区分「限流」与
		// 「不可达」；detail 不含密钥（APIError 已脱敏）。
		auditDetail := map[string]any{
			"deployment_id": seg.DeploymentID,
			"segment_id":    seg.SegmentID,
			"result":        "provider_error",
			"error_code":    "deploy_provider_error",
		}
		if errors.As(err, &apiErr) {
			if apiErr.StatusCode == 429 {
				auditDetail["error_code"] = "deploy_rate_limited"
			}
			if apiErr.RetryAfter > 0 {
				auditDetail["retry_after_seconds"] = int64(apiErr.RetryAfter / time.Second)
			}
		}
		l.recordAudit(ctx, AuditActionReconcile, auditDetail)
		return seg.State, err
	}
	snapshot, err := snapshotFromDetail(ctx, provider, detail)
	if err != nil {
		return seg.State, err
	}
	now := l.now().UTC()
	switch snapshot.Class {
	case StatusRunning:
		started := now
		if snapshot.StartedAt != nil {
			started = snapshot.StartedAt.UTC()
		}
		plannedEnd := started.Add(time.Duration(snapshot.PlannedSeconds) * time.Second)
		if _, err := l.store.markRunning(ctx, l.store.db, seg.SegmentID, started, plannedEnd, now); err != nil {
			return seg.State, err
		}
		observed := now
		if err := l.store.heartbeat(ctx, l.store.db, seg.SegmentID, now, &observed); err != nil {
			return seg.State, err
		}
		return SegmentRunning, nil
	case StatusTerminal:
		state, err := l.settleTerminal(ctx, seg, snapshot, now)
		l.recordAudit(ctx, AuditActionReconcile, map[string]any{
			"deployment_id":  seg.DeploymentID,
			"segment_id":     seg.SegmentID,
			"provider_state": snapshot.Status,
			"result":         state,
		})
		return state, err
	default:
		observed := now
		if err := l.store.heartbeat(ctx, l.store.db, seg.SegmentID, now, &observed); err != nil {
			return seg.State, err
		}
		return seg.State, nil
	}
}

func (l *Ledger) handleProviderMissing(ctx context.Context, seg Segment) (string, error) {
	now := l.now().UTC()
	if now.Sub(seg.StateChangedAt) < DeploymentReconcileWindowSeconds {
		observed := now
		if err := l.store.heartbeat(ctx, l.store.db, seg.SegmentID, now, &observed); err != nil {
			return seg.State, err
		}
		return seg.State, nil
	}
	if err := l.Release(ctx, seg.SegmentID, "provider_not_found"); err != nil {
		return seg.State, err
	}
	l.recordAudit(ctx, AuditActionReconcile, map[string]any{
		"deployment_id":  seg.DeploymentID,
		"segment_id":     seg.SegmentID,
		"provider_state": "not_found",
		"result":         SegmentAbandoned,
	})
	return SegmentAbandoned, nil
}

// settleTerminal 用 provider 终值（或内部记账 + 宽限）结算终态段。
//
// provider 累计是部署级值：段内值 = 累计 − 上一段基线（billing-05 §4.4.2）；
// provider 给出 0 但有起点时回退内部记账，避免把「未知」当「免费」。
func (l *Ledger) settleTerminal(ctx context.Context, seg Segment, snapshot DeploymentSnapshot, now time.Time) (string, error) {
	if snapshot.StartedAt == nil && snapshot.ComputeSeconds == 0 {
		if err := l.Release(ctx, seg.SegmentID, "never_started"); err != nil {
			return seg.State, err
		}
		return SegmentAbandoned, nil
	}
	started := seg.StartedAt
	if started == nil {
		started = snapshot.StartedAt
	}
	if started == nil {
		v := now.UTC()
		started = &v
	}
	replicas := seg.ReplicaCount
	gpus := seg.GPUsPerContainer
	source := UsageProviderExact
	var cumulative *int64
	var billableSeconds int64
	useProvider := snapshot.ComputeSeconds > 0
	if useProvider {
		prior, err := l.store.priorProviderCumulativeSeconds(ctx, seg)
		if err != nil {
			return seg.State, err
		}
		if snapshot.ComputeSeconds >= prior {
			billableSeconds = snapshot.ComputeSeconds - prior
			v := snapshot.ComputeSeconds
			cumulative = &v
		} else {
			// provider 累计回退（ComputeSeconds < 上一段基线）或本轮读数不可
			// 比：绝不能把差值夹成 0 还标 provider_exact——那会把「数据异常」
			// 记成「本段免费」。改走内部记账（M1 复核）。
			useProvider = false
			l.log.Warn("deploy billing: provider cumulative regressed, using internal accounting",
				"segment_id", seg.SegmentID,
				"compute_seconds", snapshot.ComputeSeconds,
				"prior_seconds", prior,
			)
		}
	}
	if !useProvider {
		source = UsageInternalAccounting
		end := now
		if snapshot.FinishedAt != nil {
			end = *snapshot.FinishedAt
		}
		if end.After(*started) {
			billableSeconds = int64(end.Sub(*started) / time.Second)
		}
	}
	// 段上界含 300s 停止宽限（billing-05 §4.4.4）：内部记账同样受此约束，
	// 防止 provider 终点异常时把整段墙钟时长记进账。
	maxBillable := seg.PlannedSeconds + DeploymentStopGraceSeconds
	if billableSeconds > maxBillable {
		billableSeconds = maxBillable
	}
	gpuSeconds := int64(gpus) * int64(replicas) * billableSeconds
	if gpuSeconds > SegmentMaxGPUSeconds(seg) {
		gpuSeconds = SegmentMaxGPUSeconds(seg)
	}
	observed := now
	if snapshot.FinishedAt != nil {
		observed = *snapshot.FinishedAt
	}
	_, err := l.Settle(ctx, seg.SegmentID, GPUUsageFact{
		SegmentID:                 seg.SegmentID,
		DeploymentID:              seg.DeploymentID,
		GPUSeconds:                gpuSeconds,
		GPUsPerContainer:          gpus,
		ReplicaCount:              replicas,
		Source:                    source,
		PeriodStart:               started,
		PeriodEnd:                 snapshot.FinishedAt,
		ProviderCumulativeSeconds: cumulative,
		ProviderObservedAt:        &observed,
	})
	if err != nil {
		return seg.State, err
	}
	return SegmentSettled, nil
}

// ReconcileInvoice 把 provider 账单与已确认成本的差异写成
// reconciliation_adjustment（billing-03 §5.9.3 第 6 条）。
//
// 幂等与更正：
//   - 同一账单引用、同一金额重复对账 → 0 差额，不追加事实；
//   - 同一周期收到更正发票（金额/引用变化）→ 写**新**调整事实（键含账单
//     引用 + 序号），不得被唯一约束静默丢弃；
//   - PeriodStart 为空 → 键里用 '-'，但序号保证逐次调整不共用键；
//   - 多个账单周期 → 各周期取最后一条调整后求和（见 segmentConfirmedCredits）。
func (l *Ledger) ReconcileInvoice(ctx context.Context, segmentID string) (int64, error) {
	if l.invoice == nil {
		return 0, ErrInvoiceUnavailable
	}
	seg, err := l.store.segmentByID(ctx, segmentID)
	if err != nil {
		return 0, err
	}
	if seg.State != SegmentSettled {
		return 0, fmt.Errorf("%w: invoice reconcile requires a settled segment", ErrInvalidSegment)
	}
	invoice, err := l.invoice.Invoice(ctx, seg.DeploymentID)
	if err != nil {
		return 0, err
	}
	invoiced, clamp := pricing.WalletQuotaFromDecimalChecked(
		invoice.AmountUSD.Mul(decimal.NewFromInt(l.quotaPerUnit())))
	if clamp != nil {
		l.log.Warn("deploy billing: quota saturated at invoice reconcile",
			"segment_id", seg.SegmentID, "kind", clamp.Kind, "op", clamp.Op)
	}
	current := int64(0)
	if seg.SettledCredits != nil {
		current = *seg.SettledCredits
	}
	if invoiced == current {
		l.recordAudit(ctx, AuditActionReconcile, map[string]any{
			"deployment_id":    seg.DeploymentID,
			"segment_id":       seg.SegmentID,
			"bill_reference":   invoice.BillReference,
			"invoiced_credits": invoiced,
			"delta_credits":    0,
			"result":           "invoice_unchanged",
		})
		return 0, nil
	}
	now := l.now().UTC()
	base := ReconciliationFactKeyBase(seg.SegmentID, invoice.PeriodStart, invoice.BillReference)
	// 序号在事务内按现有前缀计数；并发同键由 ON CONFLICT 兜底，冲突时重算。
	for attempt := 0; attempt < 5; attempt++ {
		tx, err := l.store.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		if err := l.store.lockSegment(ctx, tx, seg.SegmentID); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		exists, err := l.store.adjustmentExists(ctx, tx, seg.SegmentID, invoice.BillReference, invoiced)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if exists {
			// 并发对账已经写入同一张发票。
			_ = tx.Rollback()
			return 0, nil
		}
		sequence, err := l.store.nextAdjustmentSequence(ctx, tx, seg.SegmentID, base)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		fact := CostFact{
			FactKey:          ReconciliationFactKey(seg.SegmentID, invoice.PeriodStart, invoice.BillReference, sequence),
			SegmentID:        seg.SegmentID,
			DeploymentID:     seg.DeploymentID,
			FactType:         FactReconciliationAdjustment,
			PeriodStart:      invoice.PeriodStart,
			PeriodEnd:        invoice.PeriodEnd,
			GPUsPerContainer: seg.GPUsPerContainer,
			ReplicaCount:     seg.ReplicaCount,
			GPUSeconds:       0,
			Unit:             UnitGPUSecond,
			UsageSource:      UsageProviderExact,
			EstimatedCredits: current,
			ConfirmedCredits: &invoiced,
			State:            FactAdjusted,
			AuditRef:         nullableString(invoice.BillReference),
			CreatedAt:        now,
		}
		inserted, err := l.store.insertFact(ctx, tx, fact)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if !inserted {
			// 另一事务抢先写了同一序号：重算序号后重试。
			_ = tx.Rollback()
			continue
		}
		total, err := l.store.segmentConfirmedCredits(ctx, tx, seg.SegmentID)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if err := l.store.adjustSettledCredits(ctx, tx, seg.SegmentID, total, now); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		delta := total - current
		l.recordAudit(ctx, AuditActionReconcile, map[string]any{
			"deployment_id":       seg.DeploymentID,
			"segment_id":          seg.SegmentID,
			"bill_reference":      invoice.BillReference,
			"invoiced_credits":    invoiced,
			"confirmed_credits":   total,
			"delta_credits":       delta,
			"adjustment_sequence": sequence,
			"result":              "invoice_adjusted",
		})
		return delta, nil
	}
	return 0, fmt.Errorf("deploy billing: invoice reconciliation conflicted repeatedly for segment %s", seg.SegmentID)
}

// CostTotals 返回按部署（空串 = 全站）的成本汇总。
func (l *Ledger) CostTotals(ctx context.Context, deploymentID string) (CostTotals, error) {
	return l.store.costTotals(ctx, deploymentID)
}

// Facts 返回段的全部事实（只读对账/测试）。
func (l *Ledger) Facts(ctx context.Context, segmentID string) ([]CostFact, error) {
	return l.store.listFacts(ctx, segmentID)
}

// Segment 读取单个段（只读）。
func (l *Ledger) Segment(ctx context.Context, segmentID string) (Segment, error) {
	return l.store.segmentByID(ctx, segmentID)
}

// ActiveSegmentForDeployment 返回部署最近的未终态段（worker 判重）。
func (l *Ledger) ActiveSegmentForDeployment(ctx context.Context, deploymentID string) (Segment, error) {
	return l.store.activeSegmentByDeployment(ctx, deploymentID)
}

// writeAccrualsTx 按整点切分写 accrual 事实，返回实际新增行数。
func (l *Ledger) writeAccrualsTx(ctx context.Context, q queryer, seg Segment, end time.Time,
	includeOpen bool, source string, providerCumulative *int64) (int, error) {
	if seg.StartedAt == nil || !end.After(*seg.StartedAt) {
		return 0, nil
	}
	periods := splitPeriods(*seg.StartedAt, end, includeOpen)
	maxBillable := seg.PlannedSeconds + DeploymentStopGraceSeconds
	unitPrice := seg.UnitPriceUSDPerGPUSecond
	qpu := l.quotaPerUnit()
	now := l.now().UTC()
	inserted := 0
	for _, p := range periods {
		seconds := p.seconds()
		if seconds <= 0 {
			continue
		}
		elapsedBefore := int64(p.SegmentStart.Sub(*seg.StartedAt) / time.Second)
		allowed := maxBillable - elapsedBefore
		if allowed <= 0 {
			continue
		}
		if seconds > allowed {
			seconds = allowed
		}
		gpuSeconds := int64(seg.GPUsPerContainer) * int64(seg.ReplicaCount) * seconds
		if gpuSeconds > MaxDeploymentGPUSecondsPerSegment {
			gpuSeconds = MaxDeploymentGPUSecondsPerSegment
		}
		credits, clamp := pricing.WalletQuotaFromDecimalChecked(
			decimal.NewFromInt(gpuSeconds).Mul(unitPrice).Mul(decimal.NewFromInt(qpu)))
		if clamp != nil {
			l.log.Warn("deploy billing: quota saturated at accrual",
				"segment_id", seg.SegmentID, "kind", clamp.Kind, "op", clamp.Op)
		}
		periodStart := p.Start
		periodEnd := p.SegmentEnd
		fact := CostFact{
			FactKey:                   FactKey(FactAccrual, seg.SegmentID, &periodStart),
			SegmentID:                 seg.SegmentID,
			DeploymentID:              seg.DeploymentID,
			FactType:                  FactAccrual,
			PeriodStart:               &periodStart,
			PeriodEnd:                 &periodEnd,
			GPUsPerContainer:          seg.GPUsPerContainer,
			ReplicaCount:              seg.ReplicaCount,
			GPUSeconds:                gpuSeconds,
			Unit:                      UnitGPUSecond,
			UsageSource:               source,
			EstimatedCredits:          credits,
			State:                     FactAccrued,
			ProviderCumulativeSeconds: providerCumulative,
			CreatedAt:                 now,
		}
		ok, err := l.store.insertFact(ctx, q, fact)
		if err != nil {
			return inserted, err
		}
		if ok {
			inserted++
		}
	}
	return inserted, nil
}

func (l *Ledger) normalizeUsage(seg Segment, fact GPUUsageFact) (GPUUsageFact, error) {
	if fact.SegmentID == "" {
		fact.SegmentID = seg.SegmentID
	}
	if fact.DeploymentID == "" {
		fact.DeploymentID = seg.DeploymentID
	}
	if fact.SegmentID != seg.SegmentID || fact.DeploymentID != seg.DeploymentID {
		return GPUUsageFact{}, fmt.Errorf("%w: usage fact does not belong to segment", ErrInvalidSegment)
	}
	if fact.GPUsPerContainer == 0 {
		fact.GPUsPerContainer = seg.GPUsPerContainer
	}
	if fact.ReplicaCount == 0 {
		fact.ReplicaCount = seg.ReplicaCount
	}
	// 乘数必须与段快照一致：结算事实不能用调用方临时传入的乘数改写
	// 段内「每 GPU·秒 × 乘数」的口径（LOW 复核）。
	if fact.GPUsPerContainer != seg.GPUsPerContainer || fact.ReplicaCount != seg.ReplicaCount {
		return GPUUsageFact{}, fmt.Errorf("%w: usage multipliers must match the segment snapshot", ErrInvalidSegment)
	}
	if fact.GPUsPerContainer < 1 || fact.GPUsPerContainer > MaxDeploymentGPUsPerContainer ||
		fact.ReplicaCount < 1 || fact.ReplicaCount > MaxDeploymentReplicas {
		return GPUUsageFact{}, fmt.Errorf("%w: usage multipliers", ErrInvalidSegment)
	}
	if fact.GPUSeconds < 0 {
		return GPUUsageFact{}, fmt.Errorf("%w: negative gpu_seconds", ErrInvalidSegment)
	}
	// 结算量不得超过本段上界（含 300s 宽限），否则会绕过预扣/账单校验
	// 写入超量成本（LOW 复核）。
	if fact.GPUSeconds > SegmentMaxGPUSeconds(seg) {
		return GPUUsageFact{}, fmt.Errorf("%w: gpu_seconds %d exceeds segment max %d",
			ErrInvalidSegment, fact.GPUSeconds, SegmentMaxGPUSeconds(seg))
	}
	if fact.Source == "" {
		fact.Source = UsageInternalAccounting
	}
	switch fact.Source {
	case UsageProviderExact, UsageProviderPartial, UsageInternalAccounting, UsageEstimated:
	default:
		return GPUUsageFact{}, fmt.Errorf("%w: usage source %q", ErrInvalidSegment, fact.Source)
	}
	return fact, nil
}

func (l *Ledger) billableEnd(seg Segment, now time.Time) time.Time {
	if seg.StartedAt == nil {
		return now
	}
	maxEnd := seg.StartedAt.Add(time.Duration(seg.PlannedSeconds+DeploymentStopGraceSeconds) * time.Second)
	if now.After(maxEnd) {
		return maxEnd
	}
	return now
}

func (l *Ledger) recordAudit(ctx context.Context, action string, detail map[string]any) {
	if l.audit == nil {
		return
	}
	if err := l.audit(ctx, action, detail); err != nil && ctx.Err() == nil {
		l.log.Warn("deploy billing: audit write failed", "action", action, "error", err)
	}
}

func normalizeEstimate(est SegmentEstimate) SegmentEstimate {
	if est.SegmentKind == "" {
		est.SegmentKind = SegmentKindCreate
	}
	if est.PriceSource == "" {
		est.PriceSource = PriceSourceProviderQuote
	}
	if est.State == "" {
		if est.StartedAt != nil {
			est.State = SegmentRunning
		} else {
			est.State = SegmentPendingConfirm
		}
	}
	return est
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

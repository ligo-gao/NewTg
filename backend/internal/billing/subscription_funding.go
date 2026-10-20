package billing

// 订阅资金来源（S4a；billing-04 §3、billing-08 §4）。
//
// 本文件只负责「从哪扣」与「订阅预扣/退款」：每请求解析一次用户偏好并
// 冻结候选订阅，按四种偏好选择钱包或订阅；订阅侧先扣令牌额度，再走
// `PreConsumeUserSubscription`，失败时补偿回滚令牌。
//
// 明确不在本文件：结算 delta（`PostConsumeUserSubscriptionDelta`）、
// 日志键、崩溃恢复——它们属于 S4b。Session 对订阅来源的 Terminate /
// ChargeIncrement / Reserve 因此只做「不碰钱包」的保护，真正的订阅结算
// 由 S4b 接入。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// SubscriptionAccount 是资金分派需要的订阅实例只读视图。
//
// 刻意不直接使用 store.Subscription：资金侧只依赖这 7 个字段，
// 测试可以构造最小假仓储，而不必复制订阅表的全部快照列。
type SubscriptionAccount struct {
	ID                  int64
	EndAt               time.Time
	GraceDays           int32
	AmountTotal         int64
	AmountUsed          int64
	AllowWalletOverflow bool
	ModelScope          []byte
}

// SubscriptionFundingSource 是订阅域对资金分派暴露的接缝
// （billing-08 §4.1；方法语义见 billing-04 §3.2/§3.4/§4）。
type SubscriptionFundingSource interface {
	// BillingPreference 读取 users.billing_preference 并归一化；未知/空值
	// 必须返回 subscription_first（billing-04 §3.2）。
	BillingPreference(ctx context.Context, userID int64) (contract.BillingPreference, error)
	// ListActiveSubscriptions 返回 status='active' 且 end_at+宽限 > now 的
	// 实例，按 end_at asc, id asc（billing-08 §4.1）。
	ListActiveSubscriptions(ctx context.Context, userID int64, now time.Time) ([]SubscriptionAccount, error)
	// PreConsumeUserSubscription 单实例预扣：requestId 幂等，created=false
	// 表示命中既有记录（本次未再扣额度）。
	PreConsumeUserSubscription(ctx context.Context, requestID string, subscriptionID, amount int64) (rec store.SubscriptionPreConsume, created bool, err error)
	// MarkPreConsumeRefunded 按 requestId 幂等退款（含 amount_used 回退）。
	MarkPreConsumeRefunded(ctx context.Context, requestID string) error
	// PreConsumeRecord 按 requestId 读取预扣记录及其订阅归属用户。
	//
	// 任务失败退款（RefundByRequestIDForToken）只有 requestID 与 tokenID：
	// 订阅来源没有 wallet_transactions hold 行，必须靠这条记录判断「本次
	// 请求是否由订阅预扣」以及回补令牌所需的 userID。记录不存在时返回
	// store.ErrPreConsumeNotFound（调用方回退到钱包路径）。
	PreConsumeRecord(ctx context.Context, requestID string) (rec store.SubscriptionPreConsume, userID int64, err error)
}

// StoreSubscriptionFunding 把 store.SubscriptionRepo 适配为资金接缝。
//
// 偏好属于 users 行，D5a 的订阅仓储刻意不覆盖它（见
// SubscriptionHandlerConfig 的注释），因此这里直接读同一连接池。
type StoreSubscriptionFunding struct {
	repo store.SubscriptionRepo
	db   *sql.DB
}

// NewStoreSubscriptionFunding 构造生产适配器；repo 与 db 必须非 nil。
func NewStoreSubscriptionFunding(repo store.SubscriptionRepo, db *sql.DB) *StoreSubscriptionFunding {
	return &StoreSubscriptionFunding{repo: repo, db: db}
}

// BillingPreference 实现偏好读取与归一化。
//
// 归一化只发生在读取侧：DB CHECK 已限定四值，但历史/迁移数据仍可能为空
// 或未知；资金路径不能因为一个脏值就拒绝服务，也不能默认成 wallet_only
// （那会让订阅用户的请求静默改扣钱包）。按冻结语义统一归一为
// subscription_first（billing-04 §3.2）。
func (f *StoreSubscriptionFunding) BillingPreference(ctx context.Context, userID int64) (contract.BillingPreference, error) {
	var raw string
	err := f.db.QueryRowContext(ctx,
		`SELECT billing_preference FROM users WHERE id = $1`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: user %d", ErrUserNotFound, userID)
	}
	if err != nil {
		return 0, fmt.Errorf("billing: read billing preference for user %d: %w", userID, err)
	}
	pref, parseErr := store.ParseBillingPreference(strings.TrimSpace(raw))
	if parseErr != nil {
		return contract.PrefSubscriptionFirst, nil
	}
	return normalizeFundingPreference(pref), nil
}

// ListActiveSubscriptions 转发并裁剪为资金侧字段。
func (f *StoreSubscriptionFunding) ListActiveSubscriptions(ctx context.Context, userID int64, now time.Time) ([]SubscriptionAccount, error) {
	subs, err := f.repo.ListActiveSubscriptions(ctx, userID, now)
	if err != nil {
		return nil, err
	}
	out := make([]SubscriptionAccount, 0, len(subs))
	for _, sub := range subs {
		out = append(out, SubscriptionAccount{
			ID:                  sub.ID,
			EndAt:               sub.EndAt,
			GraceDays:           sub.GraceDays,
			AmountTotal:         sub.AmountTotal,
			AmountUsed:          sub.AmountUsed,
			AllowWalletOverflow: sub.AllowWalletOverflow,
			ModelScope:          sub.ModelScope,
		})
	}
	return out, nil
}

// PreConsumeUserSubscription 转发到订阅仓储的事务实现。
func (f *StoreSubscriptionFunding) PreConsumeUserSubscription(ctx context.Context,
	requestID string, subscriptionID, amount int64) (store.SubscriptionPreConsume, bool, error) {
	return f.repo.PreConsumeUserSubscription(ctx, requestID, subscriptionID, amount)
}

// MarkPreConsumeRefunded 转发到订阅仓储的幂等退款实现。
func (f *StoreSubscriptionFunding) MarkPreConsumeRefunded(ctx context.Context, requestID string) error {
	return f.repo.MarkPreConsumeRefunded(ctx, requestID)
}

// PreConsumeRecord 读取预扣记录并解析归属用户。
//
// 预扣表刻意不存 user_id（5 列冻结，R-D5-02）：归属从 subscription_id 对应的
// 实例读取。两个查询都在调用方相同的 ops 上下文内执行；记录不存在时原样
// 返回 store.ErrPreConsumeNotFound，让调用方无歧义地回退钱包路径。
func (f *StoreSubscriptionFunding) PreConsumeRecord(ctx context.Context, requestID string) (store.SubscriptionPreConsume, int64, error) {
	rec, err := f.repo.GetPreConsume(ctx, requestID)
	if err != nil {
		return store.SubscriptionPreConsume{}, 0, err
	}
	sub, err := f.repo.GetSubscription(ctx, rec.SubscriptionID)
	if err != nil {
		return store.SubscriptionPreConsume{}, 0, fmt.Errorf(
			"billing: read subscription %d of pre-consume %s: %w", rec.SubscriptionID, requestID, err)
	}
	return rec, sub.UserID, nil
}

// fundingPlan 是一次请求冻结下来的来源选择结果。
//
// 冻结发生在 PreConsume 内（billing-04 §3.3）：结算/回退必须复用同一份
// 候选列表与偏好，不能在中途重新读取 users.billing_preference，否则同一
// 请求可能先按订阅预扣、再按钱包结算。
type fundingPlan struct {
	preference contract.BillingPreference
	// candidates 已按 end_at asc, id asc 排序，且已通过 model_scope 过滤。
	candidates []SubscriptionAccount
	// allowOverflow 是「任一活跃订阅允许钱包溢出」的快照。
	allowOverflow bool
	// hasActive 表示用户存在至少一个活跃订阅（过滤 model_scope 之前）。
	// 它区分两种「没有候选」：完全没订阅 → subscription_first 直接走钱包；
	// 有活跃订阅但全部不覆盖当前模型 → 按订阅失败处理（M1）。
	hasActive bool
}

// primaryWallet 判断主来源是否为钱包。
//
//   - wallet_only / wallet_first：钱包；
//   - subscription_only：订阅（即使没有候选也不能改扣钱包）；
//   - subscription_first：只有「完全没有活跃订阅」才直接走钱包。有活跃订阅
//     但 model_scope 全部不匹配时仍按订阅失败处理，由溢出开关决定是否回退。
func (p *fundingPlan) primaryWallet() bool {
	switch p.preference {
	case contract.PrefWalletOnly, contract.PrefWalletFirst:
		return true
	case contract.PrefSubscriptionFirst:
		return len(p.candidates) == 0 && !p.hasActive
	default: // PrefSubscriptionOnly
		return false
	}
}

// normalizeFundingPreference 把越界枚举归一为默认偏好。
//
// store.ParseBillingPreference 已拒绝未知文本；这里再兜一层是为了让
// 假仓储/未来枚举扩展不会改变资金方向。
func normalizeFundingPreference(p contract.BillingPreference) contract.BillingPreference {
	switch p {
	case contract.PrefSubscriptionFirst, contract.PrefSubscriptionOnly,
		contract.PrefWalletOnly, contract.PrefWalletFirst:
		return p
	default:
		return contract.PrefSubscriptionFirst
	}
}

// resolveFundingPlanLocked 解析并冻结本请求的资金来源（调用方持有 s.mu）。
//
// 每请求只查一次偏好与活跃订阅。未装配订阅仓储时保持纯钱包行为（与 S4a
// 之前完全一致）：装配缺失是运维事实，只有当订阅功能上线并写入用户偏好后
// 才需要阻断；此时引擎在读偏好前就无法判断偏好，强行失败会让所有既有
// 部署不可用。
func (s *Session) resolveFundingPlanLocked(ctx context.Context) (*fundingPlan, error) {
	if s.fundingPlan != nil {
		return s.fundingPlan, nil
	}
	plan := &fundingPlan{preference: contract.PrefWalletOnly}
	funding := s.engine.subscriptionFunding
	if funding == nil {
		s.fundingPlan = plan
		return plan, nil
	}

	pref, err := funding.BillingPreference(ctx, s.userID)
	if err != nil {
		return nil, err
	}
	plan.preference = normalizeFundingPreference(pref)

	// wallet_only 不需要订阅候选：跳过活跃订阅查询，避免给纯钱包用户增加
	// 一次与资金方向无关的数据库查询（LOW）。wallet_first 仍要查，因为它的
	// 回退路径依赖候选。
	if plan.preference == contract.PrefWalletOnly {
		s.fundingPlan = plan
		return plan, nil
	}

	subs, err := funding.ListActiveSubscriptions(ctx, s.userID, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("billing: list active subscriptions: %w", err)
	}
	plan.hasActive = len(subs) > 0
	for _, sub := range subs {
		if sub.AllowWalletOverflow {
			plan.allowOverflow = true
		}
		ok, scopeErr := modelScopeAllows(sub.ModelScope, s.model)
		if scopeErr != nil {
			return nil, scopeErr
		}
		if !ok {
			continue
		}
		plan.candidates = append(plan.candidates, sub)
	}
	// 仓储已按 end_at asc, id asc 排序；这里再稳定排序一次，让假仓储/未来
	// 查询变更无法破坏「最早到期优先」的冻结语义（billing-08 §4.1）。
	sort.SliceStable(plan.candidates, func(i, j int) bool {
		if plan.candidates[i].EndAt.Equal(plan.candidates[j].EndAt) {
			return plan.candidates[i].ID < plan.candidates[j].ID
		}
		return plan.candidates[i].EndAt.Before(plan.candidates[j].EndAt)
	})
	s.fundingPlan = plan
	return plan, nil
}

// modelScopeAllows 判定订阅是否覆盖当前模型。
//
// 两形态（billing-08 §4.3-5；NULL 与 [] 语义由平台冻结）：
//   - NULL / 空字节：全部模型可用（NULL 表示未设置白名单）；
//   - JSON 数组：空数组 = 没有任何模型可用；
//   - 元素支持精确匹配（"gpt-4o"）与前缀匹配（"openai/*"），前缀形态只认
//     末尾的 "/*"，避免把任意通配符当成匹配语义。
//
// 非法 JSON 返回错误（fail closed）：把损坏的白名单当成「全部模型」会让
// 订阅被错误用于套餐外模型。
func modelScopeAllows(scope []byte, model string) (bool, error) {
	if len(scope) == 0 || strings.TrimSpace(string(scope)) == "" {
		return true, nil
	}
	var patterns []string
	if err := json.Unmarshal(scope, &patterns); err != nil {
		return false, fmt.Errorf("%w: invalid subscription model_scope: %v", ErrInvalidConfig, err)
	}
	if len(patterns) == 0 {
		return false, nil
	}
	for _, pattern := range patterns {
		if pattern == model {
			return true, nil
		}
		if strings.HasSuffix(pattern, "/*") &&
			strings.HasPrefix(model, strings.TrimSuffix(pattern, "*")) {
			return true, nil
		}
	}
	return false, nil
}

// chargeFundingLocked 按冻结的偏好实际扣减（调用方持有 s.mu）。
//
// 回退规则是**不对称**的（billing-04 §3.2）：
//   - wallet_first 只在钱包返回「用户额度不足」专用错误时回退订阅；
//   - subscription_first 只看溢出开关，与订阅失败的错误码无关；
//   - *_only 没有回退。
func (s *Session) chargeFundingLocked(ctx context.Context, plan *fundingPlan, budget int64) error {
	switch plan.preference {
	case contract.PrefWalletOnly:
		return s.chargeWalletLocked(ctx, budget)
	case contract.PrefSubscriptionOnly:
		if len(plan.candidates) == 0 {
			return subscriptionQuotaError(budget)
		}
		return s.chargeSubscriptionLocked(ctx, budget, plan.candidates)
	case contract.PrefWalletFirst:
		err := s.chargeWalletLocked(ctx, budget)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrInsufficientQuota) {
			// DB 错误等非额度错误绝不回退：回退会掩盖故障并把请求转到
			// 另一个账户，违反 wallet_first 的冻结语义（TC-D-15）。
			return err
		}
		if len(plan.candidates) == 0 {
			return err
		}
		return s.chargeSubscriptionLocked(ctx, budget, plan.candidates)
	default: // contract.PrefSubscriptionFirst
		if len(plan.candidates) == 0 {
			// 有活跃订阅但 model_scope 全部不匹配：按订阅失败处理（仅允许
			// 溢出的实例可回退钱包），不能静默改扣钱包（M1）。完全没有活跃
			// 订阅的情况已在 primaryWallet 直接走钱包，不会到这里。
			if !plan.hasActive || plan.allowOverflow {
				return s.chargeWalletLocked(ctx, budget)
			}
			return subscriptionQuotaError(budget)
		}
		err := s.chargeSubscriptionLocked(ctx, budget, plan.candidates)
		if err == nil {
			return nil
		}
		// 「已退款再预扣」是双花防护，不能以回退钱包的方式绕过。
		if errors.Is(err, store.ErrPreConsumeRefunded) {
			return err
		}
		if !plan.allowOverflow {
			return err
		}
		return s.chargeWalletLocked(ctx, budget)
	}
}

// chargeWalletLocked 是原有钱包预扣路径（令牌 + 钱包同事务）。
func (s *Session) chargeWalletLocked(ctx context.Context, budget int64) error {
	if budget == 0 {
		// 估算为 0（无计费用量/零价）：不产生 hold。结算时若真实用量为正，
		// 走「少补」直接扣减（billing-00 §6.5）。
		s.fundingSource = contract.FundingWallet
		return nil
	}
	if _, err := s.engine.ledger.ApplyDeltaWithToken(ctx, s.userID, s.tokenID, -budget,
		"consume_pre", "hold", "usage_log", s.requestID); err != nil {
		return err // 余额/令牌额度不足时是 *QuotaError
	}
	s.preConsumed = budget
	s.tokenDeducted = budget
	s.fundingSource = contract.FundingWallet
	// 预扣成功后写租约：只有真实 hold 才需要崩溃兜底。
	if err := s.registerLeaseLocked(ctx); err != nil {
		return err
	}
	return nil
}

// chargeSubscriptionLocked 从候选订阅中逐个尝试预扣。
//
// 顺序契约（billing-04 §3.3；billing-08 §4.1）：先令牌额度、后订阅账户。
// 两者不在同一事务里（订阅仓储走 database/sql 的独立事务），故订阅预扣
// 失败时必须用 CreditToken 精确回补同一金额——不能少扣，也不能多扣。
//
// 订阅预扣量：budget <= 0 时置 1（订阅预扣要求 amount > 0，billing-04
// §3.2「订阅预扣量构造」）。
func (s *Session) chargeSubscriptionLocked(ctx context.Context, budget int64, candidates []SubscriptionAccount) error {
	if s.engine.subscriptionFunding == nil {
		return fmt.Errorf("%w: subscription funding is not configured", ErrInvalidConfig)
	}
	amount := budget
	if amount <= 0 {
		amount = 1
	}

	// ① 先令牌额度。令牌也不足时直接 402，不再尝试订阅（订阅扣款同样
	//    要求令牌额度可扣）。
	if err := s.engine.ledger.DebitToken(ctx, s.userID, s.tokenID, amount); err != nil {
		return err
	}

	var lastErr error
	for _, sub := range candidates {
		rec, created, err := s.engine.subscriptionFunding.PreConsumeUserSubscription(
			ctx, s.requestID, sub.ID, amount)
		if err != nil {
			if errors.Is(err, store.ErrSubscriptionQuotaInsufficient) ||
				errors.Is(err, store.ErrSubscriptionNotActive) {
				// 单实例不可用：继续下一个候选（等价于「金额不足则 continue」）。
				lastErr = err
				continue
			}
			// 存储故障/幂等硬错误：不得继续扣订阅，也不得改扣钱包；
			// 先回滚令牌（失败语义见 frozen 规则 9：存储故障不降级为免费）。
			return s.abortSubscriptionFunding(ctx, amount, err)
		}

		if !created {
			// 幂等回放（同 requestId 的重复会话）：订阅侧没有再次扣款，
			// 本会话刚刚的令牌扣减必须原额退回，否则同一请求会双扣令牌。
			if creditErr := s.engine.ledger.CreditToken(ctx, s.userID, s.tokenID, amount); creditErr != nil {
				return fmt.Errorf("billing: rollback token after idempotent subscription replay: %w", creditErr)
			}
			s.adoptSubscriptionPreConsume(rec, 0, true)
			return nil
		}

		s.adoptSubscriptionPreConsume(rec, amount, false)
		return nil
	}

	// 所有候选都不可用/额度不足：把令牌原额退回，再返回 402。
	cause := error(subscriptionQuotaError(budget))
	if lastErr != nil {
		cause = fmt.Errorf("%w: %v", subscriptionQuotaError(budget), lastErr)
	}
	return s.abortSubscriptionFunding(ctx, amount, cause)
}

// adoptSubscriptionPreConsume 冻结本请求的订阅来源信息。
//
// preConsumed/tokenDeducted 对创建路径等于本次 token 扣减额；幂等回放路径
// 为 0（本会话没有可退的令牌额度，订阅记录由首个执行者持有）。
// replayed=true 还会置位 subscriptionReplayed，让 Refund/NeedsRefund 永不
// 去退首执行者的记录（重复退款会白送额度）。
func (s *Session) adoptSubscriptionPreConsume(rec store.SubscriptionPreConsume, tokenDeducted int64, replayed bool) {
	s.fundingSource = contract.FundingSubscription
	s.subscriptionID = rec.SubscriptionID
	s.subscriptionReplayed = replayed
	if replayed {
		// 回放会话不持有任何可退金额：preConsumed 保持 0，NeedsRefund 为
		// false；记录本身仍是首执行者的资产。
		s.subscriptionPreConsumed = 0
		s.preConsumed = 0
		s.tokenDeducted = 0
		return
	}
	s.subscriptionPreConsumed = rec.PreConsumed
	s.preConsumed = rec.PreConsumed
	s.tokenDeducted = tokenDeducted
}

// abortSubscriptionFunding 在订阅预扣失败时补偿回滚令牌额度。
//
// 回滚自身失败时返回组合错误：调用方必须把它当作 5xx（存储故障），
// 不能当作 402——那会让一笔已经扣掉的令牌额度被静默吞掉。
func (s *Session) abortSubscriptionFunding(ctx context.Context, amount int64, cause error) error {
	if err := s.engine.ledger.CreditToken(ctx, s.userID, s.tokenID, amount); err != nil {
		return fmt.Errorf("billing: rollback token quota after subscription pre-consume failure: %w (cause: %v)",
			err, cause)
	}
	return cause
}

// subscriptionQuotaError 构造与钱包不足同口径的 *QuotaError（HTTP 402）。
func subscriptionQuotaError(required int64) error {
	return &QuotaError{
		Class:     contract.ErrInsufficientQuota,
		Required:  required,
		Available: 0,
	}
}

// refundSubscriptionPreConsume 按 requestId 退款，最多 3 次、退避 200/400ms
// （billing-04 §4：订阅退款幂等、可重试；钱包退款非幂等、不重试）。
//
// 仅在第一次失败后等待；成功/已退款立即返回。ctx 用于响应取消，避免
// 退避期间把中继 goroutine 挂住。
func (e *Engine) refundSubscriptionPreConsume(ctx context.Context, requestID string) error {
	if e.subscriptionFunding == nil {
		return fmt.Errorf("%w: subscription funding is not configured", ErrInvalidConfig)
	}
	backoff := [...]time.Duration{200 * time.Millisecond, 400 * time.Millisecond}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff[attempt-1]):
			}
		}
		if err := e.subscriptionFunding.MarkPreConsumeRefunded(ctx, requestID); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

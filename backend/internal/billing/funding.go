package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// 本文件是 M5 的「资金来源」抽象与充值金额换算（modules-04 §3 / F-04-01、
// platform-01 §9/§10；裁决 P-3/P-4/P-6/P-11）。
//
// 边界：FundingSource 只负责「把积分记到某用户余额上」这一件事。订单状态
// 机、兑换码核销、渠道回调都在 Task 2–5 完成；它们保证同一 order /
// redemption code 至多调用一次 Apply，因此这里不做幂等键。

// 充值定价的冻结默认值（P-6）。它们必须与站点配置同名键的默认值一致，
// 待站点配置服务在 Task 2 接入后改由配置提供；在此之前作为唯一默认来源，
// 避免各调用点硬编码不同数字。
const (
	// DefaultQuotaPerUnit = billing.quota_per_unit：1 元 = 100 积分
	// （modules-00 §4.7-7、modules-03 R4：1 积分 = 1 分）。
	DefaultQuotaPerUnit int64 = 100
	// DefaultMinTopupCents = pay.min_topup_cny 默认 10 元（最小货币单位=分）。
	DefaultMinTopupCents int64 = 1_000
	// DefaultProviderMinTopupCents = pay.<provider>.min_topup_cny 默认 10 元。
	// 渠道最低与全局最低取 max（F-04-01 步骤 4）。
	DefaultProviderMinTopupCents int64 = 1_000
	// MaxTopupCents 是单笔充值硬上限：¥100,000 = 10,000,000 分
	// （F-04-01 异常流程【假设】；modules-99 的统一裁决）。
	MaxTopupCents int64 = 10_000_000
)

// 换算与校验的哨兵错误；调用方用 errors.Is 区分失败面。
var (
	// ErrInvalidTopupAmount 覆盖金额 ≤0、低于下限、超过上限。
	ErrInvalidTopupAmount = errors.New("billing: invalid topup amount")
	// ErrInvalidQuotaPerUnit 表示 billing.quota_per_unit 配置非法。
	ErrInvalidQuotaPerUnit = errors.New("billing: invalid quota per unit")
	// ErrInvalidTopupRatio 表示 pay.topup_group_ratio 中的倍率非法（≤0 或非数字）。
	ErrInvalidTopupRatio = errors.New("billing: invalid topup group ratio")
	// ErrCreditOverflow 表示换算结果超出 int64（绝不能静默钳制后入账）。
	ErrCreditOverflow = errors.New("billing: topup credits overflow int64")
	// ErrInvalidFundingCredit 表示 FundingSource.Apply 的入参非法。
	ErrInvalidFundingCredit = errors.New("billing: invalid funding credit")
)

// 资金来源流水的 type 取值（wallet_transactions.type 的子集）。
// 充值一律 topup（P-3）；bonus / admin_adjustment 是同一资金来源抽象的
// 其它合法用途（赠送、管理员调整），支付渠道永远不进入 type。
const (
	CreditTypeTopup           = "topup"
	CreditTypeBonus           = "bonus"
	CreditTypeAdminAdjustment = "admin_adjustment"
)

// Credit 是一次「资金来源 → 钱包余额」的入账请求。
//
// Amount 的单位是积分（已由 ComputeCredits 换算），不是分。
// ReferenceType / ReferenceID 是钱包流水的关联锚点（P-3）：
//   - 在线充值：reference_type='order'，reference_id=orders.id；
//   - 兑换码：reference_type='redemption'，reference_id=redemption_codes.id。
//
// 绝不使用 'stripe' / 'alipay' 等渠道名作 reference_type——渠道由订单行
// 承载，流水只关联订单，否则按渠道对账会与订单口径分叉。
type Credit struct {
	UserID        int64
	Amount        int64
	Type          string
	ReferenceType string
	ReferenceID   string
}

// FundingSource 是「入账」的唯一抽象。M5 的充值、兑换码与后续人工调整
// 都通过它落到钱包，避免各业务路径直接操作 users.balance。
//
// 返回 txID 为本次生成的 wallet_transactions.id（十进制文本），供订单
// 的 funded_tx_id / 兑换审计关联；balanceAfter 是同事务内的余额快照。
type FundingSource interface {
	Apply(ctx context.Context, credit Credit) (balanceAfter int64, txID string, err error)
}

// walletFunding 是基于 M3 账本的默认实现。
type walletFunding struct {
	ledger *Ledger
}

// NewWalletFunding 构造默认 FundingSource；db 必须是主库 GORM 连接
// （wallet_transactions 与 users 同库，不能使用日志库 DSN）。
func NewWalletFunding(db *gorm.DB) FundingSource {
	return &walletFunding{ledger: NewLedger(db)}
}

// Apply 在同一事务内完成「余额增加 + 恰一行流水」，委托 M3 ApplyDeltaID。
//
// 状态固定为 confirmed：充值/兑换是即时确认的入账，不经过 hold 生命周期
// （hold 只属于 consume_pre，platform-01 §11）。
func (w *walletFunding) Apply(ctx context.Context, credit Credit) (int64, string, error) {
	if err := credit.validate(); err != nil {
		return 0, "", err
	}
	balanceAfter, txID, err := w.ledger.ApplyDeltaID(ctx, credit.UserID, credit.Amount,
		credit.Type, "confirmed", credit.ReferenceType, credit.ReferenceID)
	if err != nil {
		return 0, "", err
	}
	return balanceAfter, strconv.FormatInt(txID, 10), nil
}

// validate 在触库之前拦截非法入账，避免把可读性差的 CHECK 违例抛给上层。
func (c Credit) validate() error {
	if c.UserID <= 0 {
		return fmt.Errorf("%w: user id must be > 0", ErrInvalidFundingCredit)
	}
	if c.Amount <= 0 {
		return fmt.Errorf("%w: amount must be > 0, got %d", ErrInvalidFundingCredit, c.Amount)
	}
	switch c.Type {
	case CreditTypeTopup, CreditTypeBonus, CreditTypeAdminAdjustment:
	default:
		return fmt.Errorf("%w: type %q is not a funding credit type", ErrInvalidFundingCredit, c.Type)
	}
	if !validWalletRefType[c.ReferenceType] {
		return fmt.Errorf("%w: invalid reference type %q", ErrInvalidFundingCredit, c.ReferenceType)
	}
	// P-3：充值必须锚定订单或兑换码，且锚点 id 不能为空——没有锚点的
	// 充值流水无法对账，也无从证明资金来自哪笔订单。
	if c.Type == CreditTypeTopup {
		if c.ReferenceType != "order" && c.ReferenceType != "redemption" {
			return fmt.Errorf("%w: topup reference type must be order or redemption, got %q",
				ErrInvalidFundingCredit, c.ReferenceType)
		}
		if c.ReferenceID == "" {
			return fmt.Errorf("%w: topup reference id is required", ErrInvalidFundingCredit)
		}
	}
	// 非充值入账（bonus/admin_adjustment）可以无关联对象；有则 id 必须成对出现。
	if c.ReferenceType != "" && c.ReferenceID == "" {
		return fmt.Errorf("%w: reference id is required when reference type is %q",
			ErrInvalidFundingCredit, c.ReferenceType)
	}
	return nil
}

// ComputeCredits 把「分」换算为到账积分（P-6）：
//
//	credits = floor(amount_cents × quota_per_unit / 100 × ratio)
//
// 用 decimal 而不是浮点：ratio 来自管理员配置，浮点乘法在边界金额上会
// 产生 ±1 积分的误差，直接改变用户到账金额。乘以 0.01（decimal 精确表示）
// 而不是 Div，避免 shopspring 的除法精度（默认 16 位）在 floor 前四舍五入。
//
// 任何非正输入与 int64 溢出都返回错误：充值入账绝不允许饱和钳制——
// 少算或多算都是资损。
func ComputeCredits(amountCents, quotaPerUnit int64, ratio decimal.Decimal) (int64, error) {
	if amountCents <= 0 {
		return 0, fmt.Errorf("%w: amount %d cents must be > 0", ErrInvalidTopupAmount, amountCents)
	}
	if quotaPerUnit <= 0 {
		return 0, fmt.Errorf("%w: quota per unit %d must be > 0", ErrInvalidQuotaPerUnit, quotaPerUnit)
	}
	if ratio.Sign() <= 0 {
		return 0, fmt.Errorf("%w: ratio %s must be > 0", ErrInvalidTopupRatio, ratio)
	}

	exact := decimal.NewFromInt(amountCents).
		Mul(decimal.NewFromInt(quotaPerUnit)).
		Mul(ratio).
		Mul(decimal.New(1, -2)). // ×0.01：分 → 元，精确无除法
		Floor()
	if exact.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, fmt.Errorf("%w: %s credits exceed int64", ErrCreditOverflow, exact)
	}
	return exact.IntPart(), nil
}

// ValidateTopupAmount 校验单笔充值金额（最小货币单位=分）。
//
// 实际下限 = max(globalMinCents, providerMinCents)（F-04-01：各渠道可有
// 自己的最低充值）；上限是站点硬上限，渠道最低高于硬上限属于配置错误，
// 即使金额在上限内也必须拒绝（否则该渠道永远无法满足条件）。
// 全部参数非负；amountCents 必须 > 0。
func ValidateTopupAmount(amountCents, globalMinCents, providerMinCents, maxCents int64) error {
	if amountCents <= 0 {
		return fmt.Errorf("%w: amount %d cents must be > 0", ErrInvalidTopupAmount, amountCents)
	}
	if maxCents <= 0 {
		return fmt.Errorf("%w: max %d cents must be > 0", ErrInvalidTopupAmount, maxCents)
	}
	minCents := globalMinCents
	if providerMinCents > minCents {
		minCents = providerMinCents
	}
	if minCents < 0 {
		return fmt.Errorf("%w: minimum %d cents must be >= 0", ErrInvalidTopupAmount, minCents)
	}
	if minCents > maxCents {
		return fmt.Errorf("%w: minimum %d cents exceeds max %d cents", ErrInvalidTopupAmount, minCents, maxCents)
	}
	if amountCents < minCents {
		return fmt.Errorf("%w: amount %d cents below minimum %d cents", ErrInvalidTopupAmount, amountCents, minCents)
	}
	if amountCents > maxCents {
		return fmt.Errorf("%w: amount %d cents exceeds max %d cents", ErrInvalidTopupAmount, amountCents, maxCents)
	}
	return nil
}

// ParseTopupGroupRatios 解析站点键 pay.topup_group_ratio（jsonb `{group: ratio}`）。
//
// json.Number 而不是 float64：倍率一旦经过二进制浮点就可能变成 1.1999999…，
// 直接放大到积分面额。空输入按 `{}` 处理；任何 ≤0 或非数字值都视为配置
// 非法（F-04-01：ratio ≤ 0 启动期拒绝，不回退）。
func ParseTopupGroupRatios(raw []byte) (map[string]decimal.Decimal, error) {
	out := make(map[string]decimal.Decimal)
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}
	var parsed map[string]json.Number
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidTopupRatio, err)
	}
	for group, num := range parsed {
		d, err := decimal.NewFromString(num.String())
		if err != nil {
			return nil, fmt.Errorf("%w: group %q: %v", ErrInvalidTopupRatio, group, err)
		}
		if d.Sign() <= 0 {
			return nil, fmt.Errorf("%w: group %q ratio %s must be > 0", ErrInvalidTopupRatio, group, d)
		}
		out[group] = d
	}
	return out, nil
}

// GroupRatio 返回某分组在 pay.topup_group_ratio 中的倍率；未配置回退 1.0
// （不报错、不阻断充值——F-04-01 的明确裁决）。
func GroupRatio(ratios map[string]decimal.Decimal, group string) (decimal.Decimal, error) {
	if d, ok := ratios[group]; ok {
		if d.Sign() <= 0 {
			return decimal.Zero, fmt.Errorf("%w: group %q ratio %s must be > 0", ErrInvalidTopupRatio, group, d)
		}
		return d, nil
	}
	return decimal.NewFromInt(1), nil
}

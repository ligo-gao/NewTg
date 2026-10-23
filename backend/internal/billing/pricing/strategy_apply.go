package pricing

// strategy_apply.go 把 model_pricing.strategy 的时段倍率乘到已解析的快照上
// （spec §5.2）。时序：基础价（价列/表达式/按次/分组覆盖）解析完成后，
// 再乘时段倍率；分组覆盖价先应用，因为时段是**模型级**计划，分组只决定基础价。
//
// 【只乘绝对单价字段】ModelRatio（输入）、CompletionRatio（输出）、
// CacheRatio（缓存读）、CacheCreation5mRatio、CacheCreation1hRatio 是
// 「积分 / 1M tokens」的绝对价；ImageRatio/AudioRatio/AudioCompletionRatio
// 是相对输入价的乘数，乘一次会让图片/音频被折两次（spec §5.2 全局约束）。
//
// 【显式零】`-1` 是包内 explicitZeroRatio 哨兵（显式免费），乘法前必须
// 排除：0 × m 仍是 0，但它不是「配置了 0」，保持哨兵让下游语义不变。
//
// 【失败方向】策略 JSON 非法、策略与表达式并存、倍率越界都是配置错误，
// 返回 ErrInvalidSpec；绝不退化成 1（静默按原价）或 0（静默免费）。

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/pricingstrategy"
)

// stringsTrimmedNotEmpty 判断表达式列是否是「非空字符串」。
func stringsTrimmedNotEmpty(v string) bool { return strings.TrimSpace(v) != "" }

// applyTimeStrategy 解析 row.Strategy 并对快照应用该行在 at 时刻的时段倍率。
func (s *DBSource) applyTimeStrategy(ctx context.Context, snap contract.PriceSnapshot,
	row modelPricingRow, model string, at time.Time) (contract.PriceSnapshot, error) {

	// 表达式行与结构化策略互斥（spec §5.2 第 4 条）：同时存在说明管理面
	// 写入了互相矛盾的定价来源。静默选一个都会让账单与配置不一致。
	if row.BillingExpr != nil && stringsTrimmedNotEmpty(*row.BillingExpr) {
		return contract.PriceSnapshot{}, fmt.Errorf(
			"%w: model_pricing(%s) 同时配置了 billing_expr 与 strategy", ErrInvalidSpec, model)
	}

	strategy, err := pricingstrategy.Parse(row.Strategy)
	if err != nil {
		return contract.PriceSnapshot{}, fmt.Errorf(
			"%w: model_pricing(%s).strategy: %v", ErrInvalidSpec, model, err)
	}
	if !strategy.Time.Enabled {
		// 显式关闭：快照保持逐字段不变（TimeMultiplier 保持 0），公开面/日志
		// 与旧版一致。
		return snap, nil
	}

	match, err := strategy.MultiplierAt(ctx, at, s.holidayLookup)
	if err != nil {
		return contract.PriceSnapshot{}, fmt.Errorf(
			"%w: model_pricing(%s).strategy: %v", ErrInvalidSpec, model, err)
	}
	if match.HolidayLookupFailed {
		// 数据源不可达/当日无数据：已按星期规则继续（不失败、不免费），
		// 但必须可告警——否则调休日的错价会静默存在一整天。
		slog.Warn("pricing: holiday calendar lookup failed; using weekday schedule",
			"model", model, "at", at.Format(time.RFC3339))
	}

	return scaleSnapshotByTime(snap, match.Multiplier, match.Window, match.HolidayName)
}

// ApplyTimeMultiplier 把已解析的时段倍率乘到快照上，供管理面试算复用同一
// 实现（避免预览与结算出现两套乘法规则）。
//
// 与 applyTimeStrategy 的区别只在输入：调用方已经通过 pricingstrategy 算好
// 命中结果，不再查时间/节假日。倍率区间与 -1 哨兵的处理完全一致。
func ApplyTimeMultiplier(snap contract.PriceSnapshot, multiplier float64,
	window, holidayName string) (contract.PriceSnapshot, error) {
	return scaleSnapshotByTime(snap, multiplier, window, holidayName)
}

// scaleSnapshotByTime 把时段倍率 m 乘到快照的绝对单价字段上。
//
// 返回的 TimeMultiplier/TimeWindow/HolidayName 是**冻结事实**，进入
// PriceSnapshot 供用量日志与异步结算使用。
func scaleSnapshotByTime(snap contract.PriceSnapshot, m float64,
	window, holidayName string) (contract.PriceSnapshot, error) {

	if err := validateTimeMultiplier(m); err != nil {
		return contract.PriceSnapshot{}, err
	}

	p := snap.Price
	if p.UsePrice || snap.Mode == contract.BillingModePerCall {
		// 按次：ModelPrice 是「积分 / 次」的绝对价。
		v, err := scaleTimePrice("model_price", p.ModelPrice, m)
		if err != nil {
			return contract.PriceSnapshot{}, err
		}
		p.ModelPrice = v
		if v == 0 {
			p.FreeModel = true
		}
	} else {
		fields := []struct {
			name string
			ptr  *float64
		}{
			{"model_ratio", &p.ModelRatio},
			{"completion_ratio", &p.CompletionRatio},
			{"cache_ratio", &p.CacheRatio},
			{"cache_creation_5m_ratio", &p.CacheCreation5mRatio},
			{"cache_creation_1h_ratio", &p.CacheCreation1hRatio},
		}
		for _, f := range fields {
			v, err := scaleTimePrice(f.name, *f.ptr, m)
			if err != nil {
				return contract.PriceSnapshot{}, err
			}
			*f.ptr = v
		}
	}

	snap.Price = p
	snap.TimeMultiplier = m
	snap.TimeWindow = window
	snap.HolidayName = holidayName
	return snap, nil
}

// scaleTimePrice 乘单个绝对价字段：-1 哨兵与 0 原样保留，其余乘 m 并重新
// 校验有限非负（NaN/Inf/负值都是配置错误，不得进入计费）。
func scaleTimePrice(name string, v, m float64) (float64, error) {
	if v == explicitZeroRatio || v == 0 {
		return v, nil
	}
	out := v * m
	if math.IsNaN(out) || math.IsInf(out, 0) || out < 0 {
		return 0, fmt.Errorf("%w: %s 应用时段倍率 %v 后得到 %v", ErrInvalidPrice, name, m, out)
	}
	return out, nil
}

// validateTimeMultiplier 校验倍率区间（十进制倍数 0.01–100，spec §5.1）。
func validateTimeMultiplier(m float64) error {
	if math.IsNaN(m) || math.IsInf(m, 0) {
		return fmt.Errorf("%w: 时段倍率必须是有限数值（got %v）", ErrInvalidPrice, m)
	}
	if m < pricingstrategy.MinMultiplier || m > pricingstrategy.MaxMultiplier {
		return fmt.Errorf("%w: 时段倍率越界（%v，允许 %g–%g）",
			ErrInvalidPrice, m, pricingstrategy.MinMultiplier, pricingstrategy.MaxMultiplier)
	}
	return nil
}

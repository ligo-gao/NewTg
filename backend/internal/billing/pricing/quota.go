package pricing

import (
	"errors"
	"fmt"
	"math"
	"sync/atomic"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/contract"
)

// ============================================================================
// 配额数学（billing-00 §7.2/§7.3、billing-07 §2/§3 的唯一实现）
//
// 【为什么全部收敛到这一个文件】契约 §7.2 要求「禁止裸类型转换」：Go 的
// float64→int 越界行为是实现相关的（可能回绕成负数，形成「溢出产生负扣费」）。
// 任何配额换算都必须经过本文件的饱和内核，让越界行为显式化为「钳制到界值 +
// QuotaClamp 标记」。本包之外的代码不得写 int(float64(...)) / int(math.Round(...))。
//
// 三个取整族（截断 / 半离零 / 十进制 Round(0)）各有三个变体：
//   - 便捷版：只需值，静默饱和；
//   - *Checked：返回 (值, *QuotaClamp)，供结算路径审计；
//   - *Strict：把钳制转成 error，供预扣路径在写任何额度前阻断（§7.3）。
// 两条审计/阻断契约不得合并：把预扣改走 *Checked 会让超大配额以饱和值
// 尝试预扣，即 rules/billing.md 明令禁止的「静默回绕」。
// ============================================================================

// DefaultQuotaPerUnit 是「1 货币单位 = 多少积分」的 canonical 默认值。
//
// 【依据】计划 R-01 / modules-99 B-1：唯一键名 billing.quota_per_unit、唯一
// 默认值 100；billing-02 §4.1 的 QuotaPerUnit=500_000、USD=500、USD2RMB=7.3
// 已被废止（参照侧缺陷修复）。ComputeQuota 的冻结签名没有 QPU 入参，故以
// 进程级原子变量承载站点配置：NewSource 从配置中心读取后写入此处；单次
// ComputeQuota 只在入口读一次，运行期间的配置重载类似一次原子配置切换。
const DefaultQuotaPerUnit int64 = 100

// quotaPerUnit 是当前生效的 QuotaPerUnit。用 atomic.Int64 而不是普通变量，
// 因为配置重载（configcenter 广播）与请求热路径会并发读写。
var quotaPerUnit atomic.Int64

func init() {
	quotaPerUnit.Store(DefaultQuotaPerUnit)
}

// SetQuotaPerUnit 写入站点兑换比。范围与 modules-08 §F-08-09 一致：
// 1–1,000,000。非正或超界一律拒绝——静默接受会让整站金额换算失真。
func SetQuotaPerUnit(n int64) error {
	if n < 1 || n > 1_000_000 {
		return fmt.Errorf("pricing: quota_per_unit 越界（%d，允许 1–1000000）", n)
	}
	quotaPerUnit.Store(n)
	return nil
}

// CurrentQuotaPerUnit 返回当前生效的兑换比。
func CurrentQuotaPerUnit() int64 { return quotaPerUnit.Load() }

// ---------------------------------------------------------------------------
// 钳制标记与错误
// ---------------------------------------------------------------------------

// ClampError 把一次钳制包装成 error，供 *Strict 变体返回。
//
// 【为什么不直接让 *contract.QuotaClamp 实现 error】contract 是冻结契约包，
// 只承载类型与常量；Go 不允许为其它包的类型补方法。故用本包包装类型，
// 调用方通过 AsClamp 取回原始标记（字段与契约完全一致，不另造语义）。
type ClampError struct {
	Clamp *contract.QuotaClamp
}

// Error 实现 error。文案只用于诊断，面向用户的文案由 API 层映射。
func (e *ClampError) Error() string {
	if e == nil || e.Clamp == nil {
		return "pricing: <nil> quota clamp"
	}
	c := e.Clamp
	return fmt.Sprintf("pricing: quota clamped at %s: kind=%s original=%v clamped=%d",
		c.Op, clampKindName(c.Kind), c.Original, c.Clamped)
}

// AsClamp 从错误链中取出钳制标记。
func AsClamp(err error) (*contract.QuotaClamp, bool) {
	var ce *ClampError
	if errors.As(err, &ce) && ce != nil && ce.Clamp != nil {
		return ce.Clamp, true
	}
	return nil, false
}

// ClampKindName 返回钳制原因的稳定英文名，供日志与审计 map 使用。
func ClampKindName(k contract.ClampKind) string { return clampKindName(k) }

func clampKindName(k contract.ClampKind) string {
	switch k {
	case contract.ClampOverflow:
		return "overflow"
	case contract.ClampUnderflow:
		return "underflow"
	case contract.ClampNaN:
		return "nan"
	default:
		return "unknown"
	}
}

// ClampAuditMap 把标记渲染为 admin_info.quota_saturation 的 4 键 map
// （billing-07 §4.1）。key 集合固定为 op/kind/original/clamped。
//
// 【为什么 original 要做 JSON 安全化】QuotaClamp.Original 在 NaN/±Inf 场景
// 就是 NaN/±Inf，而 encoding/json 无法序列化这些值——直接落库会让整条
// usage_logs 写入失败，审计标记反而丢失。故按 JSON 的扩展字面量习惯输出
// 字符串 "NaN"/"+Inf"/"-Inf"，有限值保持数字。
func ClampAuditMap(c *contract.QuotaClamp) map[string]any {
	if c == nil {
		return nil
	}
	return map[string]any{
		"op":       c.Op,
		"kind":     clampKindName(c.Kind),
		"original": jsonSafeFloat(c.Original),
		"clamped":  c.Clamped,
	}
}

// jsonSafeFloat 把非有限 float64 转成可被 encoding/json 序列化的表示。
func jsonSafeFloat(v float64) any {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	default:
		return v
	}
}

// clampLogger 是可选的钳制观察钩子（billing-07 M-07-2）。
//
// 【为什么是可注入钩子而不是直接打日志】本文件是纯函数层，不持有 logger，
// 也不应把测试输出污染成 warn 流。平台装配时用 SetClampLogger 接入 slog，
// 即可让所有产生 QuotaClamp 的变体（含 *Strict）都留下后端日志。
var clampLogger atomic.Value // func(*contract.QuotaClamp)

// SetClampLogger 注册钳制观察钩子；nil 表示关闭。
func SetClampLogger(fn func(*contract.QuotaClamp)) {
	if fn == nil {
		clampLogger.Store(func(*contract.QuotaClamp) {})
		return
	}
	clampLogger.Store(fn)
}

func notifyClamp(c *contract.QuotaClamp) {
	if c == nil {
		return
	}
	if fn, ok := clampLogger.Load().(func(*contract.QuotaClamp)); ok && fn != nil {
		fn(c)
	}
}

// ---------------------------------------------------------------------------
// 饱和内核
// ---------------------------------------------------------------------------

// saturateFloat 是三个非钱包浮点族的公共内核。
//
// 判定顺序必须是 NaN → 上界 → 下界：NaN 与任何数比较都为 false，若先比较
// 界值会漏判并把它当作正常值转成 int（Go 中 NaN→int 是实现相关的）。
func saturateFloat(v float64, op string, max, min int64) (int64, *contract.QuotaClamp) {
	if math.IsNaN(v) {
		c := &contract.QuotaClamp{Op: op, Kind: contract.ClampNaN, Original: v, Clamped: 0}
		notifyClamp(c)
		return 0, c
	}
	if v > float64(max) {
		c := &contract.QuotaClamp{Op: op, Kind: contract.ClampOverflow, Original: v, Clamped: max}
		notifyClamp(c)
		return max, c
	}
	if v < float64(min) {
		c := &contract.QuotaClamp{Op: op, Kind: contract.ClampUnderflow, Original: v, Clamped: min}
		notifyClamp(c)
		return min, c
	}
	// 到这里 v 已落在 [min,max]（int32 或 JS 安全整数域）内，float64→int64
	// 截断是安全的。math.Trunc 显式表达「向零截断」而不是依赖转换的隐含语义。
	return int64(math.Trunc(v)), nil
}

// saturateDecimal 是十进制族内核：先 Round(0)（半离零，见 shopspring
// decimal.Round 实现），再按界饱和。decimal 没有 NaN/Inf，故无 ClampNaN 分支。
// Original 取 decimal→float64 的近似值（billing-07 §4.1 的乙类精度说明：
// 该字段只用于审计，不用于重算）。
func saturateDecimal(d decimal.Decimal, op string, max, min int64) (int64, *contract.QuotaClamp) {
	r := d.Round(0)
	if r.Cmp(decimal.NewFromInt(max)) > 0 {
		c := &contract.QuotaClamp{
			Op: op, Kind: contract.ClampOverflow,
			Original: d.InexactFloat64(), Clamped: max,
		}
		notifyClamp(c)
		return max, c
	}
	if r.Cmp(decimal.NewFromInt(min)) < 0 {
		c := &contract.QuotaClamp{
			Op: op, Kind: contract.ClampUnderflow,
			Original: d.InexactFloat64(), Clamped: min,
		}
		notifyClamp(c)
		return min, c
	}
	return r.IntPart(), nil
}

// strictResult 把 Checked 变体的标记转成 error；越界时按 billing-07 §2.1
// 返回 (0, clamp-as-error)，让同一类型既能审计又能 fail-fast。
func strictResult(quota int64, clamp *contract.QuotaClamp) (int64, error) {
	if clamp != nil {
		return 0, &ClampError{Clamp: clamp}
	}
	return quota, nil
}

// ---------------------------------------------------------------------------
// 截断族（向零截断）
// ---------------------------------------------------------------------------

// QuotaFromFloat 向零截断并静默饱和到单请求 int32 界。
// 仅用于「调用方能证明输入有界」的非计费或已校验路径。
func QuotaFromFloat(v float64) int64 {
	q, _ := saturateFloat(v, "QuotaFromFloat", contract.MaxQuota, contract.MinQuota)
	return q
}

// QuotaFromFloatChecked 返回实际饱和值与钳制标记；标记用于结算路径审计。
func QuotaFromFloatChecked(v float64) (int64, *contract.QuotaClamp) {
	return saturateFloat(v, "QuotaFromFloatChecked", contract.MaxQuota, contract.MinQuota)
}

// QuotaFromFloatStrict 把钳制转成 error，供预扣路径在写额度前阻断。
func QuotaFromFloatStrict(v float64) (int64, error) {
	q, c := saturateFloat(v, "QuotaFromFloatStrict", contract.MaxQuota, contract.MinQuota)
	return strictResult(q, c)
}

// ---------------------------------------------------------------------------
// 取整族（半离零：.5 远离零）
// ---------------------------------------------------------------------------

// QuotaRound 半离零取整后静默饱和（0.5→1、-0.5→-1、2.5→3）。
// tiered 结算的取整必须走本族（billing-00 §7.2）。
func QuotaRound(v float64) int64 {
	q, _ := saturateFloat(math.Round(v), "QuotaRound", contract.MaxQuota, contract.MinQuota)
	return q
}

// QuotaRoundChecked 半离零取整并返回钳制标记（先 round 再饱和）。
func QuotaRoundChecked(v float64) (int64, *contract.QuotaClamp) {
	return saturateFloat(math.Round(v), "QuotaRoundChecked", contract.MaxQuota, contract.MinQuota)
}

// QuotaRoundStrict 半离零取整；越界以 error 阻断。
func QuotaRoundStrict(v float64) (int64, error) {
	q, c := saturateFloat(math.Round(v), "QuotaRoundStrict", contract.MaxQuota, contract.MinQuota)
	return strictResult(q, c)
}

// ---------------------------------------------------------------------------
// 十进制族（先 Round(0) 再饱和）
// ---------------------------------------------------------------------------

// QuotaFromDecimal 对 decimal 先 Round(0) 再静默饱和。
// 金额链路一律走十进制族，避免 float64 乘积的舍入误差累积。
func QuotaFromDecimal(d decimal.Decimal) int64 {
	q, _ := saturateDecimal(d, "QuotaFromDecimal", contract.MaxQuota, contract.MinQuota)
	return q
}

// QuotaFromDecimalChecked 返回十进制换算结果与钳制标记（结算路径）。
func QuotaFromDecimalChecked(d decimal.Decimal) (int64, *contract.QuotaClamp) {
	return saturateDecimal(d, "QuotaFromDecimalChecked", contract.MaxQuota, contract.MinQuota)
}

// QuotaFromDecimalStrict 把十进制换算的钳制转成 error（预扣路径）。
func QuotaFromDecimalStrict(d decimal.Decimal) (int64, error) {
	q, c := saturateDecimal(d, "QuotaFromDecimalStrict", contract.MaxQuota, contract.MinQuota)
	return strictResult(q, c)
}

// ---------------------------------------------------------------------------
// 钱包族（JS 安全整数域 ±(2^53−1)）
// ---------------------------------------------------------------------------

// ValidateWalletQuota 校验「已经是整数」的钱包额度：只挡上界，负值放行
// （billing-07 §3 的乙类怪癖：负向调整合法）。
func ValidateWalletQuota(q int64) error {
	if q > contract.MaxWalletQuota {
		return fmt.Errorf("pricing: 钱包额度 %d 超出 JS 安全整数上界 %d", q, contract.MaxWalletQuota)
	}
	return nil
}

// WalletQuotaFromDecimalChecked 按 ±MaxWalletQuota 饱和并返回标记。
//
// 【存在理由】billing-07 M-07-1（契约 §12 缺口 G1）：钱包路径此前只有
// Strict，饱和只以 error 呈现、不产生可审计标记。本变体让钱包侧钳制也能
// 落 admin_info.quota_saturation。
func WalletQuotaFromDecimalChecked(d decimal.Decimal) (int64, *contract.QuotaClamp) {
	return saturateDecimal(d, "WalletQuotaFromDecimalChecked", contract.MaxWalletQuota, -contract.MaxWalletQuota)
}

// WalletQuotaFromDecimalStrict 按钱包界换算；越界以 error 阻断。
// op 必须与 Checked 不同（billing-00 §7.2 v1.1.1 冻结），否则审计无法
// 区分「阻断」与「仅审计」。
func WalletQuotaFromDecimalStrict(d decimal.Decimal) (int64, error) {
	q, c := saturateDecimal(d, "WalletQuotaFromDecimalStrict", contract.MaxWalletQuota, -contract.MaxWalletQuota)
	return strictResult(q, c)
}

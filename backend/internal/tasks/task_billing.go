package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/plugin"
)

// 本文件承载任务计价表达式在任务行的**提交期冻结**（T5 / P-08）。
//
// 为什么冻结而不是结算时重新解析：任务提交后 hold 无租约，结算发生在
// worker 进程；若结算期重新读配置，管理员在任务运行期间改价/删键会让同一
// 任务的金额与提交时承诺的价格不一致，且表达式求值失败会把已成功的任务
// 卡死在 settlement_pending。冻结快照让「提交时的价格」成为唯一事实，
// 与模型侧 TieredSnapshot（contract.PriceSnapshot）同构：mode + 表达式 +
// 表达式哈希 + 分组倍率。
//
// 快照只写 private_data（台账投影永不返回，设计 §7.1）。

// PrivateTieredSnapshot 是 tasks.private_data 里的任务侧定价快照键。
const PrivateTieredSnapshot = "tiered_snapshot"

// PrivateTaskExprSkip 记录「模型级 tiered 表达式被显式跳过」的原因
// （复核 I2）：没有它，`source:"held_skip"` 的降级就与「从未配置」不可区分。
const PrivateTaskExprSkip = "task_expr_skip"

// FreezeTieredSnapshot 把解析出的任务表达式冻结成 private_data 可序列化的快照。
//
// 返回 nil 表示没有任务表达式（调用方沿用「按预扣额结清」）。
//
// 【为什么冻结 units / QPU（复核 I1/M1）】结算发生在 worker 进程、可能在
// 插件升级/改名/下架之后。若结算期才从活 manifest 取「字段 → 单位」或读
// 当前 QuotaPerUnit：
//   - 插件改名/下架会让 u("旧字段") 突然「用量缺失」，每次结算报错、hold
//     无租约且永久重试，用户余额被钉死；
//   - QPU 热更会让提交与结算的金额口径不一致。
//
// 因此把该表达式引用的字段单位与 QPU 一并冻结。旧快照（无 units/qpu）在
// EvalTaskCost 里回落到 live manifest 与当前 QPU，保持兼容。
func FreezeTieredSnapshot(e *pricing.Expr, source pricing.ExprSource, groupRatio float64, m *plugin.Manifest) map[string]any {
	if e == nil || strings.TrimSpace(e.Source) == "" {
		return nil
	}
	units := map[string]any{}
	declared := usageFieldUnits(m)
	for _, key := range e.UsageKeys() {
		// 未声明的字段也冻结（空单位）：结算时仍能读到 data 里的值；
		// 若插件当时就没声明，buildTaskUsageFact 会按「无单位」处理。
		units[key] = declared[key]
	}
	sum := sha256.Sum256([]byte(e.Source))
	return map[string]any{
		"mode":           "tiered_expr",
		"expr":           e.Source,
		"expr_hash":      hex.EncodeToString(sum[:]),
		"group_ratio":    groupRatio,
		"quota_per_unit": pricing.CurrentQuotaPerUnit(),
		"units":          units,
		"source":         string(source),
	}
}

// EvalTaskCost 按冻结快照求值任务成本。
//
// 返回值：
//   - configured=false：任务没有冻结表达式（未配置）→ 调用方沿用既有的
//     「按预扣额结清」语义，保证存量任务不被改变；
//   - err != nil：快照存在但不可用（损坏/编译失败/用量缺失/负值）→ 调用方
//     **保留预扣**并置 settlement_pending（P-07），绝不退款；
//   - 否则 credits 是最终应扣积分（多退少补的「实扣」）。
func EvalTaskCost(snapshot any, data map[string]any, m *plugin.Manifest) (credits int64, configured bool, err error) {
	raw, ok := snapshot.(map[string]any)
	if !ok || len(raw) == 0 {
		return 0, false, nil
	}
	exprString := snapshotString(raw["expr"])
	if strings.TrimSpace(exprString) == "" {
		// M2：快照存在但表达式为空 = 损坏（本仓库不会写出这种快照）。
		// 静默按 held 结清会掩盖数据损坏并可能多收，必须 fail-closed。
		return 0, true, errors.New("tasks: frozen tiered snapshot has an empty expression")
	}
	expr, err := pricing.CompileTaskExpr(exprString)
	if err != nil {
		return 0, true, fmt.Errorf("tasks: compile frozen task expression: %w", err)
	}
	expr = expr.WithGroupRatio(snapshotFloat(raw["group_ratio"])).
		WithQuotaPerUnit(int64(snapshotFloat(raw["quota_per_unit"])))
	// I1：优先用快照冻结的字段单位；旧快照没有 units 时才回落 live manifest。
	units, hasUnits := snapshotStringMap(raw["units"])
	if !hasUnits {
		units = usageFieldUnits(m)
	}
	fact := buildTaskUsageFactWithUnits(data, units)
	credits, err = pricing.EvalTaskUsage(expr, fact)
	if err != nil {
		return 0, true, fmt.Errorf("tasks: evaluate frozen task expression: %w", err)
	}
	return credits, true, nil
}

// snapshotStringMap 读取快照里的 map[string]string（JSON round-trip 后
// 值仍是 string；元素类型是 any）。
func snapshotStringMap(v any) (map[string]string, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(obj))
	for key, value := range obj {
		if unit, ok := value.(string); ok {
			out[key] = unit
		}
	}
	return out, true
}

// snapshotString 读取快照里的字符串字段（JSON round-trip 后仍是 string）。
func snapshotString(v any) string {
	s, _ := v.(string)
	return s
}

// snapshotSource 读取冻结快照的来源级别（只用于日志/审计）。
func snapshotSource(snapshot any) string {
	raw, ok := snapshot.(map[string]any)
	if !ok {
		return ""
	}
	return snapshotString(raw["source"])
}

// snapshotFloat 读取快照里的数值字段；JSON round-trip 后可能是 float64。
func snapshotFloat(v any) float64 {
	switch typed := v.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	}
	return 0
}

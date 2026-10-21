package tasks

import (
	"crypto/sha256"
	"encoding/hex"
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

// FreezeTieredSnapshot 把解析出的任务表达式冻结成 private_data 可序列化的快照。
//
// 返回 nil 表示没有任务表达式（调用方沿用「按预扣额结清」）。
func FreezeTieredSnapshot(e *pricing.Expr, source pricing.ExprSource, groupRatio float64) map[string]any {
	if e == nil || strings.TrimSpace(e.Source) == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(e.Source))
	return map[string]any{
		"mode":        "tiered_expr",
		"expr":        e.Source,
		"expr_hash":   hex.EncodeToString(sum[:]),
		"group_ratio": groupRatio,
		"source":      string(source),
	}
}

// EvalTaskCost 按冻结快照求值任务成本。
//
// 返回值：
//   - configured=false：任务没有冻结表达式（未配置或被旧版本提交）→ 调用方
//     沿用既有的「按预扣额结清」语义，保证存量任务不被改变；
//   - err != nil：快照存在但不可用（编译失败/用量缺失/负值）→ 调用方**保留
//     预扣**并置 settlement_pending（P-07），绝不退款；
//   - 否则 credits 是最终应扣积分（多退少补的「实扣」）。
func EvalTaskCost(snapshot any, data map[string]any, m *plugin.Manifest) (credits int64, configured bool, err error) {
	raw, ok := snapshot.(map[string]any)
	if !ok || len(raw) == 0 {
		return 0, false, nil
	}
	exprString := snapshotString(raw["expr"])
	if strings.TrimSpace(exprString) == "" {
		return 0, false, nil
	}
	expr, err := pricing.CompileTaskExpr(exprString)
	if err != nil {
		return 0, true, fmt.Errorf("tasks: compile frozen task expression: %w", err)
	}
	fact := buildTaskUsageFact(data, m)
	credits, err = pricing.EvalTaskUsage(expr.WithGroupRatio(snapshotFloat(raw["group_ratio"])), fact)
	if err != nil {
		return 0, true, fmt.Errorf("tasks: evaluate frozen task expression: %w", err)
	}
	return credits, true, nil
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

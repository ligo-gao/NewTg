package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// TaskModelExprLookup 是任务表达式解析链第 4 级（映射模型表达式）的读取面。
//
// 为什么不复用 pricing.DBSource：Resolve 会做分组覆盖/默认价/内置表达式
// 的完整解析，并在装配期写入进程级 QuotaPerUnit；任务提交只需要「该模型
// 是否配置了 tiered_expr」一项事实。这里用一条窄查询，避免副作用与重复
// 解析（第 3 级直接用 PreConsume 已冻结的 rc.PriceSnapshot）。
//
// 判别口径与 pricing.Source 的 R9 一致：billing_expr 非 NULL（且非空白）
// 即表达式模式（显式模式最高优先）；否则 mode=1 → per_call、mode=0 → ratio。
func TaskModelExprLookup(db *sql.DB) func(ctx context.Context, model string) (mode, expr string, ok bool, err error) {
	return func(ctx context.Context, model string) (string, string, bool, error) {
		model = strings.TrimSpace(model)
		if db == nil || model == "" {
			return "", "", false, nil
		}
		var (
			rawMode int16
			rawExpr sql.NullString
		)
		err := db.QueryRowContext(ctx,
			`SELECT mode, billing_expr FROM model_pricing WHERE model = $1 AND status = 1`,
			model).Scan(&rawMode, &rawExpr)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		if err != nil {
			return "", "", false, fmt.Errorf("tasks: read model_pricing(%s): %w", model, err)
		}
		if rawExpr.Valid && strings.TrimSpace(rawExpr.String) != "" {
			return "tiered_expr", strings.TrimSpace(rawExpr.String), true, nil
		}
		if rawMode == 1 {
			return "per_call", "", true, nil
		}
		return "ratio", "", true, nil
	}
}

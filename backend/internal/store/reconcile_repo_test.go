package store

// 本文件是 InsertItems 占位符构造的**纯单元测试**（不连 PostgreSQL）。
//
// 集成测试需要真实数据库，在无 Postgres 的环境里跑不起来；而这里固化的正是
// 当初漏过去的错误：::date 被加到了第 4 个参数（upstream_model）而不是第 3 个
// （usage_date）。没有数据库也必须守住这两条不变式：
//   - 整行有且只有一个 ::date，且落在 usage_date 那一列；
//   - 一行占位符的个数等于 INSERT 列清单的列数。

import (
	"fmt"
	"strings"
	"testing"
)

// splitRow 把 "(...)" 拆成占位符切片，便于按列下标断言。
func splitRow(t *testing.T, row string) []string {
	t.Helper()
	if !strings.HasPrefix(row, "(") || !strings.HasSuffix(row, ")") {
		t.Fatalf("行模板 %q 不是 (…) 形式", row)
	}
	return strings.Split(strings.TrimSuffix(strings.TrimPrefix(row, "("), ")"), ",")
}

// TestItemInsertRowCastsUsageDateOnly 固定 ::date 的位置：第 3 个占位符
// （0 基下标 2，usage_date），绝不能是第 4 个（upstream_model）。
func TestItemInsertRowCastsUsageDateOnly(t *testing.T) {
	// base=0 是第一行；base=perRow 是第二行，用于确认偏移没有写死。
	bases := []int{0, len(reconcileItemInsertColumns), 7}
	for _, base := range bases {
		row := itemInsertRow(base)

		if got := strings.Count(row, "::date"); got != 1 {
			t.Fatalf("base=%d 行 %q 中 ::date 出现 %d 次，期望恰好 1 次", base, row, got)
		}

		fields := splitRow(t, row)
		if len(fields) < 4 {
			t.Fatalf("base=%d 行 %q 占位符不足 4 个", base, row)
		}

		wantDate := fmt.Sprintf("$%d::date", base+3)
		if fields[2] != wantDate {
			t.Fatalf("base=%d 第 3 个占位符（usage_date）= %q，期望 %q", base, fields[2], wantDate)
		}

		// 第 4 个是 upstream_model：必须是普通占位符，不能带类型转换。
		wantModel := fmt.Sprintf("$%d", base+4)
		if fields[3] != wantModel {
			t.Fatalf("base=%d 第 4 个占位符（upstream_model）= %q，期望 %q（不得带 ::date）",
				base, fields[3], wantModel)
		}
	}
}

// TestItemInsertRowPlaceholderCountMatchesColumns 固定「一行占位符数 == 列数」。
// 列清单是 perRow 的唯一来源；一旦行模板漏列/多列，这里立即失败。
func TestItemInsertRowPlaceholderCountMatchesColumns(t *testing.T) {
	// 前 4 列的顺序同时是参数顺序；usage_date 必须落在第 3 个位置（0 基下标 2）。
	wantFirst := []string{"run_id", "account_id", "usage_date", "upstream_model"}
	if len(reconcileItemInsertColumns) < len(wantFirst) {
		t.Fatalf("列清单只有 %d 列，不足前 4 列", len(reconcileItemInsertColumns))
	}
	for i, want := range wantFirst {
		if got := reconcileItemInsertColumns[i]; got != want {
			t.Fatalf("第 %d 列 = %q，期望 %q", i+1, got, want)
		}
	}

	seen := make(map[string]bool, len(reconcileItemInsertColumns))
	for _, col := range reconcileItemInsertColumns {
		if col == "" {
			t.Fatal("列清单包含空列名")
		}
		if seen[col] {
			t.Fatalf("列清单重复列名 %q", col)
		}
		seen[col] = true
	}

	for _, base := range []int{0, 13, len(reconcileItemInsertColumns) * 3} {
		fields := splitRow(t, itemInsertRow(base))
		if len(fields) != len(reconcileItemInsertColumns) {
			t.Fatalf("base=%d 占位符个数 = %d，列数 = %d，必须相等",
				base, len(fields), len(reconcileItemInsertColumns))
		}
		for i, field := range fields {
			want := fmt.Sprintf("$%d", base+i+1)
			if i == reconcileItemUsageDateIndex {
				want += "::date"
			}
			if field != want {
				t.Fatalf("base=%d 第 %d 个占位符 = %q，期望 %q", base, i+1, field, want)
			}
		}
	}
}

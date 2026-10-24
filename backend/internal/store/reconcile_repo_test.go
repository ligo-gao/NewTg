package store

// 本文件是 InsertItems 占位符构造的**纯单元测试**（不连 PostgreSQL）。
//
// 集成测试需要真实数据库，在无 Postgres 的环境里跑不起来；而这里固化的正是
// 当初漏过去的错误：::date 被加到了第 4 个参数（upstream_model）而不是第 3 个
// （usage_date）。没有数据库也必须守住这两条不变式：
//   - 整行有且只有一个 ::date，且落在 usage_date 那一列；
//   - 一行占位符的个数等于 INSERT 列清单的列数。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
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

// ---------------------------------------------------------------------------
// GetRun 的单元测试：用固定行的假驱动，不连 PostgreSQL。
//
// 目标是把两条映射语义钉死：
//   - 行存在时列顺序/UTC 归一/可空 finished_at 与 scanReconcileRun 一致；
//   - 行不存在时返回 ErrRunNotFound（HTTP 层据此写 404），而不是 sql.ErrNoRows。
// ---------------------------------------------------------------------------

// reconcileRunTestColumns 与 reconcileRunColumns 的列顺序逐位一致。
var reconcileRunTestColumns = []string{
	"id", "account_id", "window_start", "window_end", "triggered_by", "status",
	"token_tolerance_bp", "amount_tolerance_minor", "quota_per_unit", "timezone",
	"started_at", "finished_at", "error",
}

type reconcileRunDriver struct{}

func (reconcileRunDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("reconcileRunDriver: Open is unused")
}

// reconcileRunConnector 为每次连接返回同一行；row 为 nil 时返回空结果集。
type reconcileRunConnector struct{ row *ReconcileRun }

func (c reconcileRunConnector) Connect(context.Context) (driver.Conn, error) {
	return &reconcileRunConn{row: c.row}, nil
}

func (c reconcileRunConnector) Driver() driver.Driver { return reconcileRunDriver{} }

type reconcileRunConn struct{ row *ReconcileRun }

func (c *reconcileRunConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("reconcileRunConn: Prepare is unused")
}
func (c *reconcileRunConn) Close() error { return nil }
func (c *reconcileRunConn) Begin() (driver.Tx, error) {
	return nil, errors.New("reconcileRunConn: Begin is unused")
}

func (c *reconcileRunConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &reconcileRunRows{row: c.row}, nil
}

type reconcileRunRows struct {
	row  *ReconcileRun
	done bool
}

func (r *reconcileRunRows) Columns() []string { return reconcileRunTestColumns }
func (r *reconcileRunRows) Close() error      { return nil }

func (r *reconcileRunRows) Next(dest []driver.Value) error {
	if r.row == nil || r.done {
		return io.EOF
	}
	r.done = true
	run := *r.row
	dest[0] = run.ID
	dest[1] = run.AccountID
	dest[2] = run.WindowStart
	dest[3] = run.WindowEnd
	dest[4] = run.TriggeredBy
	dest[5] = run.Status
	dest[6] = int64(run.TokenToleranceBP)
	dest[7] = run.AmountToleranceMinor
	dest[8] = run.QuotaPerUnit
	dest[9] = run.Timezone
	dest[10] = run.StartedAt
	if run.FinishedAt != nil {
		dest[11] = *run.FinishedAt
	} else {
		dest[11] = nil
	}
	dest[12] = run.Error
	return nil
}

func TestReconcileRepoGetRun(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)
	finishedAt := time.Date(2026, 10, 24, 9, 31, 0, 0, time.UTC)
	want := ReconcileRun{
		ID: 42, AccountID: 7, WindowStart: start, WindowEnd: end,
		TriggeredBy: "manual", Status: "succeeded",
		TokenToleranceBP: 100, AmountToleranceMinor: 100, QuotaPerUnit: 100,
		Timezone: "Asia/Shanghai", StartedAt: startedAt, FinishedAt: &finishedAt,
	}

	db := sql.OpenDB(reconcileRunConnector{row: &want})
	t.Cleanup(func() { _ = db.Close() })
	got, err := NewReconcileRepo(db).GetRun(ctx, 42)
	if err != nil {
		t.Fatalf("GetRun(42) error = %v，期望 nil", err)
	}
	if got.ID != want.ID || got.AccountID != want.AccountID || got.Status != want.Status ||
		got.TriggeredBy != want.TriggeredBy || got.Timezone != want.Timezone ||
		got.TokenToleranceBP != want.TokenToleranceBP ||
		got.AmountToleranceMinor != want.AmountToleranceMinor ||
		got.QuotaPerUnit != want.QuotaPerUnit || got.Error != want.Error {
		t.Fatalf("GetRun 行字段 = %#v，期望 %#v", *got, want)
	}
	if !got.WindowStart.Equal(start) || !got.WindowEnd.Equal(end) || !got.StartedAt.Equal(startedAt) {
		t.Fatalf("GetRun 时间列 = %v/%v/%v，期望 %v/%v/%v",
			got.WindowStart, got.WindowEnd, got.StartedAt, start, end, startedAt)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) {
		t.Fatalf("GetRun finished_at = %v，期望 %v", got.FinishedAt, finishedAt)
	}

	// 仍在运行的 run：finished_at 为 NULL，必须保持 nil。
	running := want
	running.FinishedAt = nil
	running.Status = "running"
	dbRunning := sql.OpenDB(reconcileRunConnector{row: &running})
	t.Cleanup(func() { _ = dbRunning.Close() })
	got, err = NewReconcileRepo(dbRunning).GetRun(ctx, 42)
	if err != nil {
		t.Fatalf("GetRun(running) error = %v，期望 nil", err)
	}
	if got.FinishedAt != nil {
		t.Fatalf("GetRun running finished_at = %v，期望 nil", got.FinishedAt)
	}

	// 不存在：必须是 ErrRunNotFound（HTTP 层据此写 404）。
	dbMissing := sql.OpenDB(reconcileRunConnector{})
	t.Cleanup(func() { _ = dbMissing.Close() })
	if _, err := NewReconcileRepo(dbMissing).GetRun(ctx, 99); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("GetRun(99) error = %v，期望 ErrRunNotFound", err)
	}
}

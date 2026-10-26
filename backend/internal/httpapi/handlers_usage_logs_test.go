package httpapi

// Task 10 单元验收：账单字段（原价 / 折扣 / 折扣目标）进入列表项、详情
// 投影与导出。非集成用例只验证纯函数与结构契约，不触碰外部 PG/Redis；
// SQL 三键投影由 handlers_usage_logs_integration_test.go 的端到端用例覆盖。

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeUsageScanner 记录扫描目标，供 scanDetail 的列顺序契约使用。
type fakeUsageScanner struct {
	fill func(dest []any)
}

func (f fakeUsageScanner) Scan(dest ...any) error {
	if f.fill != nil {
		f.fill(dest)
	}
	return nil
}

func TestUsageLogListProjectsDiscountFields(t *testing.T) {
	row := usageLogRow{
		OriginalCost:    sql.NullInt64{Int64: 100, Valid: true},
		AppliedDiscount: sql.NullFloat64{Float64: 0.9, Valid: true},
		DiscountTarget:  sql.NullString{String: "user:7", Valid: true},
	}
	item := row.toItem(false, false)
	if item.OriginalCost == nil || *item.OriginalCost != 100 {
		t.Fatalf("original_cost = %v, want 100", item.OriginalCost)
	}
	if item.AppliedDiscount == nil || *item.AppliedDiscount != 0.9 {
		t.Fatalf("applied_discount = %v, want 0.9", item.AppliedDiscount)
	}
	if item.DiscountTarget == nil || *item.DiscountTarget != "user:7" {
		t.Fatalf("discount_target = %v, want user:7", item.DiscountTarget)
	}

	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	for _, key := range []string{"original_cost", "applied_discount", "discount_target"} {
		value, ok := fields[key]
		if !ok || string(value) == "null" {
			t.Fatalf("%s missing or null in JSON: %s", key, raw)
		}
	}
}

func TestUsageLogListNullsWhenAbsent(t *testing.T) {
	item := (&usageLogRow{}).toItem(false, false)
	if item.OriginalCost != nil || item.AppliedDiscount != nil || item.DiscountTarget != nil {
		t.Fatalf("absent keys must project nil, got %+v", item)
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	for _, key := range []string{"original_cost", "applied_discount", "discount_target"} {
		value, ok := fields[key]
		if !ok || string(value) != "null" {
			t.Fatalf("%s = %s, want explicit null", key, value)
		}
	}

	// applied_discount=0 是「全免」，不是「没打过折」：Valid=true 时必须是
	// 指针指向 0，绝不能与缺键合并成 nil（修正 4）。
	zero := (&usageLogRow{AppliedDiscount: sql.NullFloat64{Valid: true}}).toItem(false, false)
	if zero.AppliedDiscount == nil || *zero.AppliedDiscount != 0 {
		t.Fatalf("applied_discount=0 projected %v, want non-nil 0", zero.AppliedDiscount)
	}
}

func TestUsageLogDetailIncludesDiscountFields(t *testing.T) {
	// 详情 SQL 是 usageListColumns + ", other"：先扫三列、最后扫 other。
	// 用假扫描器逐位回填，列序错位时类型断言会直接失败而不是静默错位。
	const wantOther = `{"public_key":"visible"}`
	scanner := fakeUsageScanner{fill: func(dest []any) {
		*(dest[23].(*time.Time)) = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
		*(dest[24].(*sql.NullInt64)) = sql.NullInt64{Int64: 100, Valid: true}
		*(dest[25].(*sql.NullFloat64)) = sql.NullFloat64{Float64: 0.9, Valid: true}
		*(dest[26].(*sql.NullString)) = sql.NullString{String: "user:7", Valid: true}
		*(dest[27].(*[]byte)) = []byte(wantOther)
	}}
	var row usageLogRow
	if err := row.scanDetail(scanner); err != nil {
		t.Fatalf("scanDetail: %v", err)
	}
	if string(row.Other) != wantOther {
		t.Fatalf("other = %s, want %s (三列必须排在 other 之前)", row.Other, wantOther)
	}
	detail := usageLogDetail{
		usageLogItem: row.toItem(false, false),
		Other:        json.RawMessage(row.Other),
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	for _, key := range []string{"original_cost", "applied_discount", "discount_target"} {
		value, ok := fields[key]
		if !ok || string(value) == "null" {
			t.Fatalf("%s missing or null in detail JSON: %s", key, raw)
		}
	}
}

func TestExportAppendsDiscountColumns(t *testing.T) {
	want := []string{"original_cost", "applied_discount", "discount_target"}
	for _, admin := range []bool{false, true} {
		cols := exportColumns(admin)
		if len(cols) < len(want) {
			t.Fatalf("admin=%v columns = %v", admin, cols)
		}
		if got := cols[len(cols)-len(want):]; !slices.Equal(got, want) {
			t.Fatalf("admin=%v trailing columns = %v, want %v", admin, got, want)
		}
	}
}

// TestExportColumnsAlignWithRowValues 是位置对齐守卫：exportColumns 与
// exportRowValues 分处两地维护，任何一侧增删列都必须同步（修正 7）。
func TestExportColumnsAlignWithRowValues(t *testing.T) {
	row := &usageLogRow{CreatedAt: time.Unix(0, 0).UTC(), Status: usageStatusSuccess}
	for _, admin := range []bool{false, true} {
		cols := exportColumns(admin)
		values := exportRowValues(row, "someone", admin, true)
		if len(cols) != len(values) {
			t.Fatalf("admin=%v columns=%d values=%d: %v / %v", admin, len(cols), len(values), cols, values)
		}
	}
}

func TestExportLeavesDiscountNumbersUnescaped(t *testing.T) {
	row := &usageLogRow{
		CreatedAt:       time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
		Status:          usageStatusSuccess,
		Model:           "gpt-test",
		OriginalCost:    sql.NullInt64{Int64: 100, Valid: true},
		AppliedDiscount: sql.NullFloat64{Float64: 0.9, Valid: true},
		DiscountTarget:  sql.NullString{String: "=SUM(A1)", Valid: true},
	}
	cols := exportColumns(false)
	var txt strings.Builder
	if err := writeExportRow(&txt, "txt", cols, row, "", false, false); err != nil {
		t.Fatalf("writeExportRow: %v", err)
	}
	cells := strings.Split(strings.TrimSuffix(txt.String(), "\n"), "\t")
	if len(cells) != len(cols) {
		t.Fatalf("row cells = %d, columns = %d", len(cells), len(cols))
	}
	if got := cells[len(cells)-3]; got != "100" {
		t.Fatalf("original_cost cell = %q, want raw 100", got)
	}
	if got := cells[len(cells)-2]; got != "0.9" {
		t.Fatalf("applied_discount cell = %q, want raw 0.9", got)
	}
	// discount_target 是文本列：公式注入前缀必须被 exportCell 中和。
	if got := cells[len(cells)-1]; got != "'=SUM(A1)" {
		t.Fatalf("discount_target cell = %q, want escaped '=SUM(A1)", got)
	}

	// 无效数值列输出空串，而不是 0。
	row.AppliedDiscount = sql.NullFloat64{}
	txt.Reset()
	if err := writeExportRow(&txt, "txt", cols, row, "", false, false); err != nil {
		t.Fatalf("writeExportRow: %v", err)
	}
	cells = strings.Split(strings.TrimSuffix(txt.String(), "\n"), "\t")
	if got := cells[len(cells)-2]; got != "" {
		t.Fatalf("missing applied_discount cell = %q, want empty", got)
	}

	// Markdown 下 discount_target 仍是文本列：管道必须转义。
	row.DiscountTarget = sql.NullString{String: "a|b", Valid: true}
	var md strings.Builder
	if err := writeExportRow(&md, "markdown", cols, row, "", false, false); err != nil {
		t.Fatalf("writeExportRow(markdown): %v", err)
	}
	if !strings.Contains(md.String(), `a\|b`) {
		t.Fatalf("markdown discount_target not escaped: %s", md.String())
	}
}

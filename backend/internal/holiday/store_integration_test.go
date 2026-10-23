package holiday

// store.go 的数据库级验收（真实 PostgreSQL）。没有 DSN 时跳过，因此
// `go test ./internal/holiday/...` 在无数据库环境仍然全绿；
// 有数据库时（已执行到 20261027090000_model_pricing_strategy 迁移）运行：
//
//	NEWTG_TEST_DB_DSN='postgres://.../newtg_test?sslmode=disable' \
//	  go test ./internal/holiday/ -run TestHolidayStore -v
//
// 测试只用国家代码 ZZ + 2099 年：既不碰真实的 CN 日历，也让清理语句只
// 命中自己写入的行（不 DROP/TRUNCATE 共享库）。

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/pricingstrategy"
)

const (
	testCountry = "ZZ"
	testYear    = 2099
)

func openHolidayTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN / TEST_DATABASE_URL 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func cleanupHolidayCountry(t *testing.T, db *sql.DB, country string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM holiday_calendar WHERE country = $1`, country)
	})
}

func countHolidayRows(t *testing.T, db *sql.DB, country string, year int) int {
	t.Helper()
	start := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	end := time.Date(year+1, time.January, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM holiday_calendar
		WHERE country = $1 AND date >= $2::date AND date < $3::date`,
		country, start, end).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func TestHolidayStoreReplaceAndLookup(t *testing.T) {
	db := openHolidayTestDB(t)
	cleanupHolidayCountry(t, db, testCountry)
	store := NewStore(db)
	ctx := context.Background()

	first := []Day{
		{Date: "2099-10-01", Name: "国庆节", OffDay: true},
		{Date: "2099-10-02", Name: "国庆节", OffDay: true},
		{Date: "2099-10-10", Name: "国庆节", OffDay: false},
	}
	if err := store.ReplaceYear(ctx, testCountry, testYear, first, SourceName); err != nil {
		t.Fatalf("ReplaceYear: %v", err)
	}
	if got := countHolidayRows(t, db, testCountry, testYear); got != len(first) {
		t.Fatalf("行数 = %d，期望 %d", got, len(first))
	}

	// 法定假日：true + 名称。
	off := time.Date(testYear, 10, 1, 2, 0, 0, 0, mustShanghai(t))
	got, ok, err := store.LookupHoliday(ctx, testCountry, off)
	if err != nil {
		t.Fatalf("LookupHoliday(off): %v", err)
	}
	if !ok || !got.OffDay || got.Name != "国庆节" {
		t.Fatalf("法定假日 = %+v ok=%v，期望 国庆节/offDay", got, ok)
	}

	// 进程内缓存：绕过 Store 直接删行，TTL 内仍返回上一次的结果
	// （多副本下就是「最多晚 30s 看到同步结果」的预期行为）。
	second := time.Date(testYear, 10, 2, 2, 0, 0, 0, mustShanghai(t))
	if _, ok, err := store.LookupHoliday(ctx, testCountry, second); err != nil || !ok {
		t.Fatalf("预热 10-02 缓存 ok=%v err=%v", ok, err)
	}
	if _, err := db.Exec(`DELETE FROM holiday_calendar WHERE country = $1 AND date = $2::date`,
		testCountry, "2099-10-02"); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	if _, cachedOK, err := store.LookupHoliday(ctx, testCountry, second); err != nil || !cachedOK {
		t.Fatalf("TTL 内应命中缓存 ok=%v err=%v", cachedOK, err)
	}
	// 显式失效后回表，删除立即生效。
	store.Invalidate()
	if _, cachedOK, err := store.LookupHoliday(ctx, testCountry, second); err != nil || cachedOK {
		t.Fatalf("Invalidate 后应回表读到已删除 ok=%v err=%v", cachedOK, err)
	}

	// 调休上班日：false。
	makeup := time.Date(testYear, 10, 10, 9, 30, 0, 0, mustShanghai(t))
	got, ok, err = store.LookupHoliday(ctx, testCountry, makeup)
	if err != nil {
		t.Fatalf("LookupHoliday(makeup): %v", err)
	}
	if !ok || got.OffDay || got.Name != "国庆节" {
		t.Fatalf("调休上班 = %+v ok=%v，期望 国庆节/非 offDay", got, ok)
	}

	// 没有记录的日期：ok=false 且不是错误。
	if _, ok, err := store.LookupHoliday(ctx, testCountry, time.Date(testYear, 3, 1, 0, 0, 0, 0, mustShanghai(t))); err != nil || ok {
		t.Fatalf("缺失日期 ok=%v err=%v，期望 false/nil", ok, err)
	}

	// 日期按入参自己的 Location 取——调用方（pricingstrategy）传进来时
	// 已经本地化到策略时区。同一瞬间用 UTC 表示会落在前一天，这是刻意的。
	instant := time.Date(testYear, 9, 30, 18, 0, 0, 0, time.UTC) // = 10-01 02:00 +08
	if _, ok, _ := store.LookupHoliday(ctx, testCountry, instant); ok {
		t.Fatal("UTC 表达同一瞬间应解析成 09-30（无记录）")
	}
	if _, ok, _ := store.LookupHoliday(ctx, testCountry, instant.In(mustShanghai(t))); !ok {
		t.Fatal("本地化到 +08 后应命中 10-01")
	}
}

func TestHolidayStoreReplaceYearOnlyTouchesItsYear(t *testing.T) {
	db := openHolidayTestDB(t)
	cleanupHolidayCountry(t, db, testCountry)
	otherCountry := "ZY"
	cleanupHolidayCountry(t, db, otherCountry)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.ReplaceYear(ctx, testCountry, testYear, []Day{
		{Date: "2099-01-01", Name: "元旦", OffDay: true},
	}, SourceName); err != nil {
		t.Fatalf("ReplaceYear 2099: %v", err)
	}
	if err := store.ReplaceYear(ctx, testCountry, testYear+1, []Day{
		{Date: "2100-01-01", Name: "元旦", OffDay: true},
	}, SourceName); err != nil {
		t.Fatalf("ReplaceYear 2100: %v", err)
	}
	if err := store.ReplaceYear(ctx, otherCountry, testYear, []Day{
		{Date: "2099-05-01", Name: "劳动节", OffDay: true},
	}, SourceName); err != nil {
		t.Fatalf("ReplaceYear other country: %v", err)
	}

	// 再次替换 2099：旧行全消失、新行生效，2100 与其它国家不受影响。
	if err := store.ReplaceYear(ctx, testCountry, testYear, []Day{
		{Date: "2099-10-01", Name: "国庆节", OffDay: true},
	}, SourceName); err != nil {
		t.Fatalf("ReplaceYear second: %v", err)
	}
	if got := countHolidayRows(t, db, testCountry, testYear); got != 1 {
		t.Fatalf("2099 行数 = %d，期望 1（整年替换）", got)
	}
	if _, ok, _ := store.LookupHoliday(ctx, testCountry,
		time.Date(testYear, 1, 1, 12, 0, 0, 0, mustShanghai(t))); ok {
		t.Fatal("旧的 2099-01-01 应已被替换掉")
	}
	if got := countHolidayRows(t, db, testCountry, testYear+1); got != 1 {
		t.Fatalf("2100 行数 = %d，期望不受影响", got)
	}
	if got := countHolidayRows(t, db, otherCountry, testYear); got != 1 {
		t.Fatalf("其它国家行数 = %d，期望不受影响", got)
	}
}

func TestHolidayStoreReplaceYearRejectsInvalidInputWithoutWriting(t *testing.T) {
	db := openHolidayTestDB(t)
	cleanupHolidayCountry(t, db, testCountry)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.ReplaceYear(ctx, testCountry, testYear, []Day{
		{Date: "2099-10-01", Name: "国庆节", OffDay: true},
	}, SourceName); err != nil {
		t.Fatalf("ReplaceYear: %v", err)
	}

	cases := []struct {
		name  string
		year  int
		days  []Day
		error error
	}{
		{"empty", testYear, nil, ErrInvalidDays},
		{"wrong year", testYear, []Day{{Date: "2100-01-01", Name: "元旦", OffDay: true}}, ErrInvalidDays},
		{"bad date", testYear, []Day{{Date: "2099/10/01", Name: "国庆节", OffDay: true}}, ErrInvalidDays},
		{"duplicate", testYear, []Day{
			{Date: "2099-10-02", Name: "国庆节", OffDay: true},
			{Date: "2099-10-02", Name: "国庆节", OffDay: false},
		}, ErrInvalidDays},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.ReplaceYear(ctx, testCountry, tc.year, tc.days, SourceName)
			if !errors.Is(err, tc.error) {
				t.Fatalf("err = %v，期望 %v", err, tc.error)
			}
			if got := countHolidayRows(t, db, testCountry, testYear); got != 1 {
				t.Fatalf("非法输入后行数 = %d，期望旧数据原样保留（1）", got)
			}
		})
	}
}

func TestHolidayStoreRequiresDatabase(t *testing.T) {
	if err := NewStore(nil).ReplaceYear(context.Background(), testCountry, testYear,
		[]Day{{Date: "2099-01-01", Name: "元旦", OffDay: true}}, SourceName); err == nil {
		t.Fatal("nil DB 应返回错误")
	}
	if _, _, err := NewStore(nil).LookupHoliday(context.Background(), testCountry, time.Now()); err == nil {
		t.Fatal("nil DB 应返回错误")
	}
}

// mustShanghai 返回 +08:00 固定时区（中国无夏令时，与策略的 Asia/Shanghai
// 在当前年份等价，且不依赖容器 tzdata）。
func mustShanghai(t *testing.T) *time.Location {
	t.Helper()
	return time.FixedZone("Asia/Shanghai", 8*60*60)
}

// TestHolidayStoreLookupCache 不触数据库：锁定 30s TTL 与显式失效语义。
func TestHolidayStoreLookupCache(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	store := NewStore(nil)
	store.now = func() time.Time { return now }

	key := testCountry + "\x00" + "2026-10-01"
	store.cachePut(key, pricingstrategy.Holiday{Name: "国庆节", OffDay: true}, true)
	got, found, ok := store.cacheGet(key)
	if !ok || !found || !got.OffDay || got.Name != "国庆节" {
		t.Fatalf("缓存命中 = %+v found=%v ok=%v", got, found, ok)
	}

	// 负缓存同样有效：普通日期不必每次回表。
	missKey := testCountry + "\x00" + "2026-03-01"
	store.cachePut(missKey, pricingstrategy.Holiday{}, false)
	if _, found, ok := store.cacheGet(missKey); !ok || found {
		t.Fatalf("负缓存 ok=%v found=%v，期望 true/false", ok, found)
	}

	// 到达 TTL 即过期（不提前）。
	now = now.Add(LookupCacheTTL - time.Second)
	if _, _, ok := store.cacheGet(key); !ok {
		t.Fatal("TTL 未到时不应过期")
	}
	now = now.Add(time.Second)
	if _, _, ok := store.cacheGet(key); ok {
		t.Fatal("TTL 到达后应过期")
	}

	// 显式失效（同步写库后）立刻清空。
	store.cachePut(key, pricingstrategy.Holiday{Name: "国庆节", OffDay: true}, true)
	store.Invalidate()
	if _, _, ok := store.cacheGet(key); ok {
		t.Fatal("Invalidate 后不应再命中")
	}
}

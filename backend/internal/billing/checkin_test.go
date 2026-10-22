package billing

// W15-C 签到入账的单元与集成验收（modules-03 F-03-12 / modules-01 §3.19）。
//
// 不依赖 PG 的用例（随机区间、自然日、连签聚合）在普通 `go test` 下运行；
// 事务/并发/回滚用例需要真实 PostgreSQL，未设置 NEWTG_TEST_DB_DSN 时跳过
// （与 ledger_test.go 同一手法）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
)

// checkinTestClock 是可并发修改的时钟接缝：跨日与并发用例都使用固定时刻，
// 避免真实时间在测试中途跨零点造成偶发失败。
type checkinTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *checkinTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *checkinTestClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func newCheckinTestClock(now time.Time) *checkinTestClock {
	return &checkinTestClock{now: now}
}

// checkinTestConfig 构造签到三键的内存配置。
func checkinTestConfig(enabled bool, minQuota, maxQuota int64) ConfigReader {
	return mapConfig{
		ConfigCheckinEnabled:  json.RawMessage(fmt.Sprintf("%t", enabled)),
		ConfigCheckinMinQuota: json.RawMessage(fmt.Sprintf("%d", minQuota)),
		ConfigCheckinMaxQuota: json.RawMessage(fmt.Sprintf("%d", maxQuota)),
	}
}

func newCheckinTestService(t *testing.T, cfg *CheckinServiceConfig) *CheckinService {
	t.Helper()
	svc, err := NewCheckinService(*cfg)
	if err != nil {
		t.Fatalf("NewCheckinService: %v", err)
	}
	return svc
}

// ---------------------------------------------------------------------------
// 不依赖数据库的纯逻辑
// ---------------------------------------------------------------------------

func TestDrawCheckinAwardClosedInterval(t *testing.T) {
	// min = max：定额，且不得调用随机接缝。
	calls := 0
	award, err := drawCheckinAward(42, 42, func(int64) (int64, error) {
		calls++
		return 0, nil
	})
	if err != nil || award != 42 || calls != 0 {
		t.Fatalf("min=max: award=%d calls=%d err=%v", award, calls, err)
	}

	// 闭区间两端可达：rand[0, max-min] → min + offset。
	cases := []struct {
		offset int64
		want   int64
	}{{0, 10}, {10, 20}}
	for _, tc := range cases {
		var gotSpan int64
		award, err := drawCheckinAward(10, 20, func(span int64) (int64, error) {
			gotSpan = span
			return tc.offset, nil
		})
		if err != nil {
			t.Fatalf("offset=%d: %v", tc.offset, err)
		}
		if gotSpan != 11 {
			t.Fatalf("offset=%d: span=%d, want 11（闭区间）", tc.offset, gotSpan)
		}
		if award != tc.want {
			t.Fatalf("offset=%d: award=%d, want %d", tc.offset, award, tc.want)
		}
	}

	// min=0：停发，draw 返回 0。
	if award, err := drawCheckinAward(0, 0, nil); err != nil || award != 0 {
		t.Fatalf("min=max=0: award=%d err=%v", award, err)
	}

	// 越界随机值必须报错，而不是写出区间外的金额。
	if _, err := drawCheckinAward(10, 20, func(int64) (int64, error) { return 11, nil }); err == nil {
		t.Fatal("offset == span 必须被拒绝")
	}
}

func TestSummarizeCheckinsTimezoneBoundary(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("Asia/Shanghai 不可用")
	}
	// 23:59 本地 = 15:59Z；00:01 本地 = 次日 16:01Z。
	first := time.Date(2026, 10, 22, 15, 59, 0, 0, time.UTC)
	second := time.Date(2026, 10, 22, 16, 1, 0, 0, time.UTC)
	if got := first.In(loc).Format(checkinDateLayout); got != "2026-10-22" {
		t.Fatalf("first local date = %s", got)
	}
	if got := second.In(loc).Format(checkinDateLayout); got != "2026-10-23" {
		t.Fatalf("second local date = %s", got)
	}

	rows := []checkinRow{
		{Date: "2026-10-21", Awarded: 5},
		{Date: "2026-10-22", Awarded: 7},
		{Date: "2026-10-23", Awarded: 9},
	}
	streak, total, checked := summarizeCheckins(rows, "2026-10-23", loc)
	if streak != 3 || total != 21 || !checked {
		t.Fatalf("streak=%d total=%d checked=%v", streak, total, checked)
	}
	// 今天没签时从昨天起算（保留「昨天连签」的连续性）。
	streak, _, checked = summarizeCheckins(rows, "2026-10-24", loc)
	if streak != 3 || checked {
		t.Fatalf("今天未签: streak=%d checked=%v, want 3/false（从昨天起算）", streak, checked)
	}
	// 昨天也断签则连续天数为 0。
	streak, _, checked = summarizeCheckins(rows, "2026-10-25", loc)
	if streak != 0 || checked {
		t.Fatalf("连续两天未签: streak=%d checked=%v, want 0/false", streak, checked)
	}
	streak, _, _ = summarizeCheckins(rows[:2], "2026-10-23", loc)
	if streak != 2 {
		t.Fatalf("只签到 10-21/10-22、今天未签时 streak=%d, want 2（从昨天起算）", streak)
	}
}

// ---------------------------------------------------------------------------
// 真实 PostgreSQL：事务、并发与回滚
// ---------------------------------------------------------------------------

func TestCheckinFirstWritesRowAndLedgerInOneTransaction(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 1000)
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	loc, _ := time.LoadLocation("Asia/Shanghai")
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 10, 10), Now: clock.Now,
		Location: func() *time.Location { return loc },
	})

	res, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Awarded != 10 || res.BalanceAfter != 1010 {
		t.Fatalf("awarded=%d balance=%d", res.Awarded, res.BalanceAfter)
	}
	if res.Date != "2026-10-22" {
		t.Fatalf("date=%s, want 2026-10-22（站点时区）", res.Date)
	}
	if res.AlreadyCheckedIn || res.Streak != 1 || res.MonthTotal != 10 || res.TotalEarned != 10 {
		t.Fatalf("result=%+v", res)
	}

	var checkinID, awarded int64
	if err := sqlDB.QueryRow(
		`SELECT id, quota_awarded FROM checkins WHERE user_id = $1`, userID).
		Scan(&checkinID, &awarded); err != nil {
		t.Fatalf("read checkin: %v", err)
	}
	if awarded != 10 {
		t.Fatalf("quota_awarded=%d", awarded)
	}
	var count int64
	var amount int64
	var refID string
	if err := sqlDB.QueryRow(
		`SELECT count(*), COALESCE(max(amount),0), COALESCE(max(reference_id),'')
		   FROM wallet_transactions
		  WHERE user_id = $1 AND type = 'checkin_bonus' AND reference_type = 'checkin'`, userID).
		Scan(&count, &amount, &refID); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if count != 1 || amount != 10 || refID != fmt.Sprintf("%d", checkinID) {
		t.Fatalf("ledger count=%d amount=%d ref=%s, checkinID=%d", count, amount, refID, checkinID)
	}
}

func TestCheckinReplayReturnsStoredAmountWithoutSecondCredit(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 0)
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 5, 5), Now: clock.Now,
		Location: func() *time.Location { return time.UTC },
	})

	first, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("first CheckIn: %v", err)
	}
	second, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("second CheckIn: %v", err)
	}
	if !second.AlreadyCheckedIn {
		t.Fatal("同日第二次必须标记 AlreadyCheckedIn")
	}
	// 随机值随签到行落库：重放返回同一金额（不是重新抽取）。
	if second.Awarded != first.Awarded || second.BalanceAfter != first.BalanceAfter {
		t.Fatalf("replay awarded=%d balance=%d, first=%d/%d",
			second.Awarded, second.BalanceAfter, first.Awarded, first.BalanceAfter)
	}
	var checkins, ledger int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&checkins); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1 AND type = 'checkin_bonus'`, userID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	var balance int64
	if err := sqlDB.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if checkins != 1 || ledger != 1 || balance != 5 {
		t.Fatalf("checkins=%d ledger=%d balance=%d, want 1/1/5", checkins, ledger, balance)
	}
}

func TestCheckinCrossDayAndTimezoneBoundary(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 0)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("Asia/Shanghai 不可用")
	}
	// 23:59 本地（15:59Z）签一次；两分钟后 00:01 本地（16:01Z）可再签。
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 15, 59, 0, 0, time.UTC))
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 5, 5), Now: clock.Now,
		Location: func() *time.Location { return loc },
	})

	first, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Date != "2026-10-22" {
		t.Fatalf("first date=%s, want 2026-10-22", first.Date)
	}
	clock.Set(time.Date(2026, 10, 22, 16, 1, 0, 0, time.UTC))
	second, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.AlreadyCheckedIn {
		t.Fatal("跨过站点时区零点后必须可以再签")
	}
	if second.Date != "2026-10-23" {
		t.Fatalf("second date=%s, want 2026-10-23", second.Date)
	}
	if second.Awarded != 5 || second.BalanceAfter != 10 {
		t.Fatalf("second awarded=%d balance=%d", second.Awarded, second.BalanceAfter)
	}
	if second.Streak != 2 || second.MonthTotal != 10 || second.TotalEarned != 10 {
		t.Fatalf("second stats=%+v", second)
	}

	cal, err := svc.Calendar(context.Background(), userID, "2026-10")
	if err != nil {
		t.Fatalf("Calendar: %v", err)
	}
	if cal.Month != "2026-10" || len(cal.Days) != 2 || cal.Streak != 2 ||
		cal.MonthTotal != 10 || cal.TotalEarned != 10 || !cal.CheckedInToday {
		t.Fatalf("calendar=%+v", cal)
	}
	if cal.Days[0].Date != "2026-10-22" || cal.Days[1].Date != "2026-10-23" {
		t.Fatalf("calendar days=%+v", cal.Days)
	}
}

func TestCheckinConcurrentRequestsCreditExactlyOnce(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 0)
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 10, 10), Now: clock.Now,
		Location: func() *time.Location { return time.UTC },
	})

	const workers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		wins     int
		replays  int
		failures []error
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := svc.CheckIn(context.Background(), userID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			if res.AlreadyCheckedIn {
				replays++
			} else {
				wins++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) != 0 {
		t.Fatalf("并发签到出现错误: %v", failures)
	}
	if wins != 1 || replays != workers-1 {
		t.Fatalf("wins=%d replays=%d, want 1/%d（唯一约束兜底）", wins, replays, workers-1)
	}
	var checkins, ledger int64
	var balance int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&checkins); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1 AND type = 'checkin_bonus'`, userID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if checkins != 1 || ledger != 1 || balance != 10 {
		t.Fatalf("checkins=%d ledger=%d balance=%d, want 1/1/10", checkins, ledger, balance)
	}
}

func TestCheckinWalletLimitRollsBackCheckinRow(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	// 余额已在上界：奖励入账必须失败，且签到行随事务一起回滚。
	userID := createBillingTestUser(t, sqlDB, contract.MaxWalletQuota)
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 10, 10), Now: clock.Now,
		Location: func() *time.Location { return time.UTC },
	})

	_, err := svc.CheckIn(context.Background(), userID)
	if !errors.Is(err, ErrWalletLimit) {
		t.Fatalf("err=%v, want ErrWalletLimit", err)
	}
	var checkins, ledger int64
	var balance int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&checkins); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, userID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if checkins != 0 || ledger != 0 || balance != contract.MaxWalletQuota {
		t.Fatalf("回滚不完整: checkins=%d ledger=%d balance=%d", checkins, ledger, balance)
	}
}

func TestCheckinMinZeroRecordsWithoutLedger(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 1000)
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 0, 0), Now: clock.Now,
		Location: func() *time.Location { return time.UTC },
	})

	res, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Awarded != 0 || res.BalanceAfter != 1000 || res.AlreadyCheckedIn {
		t.Fatalf("result=%+v", res)
	}
	var checkins, ledger int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&checkins); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, userID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if checkins != 1 || ledger != 0 {
		t.Fatalf("min=0: checkins=%d ledger=%d, want 1/0", checkins, ledger)
	}
}

func TestCheckinDisabledHasNoSideEffects(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 1000)
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(false, 10, 10), Now: time.Now,
		Location: func() *time.Location { return time.UTC },
	})

	if _, err := svc.CheckIn(context.Background(), userID); !errors.Is(err, ErrCheckinDisabled) {
		t.Fatalf("err=%v, want ErrCheckinDisabled", err)
	}
	var checkins, ledger int64
	var balance int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&checkins); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, userID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if checkins != 0 || ledger != 0 || balance != 1000 {
		t.Fatalf("关闭态产生副作用: checkins=%d ledger=%d balance=%d", checkins, ledger, balance)
	}
}

// TestCheckinGuardUsesCheckinDateNotCreatedAt 覆盖 W15-C 复核 Finding A：
// 守卫必须只比较 checkin_date，不能比较 created_at（DB 时钟）。
//
// 构造一条 checkin_date=2026-10-22、但 created_at=2030 的「未来写入」行
// （模拟 app/DB 时钟偏差）。应用时钟已到 2026-10-23（Asia/Shanghai 次日
// 00:01）时，合法的次日签到必须成功；旧 created_at 守卫会因 2030 >= 本地
// 零点而误判为重放。
func TestCheckinGuardUsesCheckinDateNotCreatedAt(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 0)
	if _, err := sqlDB.Exec(
		`INSERT INTO checkins (user_id, checkin_date, quota_awarded, created_at)
		 VALUES ($1, DATE '2026-10-22', 5, TIMESTAMPTZ '2030-01-01T00:00:00Z')`, userID); err != nil {
		t.Fatalf("seed future created_at row: %v", err)
	}

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("Asia/Shanghai 不可用")
	}
	// 2026-10-22T16:01Z = 2026-10-23 00:01 Asia/Shanghai。
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 16, 1, 0, 0, time.UTC))
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 5, 5), Now: clock.Now,
		Location: func() *time.Location { return loc },
	})

	res, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("合法次日签到被 rejected: %v", err)
	}
	if res.AlreadyCheckedIn || res.Date != "2026-10-23" || res.Awarded != 5 {
		t.Fatalf("result=%+v, want 2026-10-23/5 非重放", res)
	}
	var rows, ledger int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&rows); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1 AND type = 'checkin_bonus'`, userID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if rows != 2 || ledger != 1 {
		t.Fatalf("rows=%d ledger=%d, want 2/1（新签到成功且只入账一次）", rows, ledger)
	}
}

// TestCheckinOuterFailureAfterLedgerRollsBackLedger 覆盖 Finding D：
// 账本成功写入后，外层事务再失败时必须连同已写入的流水一起回滚。
func TestCheckinOuterFailureAfterLedgerRollsBackLedger(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 1000)
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 10, 10), Now: clock.Now,
		Location: func() *time.Location { return time.UTC },
	})
	svc.afterLedger = func() error { return errors.New("injected failure after ledger") }

	if _, err := svc.CheckIn(context.Background(), userID); err == nil {
		t.Fatal("注入的外层失败必须向上返回")
	}
	var checkins, ledger int64
	var balance int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&checkins); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, userID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if checkins != 0 || ledger != 0 || balance != 1000 {
		t.Fatalf("外层失败后残留: checkins=%d ledger=%d balance=%d", checkins, ledger, balance)
	}
}

// TestCheckinReplayDoesNotDrawRandom 覆盖 Finding F：重放请求在守卫/插入
// 之后就不再抽取随机额；随机源故障不应把「已签到」变成 500。
func TestCheckinReplayDoesNotDrawRandom(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 0)
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	drawCalls := 0
	draw := func(int64) (int64, error) {
		drawCalls++
		return 0, nil
	}
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 5, 10), Now: clock.Now,
		Location:   func() *time.Location { return time.UTC },
		DrawInt64n: func(n int64) (int64, error) { return draw(n) },
	})

	first, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if drawCalls != 1 {
		t.Fatalf("first draw calls=%d, want 1", drawCalls)
	}
	draw = func(int64) (int64, error) { return 0, errors.New("random source down") }
	second, err := svc.CheckIn(context.Background(), userID)
	if err != nil {
		t.Fatalf("replay 不应依赖随机源: %v", err)
	}
	if !second.AlreadyCheckedIn || second.Awarded != first.Awarded {
		t.Fatalf("replay=%+v, first=%+v", second, first)
	}
	if drawCalls != 1 {
		t.Fatalf("replay 后 draw calls=%d, want 1（重放不得抽额）", drawCalls)
	}
}

// TestCheckinMissingUserMinZeroMapsUserNotFound 覆盖 Finding E：
// min=0 的余额快照查询遇到已删除用户时返回 ErrUserNotFound（handler → 401），
// 不是 500；且事务回滚不留下签到行。
func TestCheckinMissingUserMinZeroMapsUserNotFound(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 0)
	if _, err := sqlDB.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	clock := newCheckinTestClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	svc := newCheckinTestService(t, &CheckinServiceConfig{
		DB: gormDB, Config: checkinTestConfig(true, 0, 0), Now: clock.Now,
		Location: func() *time.Location { return time.UTC },
	})

	if _, err := svc.CheckIn(context.Background(), userID); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err=%v, want ErrUserNotFound", err)
	}
	var checkins int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&checkins); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if checkins != 0 {
		t.Fatalf("已删除用户留下签到行 %d 条", checkins)
	}
}

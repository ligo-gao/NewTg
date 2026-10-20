//go:build integration

package rankings

// D6a 排行榜数据层的数据库级验收：真 PG 主库 + 日志库 + Redis。
//
// 覆盖单测无法证明的真相：
//   - usage_logs 的聚合口径（只算 status=1；prompt+completion）；
//   - 站点时区窗口边界（Asia/Shanghai 的当日 00:00 换算为 UTC）；
//   - 阈值过滤、并列名次与跳号、双窗口环比；
//   - 单事务快照 + 实体槽位稳定 + timeseries 幂等不双计；
//   - 保留 10 个 run 的清理与缓存失效/广播；
//   - nav.rankings.enabled=false 与 Redis 租约的跳过路径。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d6?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d6_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	  go test -tags=integration ./internal/rankings/ -count=1 -v
//
// 每个用例只使用带本次运行唯一前缀的模型/作者名，并在结束时精确删除自己的行；
// 不 DROP/TRUNCATE（共享隔离库上可能有并行包）。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/store"
)

const testSiteTimezone = "Asia/Shanghai"

// testNow 固定为 2026-10-20 12:00 +08（UTC 04:00），落在已预建的 202610 分区内。
// 固定时刻让「today 窗口 = 站点时区当日 00:00 → now」的断言可复现。
func testNow(t *testing.T) (time.Time, *time.Location) {
	t.Helper()
	return testNowAt(t, 2026, 10, 20)
}

func testNowAt(t *testing.T, year int, month time.Month, day int) (time.Time, *time.Location) {
	t.Helper()
	loc, err := time.LoadLocation(testSiteTimezone)
	if err != nil {
		t.Fatalf("load %s: %v", testSiteTimezone, err)
	}
	return time.Date(year, month, day, 12, 0, 0, 0, loc), loc
}

func openRankingsDB(t *testing.T, env string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("%s 未设置，跳过集成测试", env)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", env, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openRankingsRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis 不可用: %v", err)
	}
	return client
}

// uniquePrefix 生成本次测试独有的模型/作者前缀（合法字符集 [A-Za-z0-9._:/-]）。
func uniquePrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("d6a-%d", time.Now().UnixNano())
}

func insertUsageRows(t *testing.T, db *sql.DB, model string, count int,
	prompt, completion int, status int16, at time.Time) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin usage insert: %v", err)
	}
	for i := 0; i < count; i++ {
		if _, err := tx.Exec(`
			INSERT INTO usage_logs
			    (request_id, user_id, model, prompt_tokens, completion_tokens, status, created_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`,
			uuid.NewString(), int64(1), model, prompt, completion, status, at); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert usage log: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit usage insert: %v", err)
	}
}

// cleanupRankingsFixture 精确删除本次测试的数据（按前缀与窗口）。
func cleanupRankingsFixture(t *testing.T, mainDB, logsDB *sql.DB, prefix string, now time.Time) {
	t.Helper()
	window := CurrentWindow(PeriodToday, now, now.Location())
	// 先删 run（子表级联），再删跨 run 的 timeseries/entities 与目录行。
	if _, err := mainDB.Exec(`DELETE FROM rankings_snapshot_runs WHERE period = $1 AND window_start = $2`,
		string(PeriodToday), window.Start); err != nil {
		t.Fatalf("cleanup runs: %v", err)
	}
	if _, err := mainDB.Exec(`DELETE FROM rankings_timeseries WHERE model LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("cleanup timeseries: %v", err)
	}
	if _, err := mainDB.Exec(`DELETE FROM rankings_author_timeseries WHERE author LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("cleanup author timeseries: %v", err)
	}
	if _, err := mainDB.Exec(`DELETE FROM rankings_entities WHERE entity LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("cleanup entities: %v", err)
	}
	if _, err := mainDB.Exec(`DELETE FROM model_meta WHERE model_name LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("cleanup model_meta: %v", err)
	}
	if _, err := mainDB.Exec(`DELETE FROM model_pricing WHERE model LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("cleanup model_pricing: %v", err)
	}
	if _, err := logsDB.Exec(`DELETE FROM usage_logs WHERE model LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("cleanup usage_logs: %v", err)
	}
}

func testConfig(enabled bool, minSamples int) mapConfig {
	return mapConfig{
		configKeyTimezone:   fmt.Sprintf("%q", testSiteTimezone),
		configKeyMinSamples: fmt.Sprintf("%d", minSamples),
		configKeyEnabled:    fmt.Sprintf("%t", enabled),
	}
}

func rankingsDeps(mainDB, logsDB *sql.DB, rdb *redis.Client, cfg ConfigReader, now time.Time) Deps {
	return Deps{
		MainDB: mainDB,
		LogsDB: logsDB,
		Redis:  rdb,
		Config: cfg,
		Logger: quietLogger(),
		Now:    func() time.Time { return now },
	}
}

func countRuns(t *testing.T, db *sql.DB, period Period, windowStart time.Time) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM rankings_snapshot_runs WHERE period = $1 AND window_start = $2`,
		string(period), windowStart).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

// TestRankingsRefreshEndToEnd 覆盖主链路：阈值、双窗口环比、作者份额、
// timeseries 幂等、槽位稳定、缓存失效与广播。
func TestRankingsRefreshEndToEnd(t *testing.T) {
	mainDB := openRankingsDB(t, "NEWTG_TEST_DB_DSN")
	logsDB := openRankingsDB(t, "NEWTG_TEST_LOGS_DSN")
	rdb := openRankingsRedis(t)
	now, _ := testNow(t)
	prefix := uniquePrefix(t)
	defer cleanupRankingsFixture(t, mainDB, logsDB, prefix, now)
	// year 用例的 run 也要清理（cleanupRankingsFixture 只清 today 窗口）。
	yearWindow := CurrentWindow(PeriodYear, now, now.Location())
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM rankings_snapshot_runs WHERE period = $1 AND window_start = $2`,
			string(PeriodYear), yearWindow.Start)
	})
	// Redis 缓存/锁也必须清，避免上一轮残留影响。
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), LockKey(PeriodToday), CacheKey(PeriodToday)).Err()
	})

	modelA := prefix + "-model-a"
	modelB := prefix + "-model-b"
	modelC := prefix + "-model-c" // 请求数 49，低于阈值
	modelE := prefix + "-model-e" // 全部 status=2（error），不得计入
	modelF := prefix + "-model-f" // 站点时区前一日 23:59，不属于 today
	authorA := prefix + "-OpenAI"
	authorB := prefix + "-Anthropic"

	for _, stmt := range []struct {
		model, author string
	}{
		{modelA, authorA}, {modelB, authorB}, {modelC, authorA}, {modelE, authorA}, {modelF, authorA},
	} {
		if _, err := mainDB.Exec(`INSERT INTO model_pricing (model, author) VALUES ($1, $2)`,
			stmt.model, stmt.author); err != nil {
			t.Fatalf("insert model_pricing: %v", err)
		}
	}
	if _, err := mainDB.Exec(`INSERT INTO model_meta (model_name, display_name) VALUES ($1, $2)`,
		modelA, "Model A Display"); err != nil {
		t.Fatalf("insert model_meta: %v", err)
	}

	// 窗口：today = 2026-10-19T16:00Z .. 2026-10-20T04:00Z
	// 上一窗口（等长）= 2026-10-19T08:00Z .. 16:00Z
	prevAt := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	curAt := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)  // 10:00 +08
	lateAt := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC) // 11:00 +08
	beforeStart := time.Date(2026, 10, 19, 15, 59, 0, 0, time.UTC)

	// 上一窗口：A 50×10=500；B 50×30=1500。
	insertUsageRows(t, logsDB, modelA, 50, 10, 0, 1, prevAt)
	insertUsageRows(t, logsDB, modelB, 50, 30, 0, 1, prevAt)
	// 当前窗口：A 50×15=750；B 50×20=1000；C 49×100（低于阈值）；
	// E 50×1000 状态 2；F 50×100 在站点时区前一日 23:59。
	insertUsageRows(t, logsDB, modelA, 50, 10, 5, 1, curAt)
	insertUsageRows(t, logsDB, modelB, 50, 15, 5, 1, curAt)
	insertUsageRows(t, logsDB, modelC, 49, 100, 0, 1, curAt)
	insertUsageRows(t, logsDB, modelE, 50, 1000, 0, 2, curAt)
	insertUsageRows(t, logsDB, modelF, 50, 100, 0, 1, beforeStart)

	deps := rankingsDeps(mainDB, logsDB, rdb, testConfig(true, 50), now)
	ctx := context.Background()

	// 缓存失效 + PUBLISH：先放一个旧缓存并订阅刷新频道。
	if err := rdb.Set(ctx, CacheKey(PeriodToday), "stale", time.Minute).Err(); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	sub := rdb.Subscribe(ctx, RefreshedChannel)
	defer func() { _ = sub.Close() }()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe refreshed channel: %v", err)
	}

	refreshPeriod(ctx, deps, PeriodToday)

	if n, err := rdb.Exists(ctx, CacheKey(PeriodToday)).Result(); err != nil || n != 0 {
		t.Fatalf("刷新后缓存键应被删除，exists=%d err=%v", n, err)
	}
	select {
	case msg := <-sub.Channel():
		if msg.Payload == "" {
			t.Fatal("刷新广播 payload 为空")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 newtg:rankings.refreshed 广播")
	}

	repo := store.NewRankingsRepo(mainDB)
	snap, ok, err := repo.LatestRun(ctx, string(PeriodToday))
	if err != nil || !ok {
		t.Fatalf("LatestRun: ok=%v err=%v", ok, err)
	}
	if snap.Run.Status != statusOK || snap.Run.ModelCount != 2 {
		t.Fatalf("run status/model_count = %s/%d，want ok/2", snap.Run.Status, snap.Run.ModelCount)
	}
	if snap.Run.SampleThreshold != 50 {
		t.Fatalf("sample_threshold = %d，want 50", snap.Run.SampleThreshold)
	}
	if len(snap.Models) != 2 {
		t.Fatalf("模型行数 = %d，want 2（C 低于阈值、E 非 success、F 不在窗口）", len(snap.Models))
	}
	if snap.Models[0].Model != modelB || snap.Models[1].Model != modelA {
		t.Fatalf("模型顺序 = %s,%s，want B,A", snap.Models[0].Model, snap.Models[1].Model)
	}
	b, a := snap.Models[0], snap.Models[1]
	// F 的 50 行落在上一窗口且过阈值（5000 token），因此上一窗口名次是
	// F=1、B=2、A=3；F 不在当前窗口，不影响榜单。
	if *b.Rank != 1 || *a.Rank != 2 || *a.PrevRank != 3 || *b.PrevRank != 2 {
		t.Fatalf("名次/上期名次 = b%d a%d a_prev%d b_prev%d", *b.Rank, *a.Rank, *a.PrevRank, *b.PrevRank)
	}
	if *a.DeltaTokens != 250 || *a.DeltaPct != 50 {
		t.Fatalf("A 环比 = %d/%v，want 250/50", *a.DeltaTokens, *a.DeltaPct)
	}
	if *b.DeltaTokens != -500 || *b.DeltaPct != -33.33 {
		t.Fatalf("B 环比 = %d/%v，want -500/-33.33", *b.DeltaTokens, *b.DeltaPct)
	}
	if a.DisplayName == nil || *a.DisplayName != "Model A Display" {
		t.Fatalf("A 展示名 = %v", a.DisplayName)
	}
	if a.Author == nil || *a.Author != authorA || b.Author == nil || *b.Author != authorB {
		t.Fatalf("作者补齐错误: a=%v b=%v", a.Author, b.Author)
	}

	movers := ComputeMovers(snap.Models)
	if len(movers.Up) != 1 || movers.Up[0].Model != modelA {
		t.Fatalf("上升榜 = %+v，want A", movers.Up)
	}
	if len(movers.Down) != 1 || movers.Down[0].Model != modelB {
		t.Fatalf("下降榜 = %+v，want B", movers.Down)
	}

	// 份额合计 100，且按 token 降序。
	var shareSum float64
	for _, p := range snap.Providers {
		shareSum += *p.SharePct
	}
	if shareSum != 100 {
		t.Fatalf("作者份额合计 = %v，want 100", shareSum)
	}
	if len(snap.Providers) != 2 || snap.Providers[0].Author != authorB {
		t.Fatalf("作者行 = %+v", snap.Providers)
	}

	// timeseries：today 用小时桶，A/B 都在 02:00Z 桶。
	if len(snap.Timeseries) != 2 {
		t.Fatalf("timeseries 桶数 = %d，want 2", len(snap.Timeseries))
	}
	for _, point := range snap.Timeseries {
		want := int64(1000)
		if point.Model == modelA {
			want = 750
		}
		if point.Granularity != string(GranularityHour) || point.Tokens != want {
			t.Fatalf("timeseries %s = %s/%d，want hour/%d", point.Model, point.Granularity, point.Tokens, want)
		}
		if !point.BucketStart.Equal(curAt) {
			t.Fatalf("桶起点 = %s，want %s", point.BucketStart, curAt)
		}
	}

	// 作者序列（B5a）：两个作者都在 Top5，按站点时区的小时桶写入。
	authorBucket := func(points []store.RankingsAuthorTimeseries, author string, bucket time.Time) int64 {
		for _, point := range points {
			if point.Author == author && point.BucketStart.Equal(bucket) {
				return point.Tokens
			}
		}
		return -1
	}
	if len(snap.AuthorSeries) != 2 {
		t.Fatalf("作者桶数 = %d，want 2：%+v", len(snap.AuthorSeries), snap.AuthorSeries)
	}
	if got := authorBucket(snap.AuthorSeries, authorA, curAt); got != 750 {
		t.Fatalf("作者 A 桶 = %d，want 750", got)
	}
	if got := authorBucket(snap.AuthorSeries, authorB, curAt); got != 1000 {
		t.Fatalf("作者 B 桶 = %d，want 1000", got)
	}

	slots := entitySlots(t, mainDB, prefix)
	// 第二次刷新（同一 now）：timeseries 覆盖而不是累加，槽位不变。
	refreshPeriod(ctx, deps, PeriodToday)
	snap2, _, err := repo.LatestRun(ctx, string(PeriodToday))
	if err != nil {
		t.Fatalf("LatestRun(2): %v", err)
	}
	var seriesA int64
	for _, point := range snap2.Timeseries {
		if point.Model == modelA {
			seriesA += point.Tokens
		}
	}
	if seriesA != 750 {
		t.Fatalf("重复刷新后 A 的桶 tokens = %d，want 750（幂等覆盖，不双计）", seriesA)
	}
	// 作者桶同样必须幂等覆盖：重算同一窗口不得累加。
	if len(snap2.AuthorSeries) != 2 {
		t.Fatalf("重复刷新后作者桶数 = %d，want 2（覆盖不新增）", len(snap2.AuthorSeries))
	}
	if got := authorBucket(snap2.AuthorSeries, authorA, curAt); got != 750 {
		t.Fatalf("重复刷新后作者 A 桶 = %d，want 750", got)
	}
	if got := entitySlots(t, mainDB, prefix); fmt.Sprint(got) != fmt.Sprint(slots) {
		t.Fatalf("重复刷新后槽位变化: %v -> %v", slots, got)
	}

	// 追加 A 的调用使排名反转：槽位必须保持（颜色跟随实体，不跟随排名）。
	insertUsageRows(t, logsDB, modelA, 50, 40, 10, 1, lateAt) // +2500 → A=3250
	refreshPeriod(ctx, deps, PeriodToday)
	snap3, _, err := repo.LatestRun(ctx, string(PeriodToday))
	if err != nil {
		t.Fatalf("LatestRun(3): %v", err)
	}
	if len(snap3.Models) != 2 || snap3.Models[0].Model != modelA || *snap3.Models[0].Rank != 1 {
		t.Fatalf("排名反转后顺序错误: %+v", snap3.Models)
	}
	if got := entitySlots(t, mainDB, prefix); fmt.Sprint(got) != fmt.Sprint(slots) {
		t.Fatalf("排名变化后槽位必须稳定: %v -> %v", slots, got)
	}
	// A 现在横跨两个小时桶，合计 3250。
	var total int64
	for _, point := range snap3.Timeseries {
		if point.Model == modelA {
			total += point.Tokens
		}
	}
	if total != 3250 {
		t.Fatalf("A 两桶合计 = %d，want 3250", total)
	}
	// 作者序列横跨两个小时桶，合计同样 3250。
	var authorTotal int64
	for _, point := range snap3.AuthorSeries {
		if point.Author == authorA {
			authorTotal += point.Tokens
		}
	}
	if authorTotal != 3250 {
		t.Fatalf("作者 A 两桶合计 = %d，want 3250", authorTotal)
	}

	// R-D6-04：year 周期必须能写 week 粒度（迁移放开了 CHECK）。同一批行在
	// year 窗口内落到「周一 00:00 +08」的周桶，且同桶覆盖不双计。
	refreshPeriod(ctx, deps, PeriodYear)
	yearSnap, ok, err := repo.LatestRun(ctx, string(PeriodYear))
	if err != nil || !ok {
		t.Fatalf("LatestRun(year): ok=%v err=%v", ok, err)
	}
	if len(yearSnap.Timeseries) == 0 {
		t.Fatal("year 周期应写入 week 桶")
	}
	weekStart := time.Date(2026, 10, 18, 16, 0, 0, 0, time.UTC) // 2026-10-19 00:00 +08
	for _, point := range yearSnap.Timeseries {
		if point.Granularity != string(GranularityWeek) {
			t.Fatalf("year 桶粒度 = %s，want week", point.Granularity)
		}
		if !point.BucketStart.Equal(weekStart) {
			t.Fatalf("year 周桶起点 = %s，want %s", point.BucketStart, weekStart)
		}
		// year 窗口覆盖 1 月 1 日至今，因此包含上一窗口（也是本周）的
		// 500 token：750+2500+500 = 3750。
		if point.Model == modelA && point.Tokens != 3750 {
			t.Fatalf("A 的周桶 tokens = %d，want 3750", point.Tokens)
		}
	}
	// 作者周桶：A = modelA 的 3750 + 同一作者 modelF 的 5000（modelF 在
	// year 窗口内达阈值，作者聚合要按「同作者多模型合并」计入）= 8750；
	// B = 2500（含上一窗口 1500）。
	if got := authorBucket(yearSnap.AuthorSeries, authorA, weekStart); got != 8750 {
		t.Fatalf("作者 A 周桶 = %d，want 8750", got)
	}
	if got := authorBucket(yearSnap.AuthorSeries, authorB, weekStart); got != 2500 {
		t.Fatalf("作者 B 周桶 = %d，want 2500", got)
	}
}

// entitySlots 返回 prefix 下所有实体的 kind/entity -> slot 映射。
func entitySlots(t *testing.T, db *sql.DB, prefix string) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT kind, entity, slot FROM rankings_entities WHERE entity LIKE $1`, prefix+"%")
	if err != nil {
		t.Fatalf("query entity slots: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind, entity string
		var slot int
		if err := rows.Scan(&kind, &entity, &slot); err != nil {
			t.Fatalf("scan entity slot: %v", err)
		}
		out[kind+"/"+entity] = slot
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("entity slots rows: %v", err)
	}
	return out
}

// TestAssignSlotsNoCollisionAcrossRounds 覆盖 H1/H2 的槽位语义。
//
// 两个 period（today/week）或两轮刷新会用完全不同的 top8 调用
// assignSlotsTx。旧的 `entity = ANY($2)` 读取方式看不到上一批占用的槽位，
// 第二批会把 1–8 再分配一遍，产生重复 (kind, slot)。修复后：
//   - 第二批能看到全部已注册行，不再产生重复；
//   - 8 槽耗尽后新实体 slot=NULL（不复用、不重排），第 9 个实体及其后都是 NULL。
//
// 用例在事务内先清空 kind=model 的注册行再回滚，保证起点确定，且不污染
// 其他并行测试（rollback 后什么都没写）。
func TestAssignSlotsNoCollisionAcrossRounds(t *testing.T) {
	db := openRankingsDB(t, "NEWTG_TEST_DB_DSN")
	ctx := context.Background()
	prefix := uniquePrefix(t)

	beginClean := func(t *testing.T) *sql.Tx {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin slots tx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		if _, err := tx.ExecContext(ctx, `DELETE FROM rankings_entities WHERE kind = 'model'`); err != nil {
			t.Fatalf("clear model entities: %v", err)
		}
		return tx
	}

	assertNoDuplicateSlots := func(t *testing.T, tx *sql.Tx) {
		t.Helper()
		var dup int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM (
			  SELECT slot FROM rankings_entities
			  WHERE kind = 'model' AND slot IS NOT NULL
			  GROUP BY slot HAVING count(*) > 1
			) d`).Scan(&dup); err != nil {
			t.Fatalf("count duplicate slots: %v", err)
		}
		if dup != 0 {
			t.Fatalf("出现 %d 个重复 (kind,slot)", dup)
		}
	}

	t.Run("two rounds with different top8 allocate disjoint slots", func(t *testing.T) {
		tx := beginClean(t)
		round1 := make([]string, 8)
		round2 := make([]string, 8)
		for i := 0; i < 8; i++ {
			round1[i] = fmt.Sprintf("%s-r1-%d", prefix, i)
			round2[i] = fmt.Sprintf("%s-r2-%d", prefix, i)
		}

		slots1, err := assignSlotsTx(ctx, tx, entityKindModel, round1)
		if err != nil {
			t.Fatalf("assign round1: %v", err)
		}
		seen := map[int16]struct{}{}
		for _, entity := range round1 {
			slot := slots1[entity]
			if slot == nil {
				t.Fatalf("round1 实体 %s 应拿到槽位", entity)
			}
			if _, dup := seen[*slot]; dup {
				t.Fatalf("round1 内部重复槽位 %d", *slot)
			}
			seen[*slot] = struct{}{}
		}
		if len(seen) != entitySlotCount {
			t.Fatalf("round1 使用槽位数 = %d，want %d", len(seen), entitySlotCount)
		}

		// 第二批（模拟另一个 period 的 top8）必须全部 NULL，且不得复用 1–8。
		slots2, err := assignSlotsTx(ctx, tx, entityKindModel, round2)
		if err != nil {
			t.Fatalf("assign round2: %v", err)
		}
		for _, entity := range round2 {
			if slot := slots2[entity]; slot != nil {
				t.Fatalf("槽位耗尽后实体 %s 拿到槽位 %d（禁止回绕复用）", entity, *slot)
			}
		}
		assertNoDuplicateSlots(t, tx)
	})

	t.Run("ninth entity is null and stays null", func(t *testing.T) {
		tx := beginClean(t)
		entities := make([]string, 9)
		for i := 0; i < 9; i++ {
			entities[i] = fmt.Sprintf("%s-nine-%d", prefix, i)
		}
		slots, err := assignSlotsTx(ctx, tx, entityKindModel, entities)
		if err != nil {
			t.Fatalf("assign nine: %v", err)
		}
		for i := 0; i < 8; i++ {
			if slots[entities[i]] == nil {
				t.Fatalf("第 %d 个实体应拿到槽位", i+1)
			}
		}
		if slot := slots[entities[8]]; slot != nil {
			t.Fatalf("第 9 个实体槽位 = %d，want NULL", *slot)
		}
		assertNoDuplicateSlots(t, tx)

		// 释放一个槽位后再传入同一实体：NULL 必须保持 NULL，不得补位
		// （槽位永不重排；否则颜色会在实体生命周期内变化）。
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM rankings_entities WHERE kind = 'model' AND entity = $1`, entities[0]); err != nil {
			t.Fatalf("free slot: %v", err)
		}
		again, err := assignSlotsTx(ctx, tx, entityKindModel, entities)
		if err != nil {
			t.Fatalf("assign nine again: %v", err)
		}
		if slot := again[entities[8]]; slot != nil {
			t.Fatalf("已登记 NULL 的实体在槽位释放后被重新分配 %d", *slot)
		}
		assertNoDuplicateSlots(t, tx)
	})
}

func TestRankingsRefreshEmptyStatus(t *testing.T) {
	mainDB := openRankingsDB(t, "NEWTG_TEST_DB_DSN")
	logsDB := openRankingsDB(t, "NEWTG_TEST_LOGS_DSN")
	rdb := openRankingsRedis(t)
	now, _ := testNowAt(t, 2027, 2, 15) // 独立时刻：不依赖其他用例是否已清理数据
	window := CurrentWindow(PeriodToday, now, now.Location())
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM rankings_snapshot_runs WHERE period = $1 AND window_start = $2`,
			string(PeriodToday), window.Start)
		_ = rdb.Del(context.Background(), LockKey(PeriodToday), CacheKey(PeriodToday)).Err()
	})

	// 本测试不插入任何 usage_logs（同包上一个用例已清理自己的行）。
	deps := rankingsDeps(mainDB, logsDB, rdb, testConfig(true, 50), now)
	refreshPeriod(context.Background(), deps, PeriodToday)

	repo := store.NewRankingsRepo(mainDB)
	snap, ok, err := repo.LatestRun(context.Background(), string(PeriodToday))
	if err != nil || !ok {
		t.Fatalf("LatestRun: ok=%v err=%v", ok, err)
	}
	if snap.Run.Status != statusEmpty || snap.Run.ModelCount != 0 {
		t.Fatalf("空窗口 status/model_count = %s/%d，want empty/0", snap.Run.Status, snap.Run.ModelCount)
	}
	if len(snap.Models) != 0 || len(snap.Providers) != 0 || len(snap.Timeseries) != 0 {
		t.Fatalf("空窗口不应有子表行: models=%d providers=%d series=%d",
			len(snap.Models), len(snap.Providers), len(snap.Timeseries))
	}
}

func TestRankingsRefreshSkipsWhenDisabledOrLocked(t *testing.T) {
	mainDB := openRankingsDB(t, "NEWTG_TEST_DB_DSN")
	logsDB := openRankingsDB(t, "NEWTG_TEST_LOGS_DSN")
	rdb := openRankingsRedis(t)
	now, _ := testNowAt(t, 2027, 3, 10) // 独立窗口：只断言本窗口的 run 数
	ctx := context.Background()
	window := CurrentWindow(PeriodToday, now, now.Location())
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM rankings_snapshot_runs WHERE period = $1 AND window_start = $2`,
			string(PeriodToday), window.Start)
		_ = rdb.Del(context.Background(), LockKey(PeriodToday)).Err()
	})

	before := countRuns(t, mainDB, PeriodToday, window.Start)

	// (1) nav.rankings.enabled=false：不抢锁、不写 run。
	disabled := rankingsDeps(mainDB, logsDB, rdb, testConfig(false, 50), now)
	refreshPeriod(ctx, disabled, PeriodToday)
	if got := countRuns(t, mainDB, PeriodToday, window.Start); got != before {
		t.Fatalf("开关关闭时不应写 run：before=%d after=%d", before, got)
	}
	if n, _ := rdb.Exists(ctx, LockKey(PeriodToday)).Result(); n != 0 {
		t.Fatal("开关关闭时不应获取租约")
	}

	// (2) 其他副本持有租约：跳过本轮，且不得删除别人的锁。
	if err := rdb.Set(ctx, LockKey(PeriodToday), "other-replica", time.Minute).Err(); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	enabled := rankingsDeps(mainDB, logsDB, rdb, testConfig(true, 50), now)
	refreshPeriod(ctx, enabled, PeriodToday)
	if got := countRuns(t, mainDB, PeriodToday, window.Start); got != before {
		t.Fatalf("租约被占用时不应写 run：before=%d after=%d", before, got)
	}
	if token, err := rdb.Get(ctx, LockKey(PeriodToday)).Result(); err != nil || token != "other-replica" {
		t.Fatalf("不得删除他人租约，token=%q err=%v", token, err)
	}
}

// TestCleanupExpiredTimeseriesRetention 覆盖 B5a 的桶保留清理：
// hour 30d / day 730d / week 1825d，模型桶与作者桶都清，且分批循环能把
// 超过一批（1000 行）的过期桶一次清理完。
func TestCleanupExpiredTimeseriesRetention(t *testing.T) {
	mainDB := openRankingsDB(t, "NEWTG_TEST_DB_DSN")
	ctx := context.Background()
	now, _ := testNow(t)
	prefix := uniquePrefix(t)

	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM rankings_timeseries WHERE model LIKE $1`, prefix+"%")
		_, _ = mainDB.Exec(`DELETE FROM rankings_author_timeseries WHERE author LIKE $1`, prefix+"%")
	})

	type bucket struct {
		granularity string
		at          time.Time
		expired     bool
	}
	cases := []bucket{
		{"hour", now.Add(-retentionHour - time.Hour), true},
		{"hour", now.Add(-time.Hour), false},
		{"day", now.Add(-retentionDay - 24*time.Hour), true},
		{"day", now.Add(-24 * time.Hour), false},
		{"week", now.Add(-retentionWeek - 7*24*time.Hour), true},
		{"week", now.Add(-7 * 24 * time.Hour), false},
	}
	model := prefix + "-model"
	author := prefix + "-author"
	for i, c := range cases {
		if _, err := mainDB.Exec(`INSERT INTO rankings_timeseries (model, granularity, bucket_start, tokens)
			VALUES ($1, $2, $3, $4)`, model, c.granularity, c.at, i+1); err != nil {
			t.Fatalf("insert model bucket: %v", err)
		}
		if _, err := mainDB.Exec(`INSERT INTO rankings_author_timeseries (author, granularity, bucket_start, tokens)
			VALUES ($1, $2, $3, $4)`, author, c.granularity, c.at, i+1); err != nil {
			t.Fatalf("insert author bucket: %v", err)
		}
	}

	// 一批以上的过期 hour 桶：验证 LIMIT 1000 的循环删除不是只删第一批。
	batchAuthor := prefix + "-batch"
	if _, err := mainDB.Exec(`
		INSERT INTO rankings_author_timeseries (author, granularity, bucket_start, tokens)
		SELECT $1, 'hour', $2::timestamptz + (i || ' seconds')::interval, 1
		FROM generate_series(0, $3) AS i`,
		batchAuthor, now.Add(-retentionHour-time.Hour), cleanupBatchSize+4); err != nil {
		t.Fatalf("insert batch buckets: %v", err)
	}

	deleted, err := CleanupExpiredTimeseries(ctx, mainDB, now)
	if err != nil {
		t.Fatalf("CleanupExpiredTimeseries: %v", err)
	}
	if deleted < int64(cleanupBatchSize+4+6) {
		t.Fatalf("删除行数 = %d，want ≥ %d", deleted, cleanupBatchSize+4+6)
	}

	for _, table := range []string{"rankings_timeseries", "rankings_author_timeseries"} {
		col, key := "model", model
		if table == "rankings_author_timeseries" {
			col, key = "author", author
		}
		for _, c := range cases {
			var n int
			if err := mainDB.QueryRow(fmt.Sprintf(
				`SELECT count(*) FROM %s WHERE %s = $1 AND granularity = $2 AND bucket_start = $3`,
				table, col), key, c.granularity, c.at).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			want := 1
			if c.expired {
				want = 0
			}
			if n != want {
				t.Fatalf("%s %s bucket %s 剩余 %d，want %d", table, c.granularity, c.at, n, want)
			}
		}
	}
	var batchLeft int
	if err := mainDB.QueryRow(`SELECT count(*) FROM rankings_author_timeseries WHERE author = $1`,
		batchAuthor).Scan(&batchLeft); err != nil {
		t.Fatalf("count batch: %v", err)
	}
	if batchLeft != 0 {
		t.Fatalf("过期批次剩余 %d 行，want 0（必须循环删除到不足一批）", batchLeft)
	}

	// 重复执行幂等：本用例的数据已清空，不会再有本前缀的行被删。
	if _, err := CleanupExpiredTimeseries(ctx, mainDB, now); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	var remaining int
	if err := mainDB.QueryRow(`SELECT count(*) FROM rankings_author_timeseries WHERE author LIKE $1`,
		prefix+"%").Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 3 { // 三个未过期桶
		t.Fatalf("未过期桶剩余 %d，want 3", remaining)
	}
}

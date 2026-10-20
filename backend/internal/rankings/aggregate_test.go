package rankings

import (
	"fmt"
	"testing"

	"newtg/gateway/internal/store"
)

func intPtr(v int) *int         { return &v }
func i64Ptr(v int64) *int64     { return &v }
func f64Ptr(v float64) *float64 { return &v }
func strPtr(v string) *string   { return &v }

func modelStat(model string, tokens, requests int64) store.RankingsModelStat {
	return store.RankingsModelStat{Model: model, TotalTokens: tokens, Requests: requests}
}

func TestBuildModelRowsTiesAndSkip(t *testing.T) {
	current := []modelTotal{
		{Model: "alpha", Tokens: 100, Requests: 10},
		{Model: "beta", Tokens: 100, Requests: 10},
		{Model: "gamma", Tokens: 50, Requests: 10},
		{Model: "delta", Tokens: 10, Requests: 3}, // 低于阈值
	}
	rows := buildModelRows(current, nil, 5)
	if len(rows) != 3 {
		t.Fatalf("入榜模型数 = %d，want 3（delta 低于阈值）", len(rows))
	}
	// token 相同 → 并列第 1；下一名跳号到第 3（不是 2）。
	wantRanks := []int{1, 1, 3}
	wantModels := []string{"alpha", "beta", "gamma"}
	for i, row := range rows {
		if row.Model != wantModels[i] {
			t.Fatalf("第 %d 行模型 = %s，want %s（按 token desc、模型字典序）", i, row.Model, wantModels[i])
		}
		if row.Rank == nil || *row.Rank != wantRanks[i] {
			t.Fatalf("%s 名次 = %v，want %d", row.Model, row.Rank, wantRanks[i])
		}
		if row.PrevRank != nil || row.DeltaPct != nil {
			t.Fatalf("%s 无上一窗口时不应有 prev_rank/delta_pct", row.Model)
		}
		if row.DeltaTokens == nil || *row.DeltaTokens != row.TotalTokens {
			t.Fatalf("%s 无上一窗口时 delta_tokens 应等于当前值", row.Model)
		}
	}
}

func TestBuildModelRowsPreviousWindowAndThreshold(t *testing.T) {
	current := []modelTotal{
		{Model: "alpha", Tokens: 120, Requests: 10},
		{Model: "beta", Tokens: 100, Requests: 10},
		{Model: "newcomer", Tokens: 500, Requests: 10},
	}
	previous := []modelTotal{
		{Model: "alpha", Tokens: 100, Requests: 10},
		{Model: "beta", Tokens: 200, Requests: 10},
		{Model: "newcomer", Tokens: 500, Requests: 3}, // 上窗口样本不足
	}
	rows := buildModelRows(current, previous, 5)
	byModel := map[string]store.RankingsModelStat{}
	for _, row := range rows {
		byModel[row.Model] = row
	}

	alpha := byModel["alpha"]
	if alpha.PrevRank == nil || *alpha.PrevRank != 2 {
		t.Fatalf("alpha 上窗口名次 = %v，want 2", alpha.PrevRank)
	}
	if alpha.DeltaTokens == nil || *alpha.DeltaTokens != 20 {
		t.Fatalf("alpha delta_tokens = %v，want 20", alpha.DeltaTokens)
	}
	if alpha.DeltaPct == nil || *alpha.DeltaPct != 20 {
		t.Fatalf("alpha delta_pct = %v，want 20", alpha.DeltaPct)
	}

	beta := byModel["beta"]
	if beta.PrevRank == nil || *beta.PrevRank != 1 {
		t.Fatalf("beta 上窗口名次 = %v，want 1", beta.PrevRank)
	}
	if beta.DeltaPct == nil || *beta.DeltaPct != -50 {
		t.Fatalf("beta delta_pct = %v，want -50", beta.DeltaPct)
	}

	// 上窗口样本不足的模型：prev_rank/delta_pct 为 NULL（不得当作 +∞ 上升）。
	newcomer := byModel["newcomer"]
	if newcomer.PrevRank != nil || newcomer.DeltaPct != nil {
		t.Fatalf("上一窗口样本不足时 prev_rank/delta_pct 应为 NULL，实际 %v/%v",
			newcomer.PrevRank, newcomer.DeltaPct)
	}
	if newcomer.Rank == nil || *newcomer.Rank != 1 {
		t.Fatalf("newcomer 当前名次 = %v，want 1", newcomer.Rank)
	}
}

func TestComputeMoversDoubleWindowAndLimit(t *testing.T) {
	models := []store.RankingsModelStat{
		// 有上一窗口：可参与升降榜。
		{Model: "up1", TotalTokens: 200, Rank: intPtr(1), PrevRank: intPtr(2), DeltaTokens: i64Ptr(60), DeltaPct: f64Ptr(42.86)},
		{Model: "up2", TotalTokens: 150, Rank: intPtr(2), PrevRank: intPtr(3), DeltaTokens: i64Ptr(50), DeltaPct: f64Ptr(50)},
		{Model: "down1", TotalTokens: 80, Rank: intPtr(3), PrevRank: intPtr(1), DeltaTokens: i64Ptr(-40), DeltaPct: f64Ptr(-33.33)},
		{Model: "flat", TotalTokens: 100, Rank: intPtr(4), PrevRank: intPtr(4), DeltaTokens: i64Ptr(0), DeltaPct: f64Ptr(0)},
		// 新模型：delta_pct 为 NULL，即使 token 巨大也不得进入升降榜。
		{Model: "newcomer", TotalTokens: 1000000, Rank: intPtr(0), DeltaTokens: i64Ptr(1000000)},
	}
	movers := ComputeMovers(models)
	if len(movers.Up) != 2 || movers.Up[0].Model != "up2" || movers.Up[1].Model != "up1" {
		t.Fatalf("上升榜 = %+v", movers.Up)
	}
	if len(movers.Down) != 1 || movers.Down[0].Model != "down1" {
		t.Fatalf("下降榜 = %+v", movers.Down)
	}
	if movers.Up[0].PrevTokens != 100 {
		t.Fatalf("up2 上窗口 token = %d，want 100", movers.Up[0].PrevTokens)
	}
	if movers.Up[0].Author != "Unknown" {
		t.Fatalf("作者缺失应回退 Unknown，实际 %q", movers.Up[0].Author)
	}

	// 每方向最多 5 项。
	many := make([]store.RankingsModelStat, 0, 12)
	for i := 0; i < 12; i++ {
		many = append(many, store.RankingsModelStat{
			Model:       string(rune('a' + i)),
			TotalTokens: int64(1000 - i*10),
			PrevRank:    intPtr(i + 1),
			DeltaTokens: i64Ptr(int64(i + 1)),
			DeltaPct:    f64Ptr(float64(i + 1)),
		})
	}
	limited := ComputeMovers(many)
	if len(limited.Up) != moverLimit {
		t.Fatalf("上升榜上限 = %d 项，want %d", len(limited.Up), moverLimit)
	}
}

func TestBuildProvidersSharesSumTo100(t *testing.T) {
	models := []store.RankingsModelStat{
		{Model: "m1", Author: strPtr("OpenAI"), TotalTokens: 1},
		{Model: "m2", Author: strPtr("OpenAI"), TotalTokens: 1},
		{Model: "m3", Author: strPtr("Anthropic"), TotalTokens: 1},
		{Model: "m4", Author: nil, TotalTokens: 1}, // 作者缺失归入 Unknown
	}
	providers := buildProviders(models)
	if len(providers) != 3 {
		t.Fatalf("作者数 = %d，want 3", len(providers))
	}
	var sum float64
	for _, p := range providers {
		sum += *p.SharePct
	}
	// 1/3 四舍五入会得到 33.33*3=99.99；末位作者吸收残差后必须恰为 100.00。
	if sum != 100 {
		t.Fatalf("份额合计 = %v，want 100.00", sum)
	}

	byAuthor := map[string]store.RankingsProviderStat{}
	for _, p := range providers {
		byAuthor[p.Author] = p
	}
	if byAuthor["OpenAI"].TotalTokens != 2 {
		t.Fatalf("OpenAI 合计 = %d，want 2", byAuthor["OpenAI"].TotalTokens)
	}
	if _, ok := byAuthor["Unknown"]; !ok {
		t.Fatal("缺失作者应归入 Unknown")
	}
	// OpenAI 合计 2（两份 1），Anthropic/Unknown 各 1 → 后两者并列第 2。
	wantRanks := map[string]int{"OpenAI": 1, "Anthropic": 2, "Unknown": 2}
	for _, p := range providers {
		if p.Rank == nil || *p.Rank != wantRanks[p.Author] {
			t.Fatalf("%s 名次 = %v，want %d", p.Author, *p.Rank, wantRanks[p.Author])
		}
	}
}

func TestBuildProvidersSingleAuthor(t *testing.T) {
	providers := buildProviders([]store.RankingsModelStat{
		{Model: "m1", Author: strPtr("OpenAI"), TotalTokens: 10},
		{Model: "m2", Author: strPtr("OpenAI"), TotalTokens: 30},
	})
	if len(providers) != 1 || *providers[0].SharePct != 100 {
		t.Fatalf("单作者应占 100%%，实际 %+v", providers)
	}
	if buildProviders(nil) != nil {
		t.Fatal("无模型时不应产生作者行")
	}
}

func TestRound2(t *testing.T) {
	if got := round2(1.236); got != 1.24 {
		t.Fatalf("round2(1.236) = %v", got)
	}
	if got := round2(-33.333); got != -33.33 {
		t.Fatalf("round2(-33.333) = %v", got)
	}
	if got := round2(20); got != 20 {
		t.Fatalf("round2(20) = %v", got)
	}
}

func TestClampDeltaPct(t *testing.T) { // numeric(7,2) 上限 99999.99：极端增长必须钳制而不是让快照事务溢出回滚。
	if got := clampDeltaPct(1e8); got != maxDeltaPct {
		t.Fatalf("钳制上限 = %v", got)
	}
	if got := clampDeltaPct(-1e8); got != -maxDeltaPct {
		t.Fatalf("钳制下限 = %v", got)
	}
	if got := clampDeltaPct(12.34); got != 12.34 {
		t.Fatalf("区间内不应改变: %v", got)
	}
}

// TestBuildProvidersResidualNeverNegative 覆盖末位吸收残差的负数边界。
//
// (1) 99.995% / 0.005%：两项各自四舍五入后合计 10001，末位吸收 -1 后必须
// 是 0.00 而不是负数；
// (2) 多个作者的舍入误差同向累积时，末位不足以吸收全部负残差，必须从最大
// 作者扣回，保证所有 share_pct ≥ 0 且合计恰为 100.00。
func TestBuildProvidersResidualNeverNegative(t *testing.T) {
	assertShares := func(t *testing.T, providers []store.RankingsProviderStat, want int) {
		t.Helper()
		if len(providers) != want {
			t.Fatalf("作者行数 = %d，want %d", len(providers), want)
		}
		var sum float64
		for _, p := range providers {
			if p.SharePct == nil || *p.SharePct < 0 {
				t.Fatalf("作者 %s 份额 = %v，不得为负", p.Author, p.SharePct)
			}
			sum += *p.SharePct
		}
		// 以「万分之一」比较，避免 0.01 量级的浮点尾差。
		if got := round2(sum); got != 100 {
			t.Fatalf("份额合计 = %v，want 100", got)
		}
	}

	t.Run("99.995/0.005", func(t *testing.T) {
		models := []store.RankingsModelStat{
			{Model: "a", Author: strPtr("author-a"), TotalTokens: 999_950},
			{Model: "b", Author: strPtr("author-b"), TotalTokens: 50},
		}
		providers := buildProviders(models)
		assertShares(t, providers, 2)
		if *providers[0].SharePct != 100 || *providers[1].SharePct != 0 {
			t.Fatalf("份额 = %v/%v，want 100.00/0.00", *providers[0].SharePct, *providers[1].SharePct)
		}
	})

	t.Run("negative residual moves to largest", func(t *testing.T) {
		// 精度刻意取到 0.0001%：7 个作者各 1249.575 bp、1 个 1252.575 bp、
		// 末位 0.4 bp。四舍五入后合计 10003，负残差 -3 超过末位的 0 bp。
		models := []store.RankingsModelStat{
			{Model: "big", Author: strPtr("author-big"), TotalTokens: 12_525_750},
			{Model: "tiny", Author: strPtr("author-tiny"), TotalTokens: 4_000},
		}
		for i := 0; i < 7; i++ {
			models = append(models, store.RankingsModelStat{
				Model:       fmt.Sprintf("mid-%d", i),
				Author:      strPtr(fmt.Sprintf("author-mid-%d", i)),
				TotalTokens: 12_495_750,
			})
		}
		providers := buildProviders(models)
		assertShares(t, providers, 9)
		// 末位吸收后为负数时被归零，差额从最大作者扣回。
		if got := *providers[len(providers)-1].SharePct; got != 0 {
			t.Fatalf("末位份额 = %v，want 0（不得为负）", got)
		}
	})
}

func TestSlotEntitiesCapsAtPaletteSize(t *testing.T) {
	rows := make([]store.RankingsModelStat, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, store.RankingsModelStat{Model: string(rune('a' + i))})
	}
	got := slotEntities(rows, func(m store.RankingsModelStat) string { return m.Model })
	if len(got) != entitySlotCount {
		t.Fatalf("注册槽位的实体数 = %d，want %d（超出归 Other）", len(got), entitySlotCount)
	}
	if got[0] != "a" || got[entitySlotCount-1] != "h" {
		t.Fatalf("截断应保持榜单顺序: %v", got)
	}
	if small := slotEntities(rows[:3], func(m store.RankingsModelStat) string { return m.Model }); len(small) != 3 {
		t.Fatalf("不足 8 个时不应补齐: %v", small)
	}
}

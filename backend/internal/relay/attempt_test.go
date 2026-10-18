package relay

import (
	"math/rand/v2"
	"reflect"
	"testing"

	"newtg/gateway/internal/contract"
)

// attemptCandidateIDs 提取候选渠道 ID，便于用「期望顺序」直接断言。
func attemptCandidateIDs(candidates []contract.ChannelCandidate) []int {
	out := make([]int, len(candidates))
	for i, c := range candidates {
		out[i] = c.ChannelID
	}
	return out
}

// TestAttemptPlanSortOrder 冻结 R-P3：优先级降序优先，权重降序次之，
// 完全相同键保持输入顺序（稳定排序）。
func TestAttemptPlanSortOrder(t *testing.T) {
	candidates := []contract.ChannelCandidate{
		{ChannelID: 1, Priority: 5, Weight: 90},
		{ChannelID: 2, Priority: 10, Weight: 10},
		{ChannelID: 3, Priority: 10, Weight: 30},
		{ChannelID: 4, Priority: 10, Weight: 10}, // 与 2 同键：稳定排序必须保留 2 在 4 前
		{ChannelID: 5, Priority: 1, Weight: 1000},
	}

	plan := BuildAttemptPlan(nil, candidates)

	want := []int{3, 2, 4, 1, 5}
	if got := attemptCandidateIDs(plan.Candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("候选顺序 = %v，期望 %v（R-P3：priority 降序、weight 降序、同键稳定）", got, want)
	}
}

// TestAttemptPlanDedupe 冻结去重语义：按排序后的顺序保留同一 ChannelID 的第一次出现，
// 即最高优先级/权重的候选胜出；完全重复的条目只留一个。
func TestAttemptPlanDedupe(t *testing.T) {
	candidates := []contract.ChannelCandidate{
		{ChannelID: 7, Priority: 1, Weight: 1},  // 低优先级重复，应被丢弃
		{ChannelID: 8, Priority: 5, Weight: 10}, // 与 9 同键，输入序在前
		{ChannelID: 7, Priority: 9, Weight: 20}, // 高优先级，保留
		{ChannelID: 9, Priority: 5, Weight: 10},
		{ChannelID: 8, Priority: 5, Weight: 10}, // 完全重复，丢弃
	}

	plan := BuildAttemptPlan(nil, candidates)

	// 排序后：7(p9) → 8(p5,w10) → 9(p5,w10) → 7(p1)；去重后剩 7, 8, 9。
	want := []int{7, 8, 9}
	if got := attemptCandidateIDs(plan.Candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("去重后候选 = %v，期望 %v", got, want)
	}
}

// TestAttemptPlanClampTable 冻结 GC-5 / C-9/D11 的重试钳制表：
// 默认 3，区间 [1,5]；MaxAttempts = 钳制后的重试次数 + 1（relay-01 §6.1）。
func TestAttemptPlanClampTable(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, 1},
		{0, 1},
		{1, 1},
		{3, 3},
		{5, 5},
		{6, 5},
		{100, 5},
	}
	for _, tc := range cases {
		if got := ClampRetries(tc.in); got != tc.want {
			t.Errorf("ClampRetries(%d) = %d，期望 %d", tc.in, got, tc.want)
		}
	}

	one := []contract.ChannelCandidate{{ChannelID: 1, Priority: 1}}

	if got := BuildAttemptPlan(nil, one).MaxAttempts; got != 4 {
		t.Errorf("BuildAttemptPlan 默认 MaxAttempts = %d，期望 4（默认重试 3 + 首试 1）", got)
	}
	if got := BuildAttemptPlanWithRetries(nil, one, 0).MaxAttempts; got != 2 {
		t.Errorf("BuildAttemptPlanWithRetries(0) MaxAttempts = %d，期望 2（钳制 1 + 首试 1）", got)
	}
	if got := BuildAttemptPlanWithRetries(nil, one, 3).MaxAttempts; got != 4 {
		t.Errorf("BuildAttemptPlanWithRetries(3) MaxAttempts = %d，期望 4", got)
	}
	if got := BuildAttemptPlanWithRetries(nil, one, 6).MaxAttempts; got != 6 {
		t.Errorf("BuildAttemptPlanWithRetries(6) MaxAttempts = %d，期望 6（钳制 5 + 首试 1）", got)
	}
}

// TestAttemptPlanNextBounds 冻结 Next 的边界：nil 计划、空计划、负索引、越界一律
// 返回 ok=false；合法索引按排序后的候选返回。索引钳制（relay-01 §6.4）由路由层负责。
func TestAttemptPlanNextBounds(t *testing.T) {
	if _, ok := Next(nil, 0); ok {
		t.Error("Next(nil, 0) = ok=true，期望 false")
	}

	empty := &contract.AttemptPlan{}
	if _, ok := Next(empty, 0); ok {
		t.Error("Next(空计划, 0) = ok=true，期望 false")
	}

	plan := BuildAttemptPlan(nil, []contract.ChannelCandidate{
		{ChannelID: 11, Priority: 1},
		{ChannelID: 22, Priority: 2},
	})

	if got, ok := Next(plan, 0); !ok || got.ChannelID != 22 {
		t.Errorf("Next(plan, 0) = (%d, %v)，期望 (22, true)", got.ChannelID, ok)
	}
	if got, ok := Next(plan, 1); !ok || got.ChannelID != 11 {
		t.Errorf("Next(plan, 1) = (%d, %v)，期望 (11, true)", got.ChannelID, ok)
	}
	if got, ok := Next(plan, 2); ok || got.ChannelID != 0 {
		t.Errorf("Next(plan, 2) = (%d, %v)，期望零值 + false", got.ChannelID, ok)
	}
	if _, ok := Next(plan, -1); ok {
		t.Error("Next(plan, -1) = ok=true，期望 false")
	}
}

// TestAttemptPlanGroupFilter 冻结分组过滤：rc.UsingGroup 非空时只保留
// Group == ""（组无关）或 == UsingGroup 的候选；异组候选即使优先级更高也剔除。
func TestAttemptPlanGroupFilter(t *testing.T) {
	candidates := []contract.ChannelCandidate{
		{ChannelID: 1, Group: "vip", Priority: 1},
		{ChannelID: 2, Group: "default", Priority: 99}, // 异组，必须被过滤
		{ChannelID: 3, Group: "", Priority: 1},
	}
	rc := &contract.RelayContext{UsingGroup: "vip"}

	plan := BuildAttemptPlan(rc, candidates)

	want := []int{1, 3}
	if got := attemptCandidateIDs(plan.Candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("分组过滤后候选 = %v，期望 %v", got, want)
	}
	if plan.CrossGroup {
		t.Error("指定 UsingGroup 且异组候选已过滤时 CrossGroup = true，期望 false")
	}
}

// TestAttemptPlanCrossGroup 冻结控制器的 CrossGroup 判据：
// 仅有「非空 Group 且不同于 rc.UsingGroup」的候选存活时才置 true。
// 指定 UsingGroup 时分组过滤已剔除异组候选 → 恒 false（M2 默认；跨组轮换属 M3 auto 分组）。
func TestAttemptPlanCrossGroup(t *testing.T) {
	rc := &contract.RelayContext{UsingGroup: "vip"}
	plan := BuildAttemptPlan(rc, []contract.ChannelCandidate{
		{ChannelID: 1, Group: "vip"},
		{ChannelID: 2, Group: ""},
		{ChannelID: 3, Group: "other"},
	})
	if plan.CrossGroup {
		t.Error("指定 UsingGroup 时 CrossGroup = true，期望 false")
	}

	// 未指定分组：候选可跨组，计划应标记为跨组；空组（组无关）不算跨组。
	plan = BuildAttemptPlan(&contract.RelayContext{}, []contract.ChannelCandidate{
		{ChannelID: 4, Group: "a"},
		{ChannelID: 5, Group: ""},
	})
	if !plan.CrossGroup {
		t.Error("未指定 UsingGroup 且候选含非空分组时 CrossGroup = false，期望 true")
	}

	plan = BuildAttemptPlan(nil, []contract.ChannelCandidate{
		{ChannelID: 6, Group: ""},
		{ChannelID: 7, Group: ""},
	})
	if plan.CrossGroup {
		t.Error("候选全为组无关（Group 空）时 CrossGroup = true，期望 false")
	}
}

// TestAttemptPlanInputNotMutated 冻结「不原地修改输入」：候选切片可能来自路由索引缓存
// （relay-01 §5.1 进程内缓存），原地排序会污染其他请求看到的顺序，必须先复制。
func TestAttemptPlanInputNotMutated(t *testing.T) {
	candidates := []contract.ChannelCandidate{
		{ChannelID: 3, Group: "default", Priority: 1, Weight: 5},
		{ChannelID: 1, Group: "vip", Priority: 9, Weight: 10},
		{ChannelID: 1, Group: "vip", Priority: 9, Weight: 10},
		{ChannelID: 2, Group: "vip", Priority: 2, Weight: 1},
	}
	snapshot := append([]contract.ChannelCandidate(nil), candidates...)
	rc := &contract.RelayContext{UsingGroup: "vip"}

	plan := BuildAttemptPlan(rc, candidates)

	if !reflect.DeepEqual(candidates, snapshot) {
		t.Fatalf("BuildAttemptPlan 修改了输入切片：\n got  = %+v\n want = %+v", candidates, snapshot)
	}

	// 计划必须拥有独立底层数组：改动计划不得回写到输入。
	if len(plan.Candidates) == 0 {
		t.Fatal("计划候选为空，无法验证独立底层数组")
	}
	plan.Candidates[0].Weight = 7777
	for _, c := range candidates {
		if c.Weight == 7777 {
			t.Fatal("修改计划候选回写到了输入切片（共享底层数组）")
		}
	}
}

// TestAttemptPlanEmptyAndNilContext 覆盖空输入与 nil 上下文：
// 空输入得到空计划（Next 立即 false），nil rc 不 panic 且不做分组过滤。
func TestAttemptPlanEmptyAndNilContext(t *testing.T) {
	plan := BuildAttemptPlan(nil, nil)
	if plan == nil {
		t.Fatal("BuildAttemptPlan(nil, nil) = nil，期望非 nil 计划")
	}
	if len(plan.Candidates) != 0 {
		t.Fatalf("空输入候选数 = %d，期望 0", len(plan.Candidates))
	}
	if plan.MaxAttempts != 4 {
		t.Errorf("空输入 MaxAttempts = %d，期望默认 4", plan.MaxAttempts)
	}
	if _, ok := Next(plan, 0); ok {
		t.Error("空计划 Next 返回 ok=true，期望 false")
	}

	// nil rc 不做分组过滤：异组候选仍然保留（仅内部/测试直调用场景）。
	plan = BuildAttemptPlan(nil, []contract.ChannelCandidate{{ChannelID: 1, Group: "default"}})
	if len(plan.Candidates) != 1 {
		t.Fatalf("nil rc 时候选被过滤：%v", attemptCandidateIDs(plan.Candidates))
	}

	rc := &contract.RelayContext{UsingGroup: "vip"}
	plan = BuildAttemptPlan(rc, []contract.ChannelCandidate{{ChannelID: 1, Group: "default"}})
	if len(plan.Candidates) != 0 {
		t.Fatalf("异组唯一候选仍存活：%v", attemptCandidateIDs(plan.Candidates))
	}
}

// ============================================================================
// 同优先级加权随机（relay-01 §5.2–§5.3；final-review finding 1）
// ============================================================================

// TestSelectWithinPrioritySingleCandidate 冻结单候选短路：直接返回该候选，
// 不消耗随机源（加权随机没有可选项）。
func TestSelectWithinPrioritySingleCandidate(t *testing.T) {
	called := false
	got := SelectWithinPriority([]contract.ChannelCandidate{{ChannelID: 7, Weight: 0}}, func(uint64) uint64 {
		called = true
		return 0
	})
	if got.ChannelID != 7 {
		t.Fatalf("单候选 = %d，期望 7", got.ChannelID)
	}
	if called {
		t.Fatal("单候选不得调用随机源")
	}
	if got := SelectWithinPriority(nil, rand.Uint64N); got.ChannelID != 0 {
		t.Fatalf("空候选 = %+v，期望零值", got)
	}
}

// TestSelectWithinPriorityWeightedDistribution 覆盖 §5.3 的加权比例：
// weight=1/1/2 时三次采样分布应约为 1:1:2（TC-A-09，容差 5%）。
func TestSelectWithinPriorityWeightedDistribution(t *testing.T) {
	candidates := []contract.ChannelCandidate{
		{ChannelID: 1, Weight: 1},
		{ChannelID: 2, Weight: 1},
		{ChannelID: 3, Weight: 2},
	}
	rng := rand.New(rand.NewPCG(0x5eed, 0x1234))
	counts := map[int]int{}
	const runs = 60000
	for i := 0; i < runs; i++ {
		counts[SelectWithinPriority(candidates, rng.Uint64N).ChannelID]++
	}
	want := map[int]float64{1: 0.25, 2: 0.25, 3: 0.5}
	for id, ratio := range want {
		got := float64(counts[id]) / runs
		if got < ratio-0.05 || got > ratio+0.05 {
			t.Fatalf("渠道 %d 采样比例 = %.3f，期望 %.2f±0.05（counts=%v）", id, got, ratio, counts)
		}
	}
}

// TestSelectWithinPriorityZeroWeightsUniform 覆盖 §5.3 的全零权重边界：
// 退化为均匀分布（甲类修正，避免 rand.Uint64N(0) panic）。
func TestSelectWithinPriorityZeroWeightsUniform(t *testing.T) {
	candidates := []contract.ChannelCandidate{
		{ChannelID: 1},
		{ChannelID: 2},
		{ChannelID: 3},
	}
	rng := rand.New(rand.NewPCG(0xfeed, 0x4321))
	counts := map[int]int{}
	const runs = 60000
	for i := 0; i < runs; i++ {
		counts[SelectWithinPriority(candidates, rng.Uint64N).ChannelID]++
	}
	for _, id := range []int{1, 2, 3} {
		got := float64(counts[id]) / runs
		if got < 0.28 || got > 0.39 {
			t.Fatalf("全零权重下渠道 %d 采样比例 = %.3f，期望约 1/3（counts=%v）", id, got, counts)
		}
	}
}

// TestAttemptPrioritiesClamp 冻结 §6.4 的优先级映射：去重后的优先级序列保持
// 首次出现顺序；重试索引 i 映射到 priorities[min(i, len-1)]，越界钳到
// **最低优先级**（不是最后一个候选）。final-review finding 11 的注释修正
// 与这里是同一行为。
func TestAttemptPrioritiesClamp(t *testing.T) {
	ordered := []contract.ChannelCandidate{
		{ChannelID: 1, Priority: 10},
		{ChannelID: 2, Priority: 10},
		{ChannelID: 3, Priority: 5},
		{ChannelID: 4, Priority: 1},
	}
	priorities := attemptPriorities(ordered)
	if !reflect.DeepEqual(priorities, []int{10, 5, 1}) {
		t.Fatalf("priorities = %v，期望 [10 5 1]", priorities)
	}
	wantTargets := []int{10, 5, 1, 1, 1, 1}
	for i, want := range wantTargets {
		target := priorities[min(i, len(priorities)-1)]
		if target != want {
			t.Fatalf("attemptIndex %d → target = %d，期望 %d", i, target, want)
		}
	}
	if got := candidatesAtPriority(ordered, 10); len(got) != 2 || got[0].ChannelID != 1 || got[1].ChannelID != 2 {
		t.Fatalf("candidatesAtPriority(10) = %+v，期望 [1 2]", got)
	}
}

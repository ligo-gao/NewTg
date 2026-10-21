package mj

import (
	"testing"

	"newtg/gateway/internal/tasks"
)

func channelFixture(id int64, typ string, status, priority int, weight uint, models ...string) ChannelCandidate {
	return ChannelCandidate{
		ID: id, Type: typ, Status: status, Priority: priority, Weight: weight,
		SupportedModels: models,
	}
}

// TestEligibleChannels 钉死渠道准入：启用、类型、模型（原生或映射）。
func TestEligibleChannels(t *testing.T) {
	candidates := []ChannelCandidate{
		channelFixture(1, ChannelTypeMidjourney, ChannelStatusEnabled, 0, 1, "mj_imagine"),
		channelFixture(2, ChannelTypeMidjourneyPlus, ChannelStatusEnabled, 0, 1, "mj_imagine"),
		channelFixture(3, "openai", ChannelStatusEnabled, 0, 1, "mj_imagine"), // 误配也必须排除
		channelFixture(4, ChannelTypeMidjourney, 2, 0, 1, "mj_imagine"),       // 人工禁用
		channelFixture(5, ChannelTypeMidjourney, 3, 0, 1, "mj_imagine"),       // 自动禁用
		channelFixture(6, ChannelTypeMidjourney, ChannelStatusEnabled, 0, 1, "mj_upscale"),
		{ID: 7, Type: ChannelTypeMidjourney, Status: ChannelStatusEnabled, Priority: 0, Weight: 1,
			ModelMapping: map[string]string{"mj_imagine": "upstream-imagine"}},
	}

	got := EligibleChannels(candidates, "mj_imagine", false)
	wantIDs := []int64{1, 2, 7}
	if len(got) != len(wantIDs) {
		t.Fatalf("普通请求候选 = %v，期望 %v", ids(got), wantIDs)
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Fatalf("普通请求候选 = %v，期望 %v", ids(got), wantIDs)
		}
	}

	got = EligibleChannels(candidates, "mj_imagine", true)
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("Plus-only 候选 = %v，期望仅渠道 2", ids(got))
	}

	if got := EligibleChannels(candidates, "", false); len(got) != 0 {
		t.Fatalf("空模型不得匹配任何渠道，got %v", ids(got))
	}
}

func ids(candidates []ChannelCandidate) []int64 {
	out := make([]int64, len(candidates))
	for i, candidate := range candidates {
		out[i] = candidate.ID
	}
	return out
}

// TestSelectChannelPriorityAndWeight 钉死「最高优先级 + 同优先级加权」。
func TestSelectChannelPriorityAndWeight(t *testing.T) {
	candidates := []ChannelCandidate{
		channelFixture(1, ChannelTypeMidjourney, ChannelStatusEnabled, 1, 100, "mj_imagine"),
		channelFixture(2, ChannelTypeMidjourney, ChannelStatusEnabled, 5, 10, "mj_imagine"),
		channelFixture(3, ChannelTypeMidjourney, ChannelStatusEnabled, 5, 30, "mj_imagine"),
		channelFixture(4, ChannelTypeMidjourneyPlus, ChannelStatusEnabled, 9, 1, "mj_imagine"),
	}

	// 普通请求：最高优先级是 9（渠道 4），单候选不消耗随机源。
	called := false
	got, ok := SelectChannel(candidates, "mj_imagine", false, func(uint64) uint64 {
		called = true
		return 0
	})
	if !ok || got.ID != 4 || called {
		t.Fatalf("普通请求选中 %v（RNG called=%v），期望渠道 4 且不采样", got.ID, called)
	}

	// Plus-only：只有渠道 4 合法。
	got, ok = SelectChannel(candidates, "mj_imagine", true, func(uint64) uint64 { return 0 })
	if !ok || got.ID != 4 {
		t.Fatalf("Plus-only 选中 %v，期望 4", got.ID)
	}

	// 去掉渠道 4 后，最高优先级是 5：权重 10/30，r∈[0,40)。
	higherOnly := candidates[:3]
	for _, c := range []struct {
		r    uint64
		want int64
	}{
		{0, 2}, {9, 2}, {10, 3}, {39, 3},
	} {
		got, ok := SelectChannel(higherOnly, "mj_imagine", false, func(uint64) uint64 { return c.r })
		if !ok || got.ID != c.want {
			t.Fatalf("r=%d 选中 %v，期望 %d", c.r, got.ID, c.want)
		}
	}

	// 全零权重退化为均匀分布，而不是除零/panic。
	zero := []ChannelCandidate{
		channelFixture(10, ChannelTypeMidjourney, ChannelStatusEnabled, 0, 0, "mj_imagine"),
		channelFixture(11, ChannelTypeMidjourney, ChannelStatusEnabled, 0, 0, "mj_imagine"),
	}
	for r, want := range map[uint64]int64{0: 10, 1: 11} {
		got, ok := SelectChannel(zero, "mj_imagine", false, func(n uint64) uint64 {
			if n != 2 {
				t.Fatalf("均匀分布应传入候选数 2，got %d", n)
			}
			return r
		})
		if !ok || got.ID != want {
			t.Fatalf("全零权重 r=%d 选中 %v，期望 %d", r, got.ID, want)
		}
	}

	if _, ok := SelectChannel(nil, "mj_imagine", false, nil); ok {
		t.Fatalf("无候选必须返回 false")
	}
}

// TestResolveLock 钉死动作类任务的原渠道三校验（relay-05 §4.1）。
func TestResolveLock(t *testing.T) {
	enabledSuccess := OriginTask{Exists: true, ChannelID: 9, ChannelEnabled: true, State: tasks.StateSuccess}
	if id, err := ResolveLock(enabledSuccess, LockPolicy{RequireSuccess: true}); err != nil || id != 9 {
		t.Fatalf("合法原任务 = (%d,%v)", id, err)
	}
	// 开关关闭时不校验状态。
	if id, err := ResolveLock(
		OriginTask{Exists: true, ChannelID: 9, ChannelEnabled: true, State: tasks.StateInProgress},
		LockPolicy{},
	); err != nil || id != 9 {
		t.Fatalf("开关关闭时非 SUCCESS 也应放行: (%d,%v)", id, err)
	}

	cases := []struct {
		name    string
		origin  OriginTask
		policy  LockPolicy
		wantErr string
	}{
		{"missing", OriginTask{}, LockPolicy{}, DescriptionOriginTaskMissing},
		{"zero channel", OriginTask{Exists: true, ChannelID: 0}, LockPolicy{}, DescriptionOriginTaskMissing},
		{"disabled", OriginTask{Exists: true, ChannelID: 9, ChannelEnabled: false}, LockPolicy{}, DescriptionChannelDisabled},
		{
			"not success when required",
			OriginTask{Exists: true, ChannelID: 9, ChannelEnabled: true, State: tasks.StateInProgress},
			LockPolicy{RequireSuccess: true}, DescriptionTaskNotSuccess,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			id, err := ResolveLock(c.origin, c.policy)
			if err == nil || err.Description != c.wantErr || id != 0 {
				t.Fatalf("ResolveLock = (%d,%#v)，期望错误 %s", id, err, c.wantErr)
			}
			if err.Code != CodeRequestError || err.HTTPStatus() != 400 {
				t.Fatalf("锁定失败必须是 400/code4，got %+v", err)
			}
		})
	}
}

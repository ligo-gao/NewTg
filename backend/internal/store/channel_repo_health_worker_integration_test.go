//go:build integration

package store

// W7/M4-b：健康 worker 三个新 store 方法的真实 PostgreSQL 验收。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m4?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/store/ -run 'TestChannelRepo(AutoEnable|ListForHealthCheck|UpdatePeriodic)' -v
//
// 为什么要连真 PG：
//   - AutoEnable 的 `WHERE status=3` 是「人工禁用永不自动恢复」的数据库级
//     守卫（R-M4-09/15），只有真实条件 UPDATE 才能证明；
//   - UpdatePeriodicTestResult 不推进 updated_at 是乐观锁契约（R-M4-08），
//     必须用随后的真实 PATCH 版本比对来证明「不会 409」。

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestChannelRepoAutoEnableConditions 覆盖自动恢复的三条守卫与清空语义。
func TestChannelRepoAutoEnableConditions(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()

	t.Run("auto-disabled-recovers", func(t *testing.T) {
		id := insertAutoDisableChannel(t, repo, "m4b-auto", 3, true, "channel_keyword")
		if _, err := db.Exec(`UPDATE channels SET fail_count = 7 WHERE id = $1`, id); err != nil {
			t.Fatalf("seed fail_count: %v", err)
		}
		before, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get before: %v", err)
		}

		channel, changed, err := repo.AutoEnable(ctx, id)
		if err != nil {
			t.Fatalf("AutoEnable: %v", err)
		}
		if !changed || channel == nil {
			t.Fatalf("changed=%v channel=%v, want true/non-nil", changed, channel)
		}
		if channel.Status != 1 {
			t.Fatalf("status = %d, want 1（enabled）", channel.Status)
		}
		if channel.AutoDisabledReason != nil {
			t.Fatalf("reason = %v, want NULL（R-M4-16）", channel.AutoDisabledReason)
		}
		if channel.FailCount != 0 {
			t.Fatalf("fail_count = %d, want 0（R-M4-16）", channel.FailCount)
		}
		// 状态迁移必须推进乐观锁版本：管理员旧快照要失效。
		if !channel.UpdatedAt.After(before.UpdatedAt) {
			t.Fatalf("updated_at 未推进: before=%v after=%v", before.UpdatedAt, channel.UpdatedAt)
		}
		// 二次调用 no-op（并发/重复恢复的收敛路径）。
		again, changedAgain, err := repo.AutoEnable(ctx, id)
		if err != nil || changedAgain || again != nil {
			t.Fatalf("二次 AutoEnable = (%v,%v,%v), want (nil,false,nil)", again, changedAgain, err)
		}
	})

	t.Run("manual-disabled-never-recovers", func(t *testing.T) {
		id := insertAutoDisableChannel(t, repo, "m4b-manual", 2, true, "")
		channel, changed, err := repo.AutoEnable(ctx, id)
		if err != nil || changed || channel != nil {
			t.Fatalf("manual_disabled 被自动恢复: (%v,%v,%v)", channel, changed, err)
		}
		after, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if after.Status != 2 {
			t.Fatalf("status = %d, want 2", after.Status)
		}
	})

	t.Run("enabled-no-op", func(t *testing.T) {
		id := insertAutoDisableChannel(t, repo, "m4b-enabled", 1, true, "")
		channel, changed, err := repo.AutoEnable(ctx, id)
		if err != nil || changed || channel != nil {
			t.Fatalf("enabled 行被 AutoEnable 改动: (%v,%v,%v)", channel, changed, err)
		}
	})

	t.Run("missing-id-no-op", func(t *testing.T) {
		channel, changed, err := repo.AutoEnable(ctx, 1<<62)
		if err != nil || changed || channel != nil {
			t.Fatalf("不存在的 id = (%v,%v,%v), want (nil,false,nil)", channel, changed, err)
		}
	})
}

// TestChannelRepoListForHealthCheck 锁定扫描集：只有 1/3，manual_disabled(2)
// 不进入体检（变异：改成 IN (1,2,3) → 本用例红）。
func TestChannelRepoListForHealthCheck(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()

	enabled := insertAutoDisableChannel(t, repo, "m4b-scan-enabled", 1, true, "")
	autoDisabled := insertAutoDisableChannel(t, repo, "m4b-scan-auto", 3, true, "channel_auth_error")
	manual := insertAutoDisableChannel(t, repo, "m4b-scan-manual", 2, true, "")
	disabledOther := insertAutoDisableChannel(t, repo, "m4b-scan-manual2", 2, false, "")

	items, err := repo.ListForHealthCheck(ctx)
	if err != nil {
		t.Fatalf("ListForHealthCheck: %v", err)
	}
	want := map[int64]bool{enabled: true, autoDisabled: true}
	seen := make(map[int64]bool, len(items))
	var lastID int64
	for _, ch := range items {
		if ch.ID <= lastID {
			t.Fatalf("结果未按 id 升序: %d 出现在 %d 之后", ch.ID, lastID)
		}
		lastID = ch.ID
		seen[ch.ID] = true
		if ch.ID == manual || ch.ID == disabledOther {
			t.Fatalf("manual_disabled 渠道 %d 不得进入体检扫描集", ch.ID)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("扫描集缺少渠道 %d（got=%v）", id, seen)
		}
	}
}

// TestChannelRepoUpdatePeriodicTestResultKeepsVersion 是 R-M4-08 的权威断言：
// 周期体检写 last_test_* 不推进 updated_at，因此管理员携带旧版本的 PATCH
// 不会 409；作为对照，状态迁移（AutoDisable）必须推进版本。
func TestChannelRepoUpdatePeriodicTestResultKeepsVersion(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()

	id := insertAutoDisableChannel(t, repo, "m4b-periodic", 1, true, "")
	before, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}
	testedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.UpdatePeriodicTestResult(ctx, id, testedAt, true); err != nil {
		t.Fatalf("UpdatePeriodicTestResult: %v", err)
	}
	after, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("周期写推进了 updated_at: before=%v after=%v（R-M4-08）",
			before.UpdatedAt, after.UpdatedAt)
	}
	if after.LastTestAt == nil || !after.LastTestAt.UTC().Equal(testedAt) {
		t.Fatalf("last_test_at = %v, want %v", after.LastTestAt, testedAt)
	}
	if after.LastTestResult == nil || !*after.LastTestResult {
		t.Fatalf("last_test_result = %v, want true", after.LastTestResult)
	}

	// 管理员用「体检之前」的版本 PATCH 必须成功（不 409）。这同时证明
	// 周期写没有消耗乐观锁版本。
	remark := "edit after periodic health write"
	updated, err := repo.Update(ctx, id, ChannelPatch{RemarkSet: true, Remark: &remark}, before.UpdatedAt)
	if err != nil {
		t.Fatalf("并发 PATCH 失败（周期写不应推进版本）: %v", err)
	}
	if updated.Remark == nil || *updated.Remark != remark {
		t.Fatalf("PATCH 未生效: %v", updated.Remark)
	}

	// 对照：失败结果的周期写同样不推进版本；状态迁移才推进。
	if err := repo.UpdatePeriodicTestResult(ctx, id, testedAt.Add(time.Second), false); err != nil {
		t.Fatalf("UpdatePeriodicTestResult(false): %v", err)
	}
	if _, changed, err := repo.AutoDisable(ctx, id, "channel_auth_error"); err != nil || !changed {
		t.Fatalf("AutoDisable = (changed=%v, err=%v)", changed, err)
	}
	disabled, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get disabled: %v", err)
	}
	if !disabled.UpdatedAt.After(updated.UpdatedAt) {
		t.Fatalf("状态迁移未推进 updated_at: %v → %v", updated.UpdatedAt, disabled.UpdatedAt)
	}

	if err := repo.UpdatePeriodicTestResult(ctx, 1<<62, testedAt, true); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("不存在的 id 错误 = %v, want ErrChannelNotFound", err)
	}
}
